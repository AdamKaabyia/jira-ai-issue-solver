package services

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v75/github"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"jira-ai-issue-solver/models"
)

// ErrNoChanges is returned by CommitChanges when all workspace changes
// are bot artifacts and there is nothing to commit.
var ErrNoChanges = errors.New("no committable changes")

// ErrMergeConflict is returned by MergeBase when git merge produces
// conflicts. The working tree contains conflict markers for resolution.
var ErrMergeConflict = errors.New("merge conflict")

// fileExists returns true if the file exists, false if it does not exist,
// and an error if the existence check failed for reasons other than the file not existing.
func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}

	return false, err
}

const (
	// githubAPITimeout is the default timeout for GitHub API operations
	// Allows sufficient time for API calls while preventing indefinite hangs
	githubAPITimeout = 30 * time.Second

	// maxPaginationPages is the safety limit for API pagination
	// 100 pages * 100 items per page = 10,000 items max
	maxPaginationPages = 100

	// maxMergeCommitFiles is the safety limit for files in a single merge commit
	// Prevents DoS and OOM when processing commits with thousands of files
	maxMergeCommitFiles = 1000

	// maxInlineContentBytes is the per-file size threshold for inline
	// content in tree entries. Files larger than this fall back to
	// blob creation via the API to keep tree payloads bounded.
	maxInlineContentBytes = 1 << 20 // 1 MB

	// mergeabilityMaxRetries is the number of additional GET requests when
	// GitHub returns mergeable=null. The first request triggers the async
	// computation; retries wait for the result.
	mergeabilityMaxRetries = 3

	// mergeabilityRetryDelay is the pause between retry attempts, giving
	// GitHub time to finish the background merge-test computation.
	mergeabilityRetryDelay = 3 * time.Second
)

// GitHubServiceImpl is the concrete implementation for GitHub operations.
// There is no shared interface in this package — each consumer package
// declares a narrow interface for only the methods it needs, and
// GitHubServiceImpl satisfies them all implicitly.
type GitHubServiceImpl struct {
	config               *models.Config
	client               *http.Client
	appTransport         *ghinstallation.AppsTransport       // For app-level operations
	installationAuth     map[int64]*ghinstallation.Transport // Per-installation auth
	installationAuthMu   sync.RWMutex                        // Protects installationAuth map
	installationClients  map[int64]*github.Client            // Per-installation go-github clients
	installationClientMu sync.RWMutex                        // Protects installationClients map
	installationIDs      map[string]int64                    // Cache: "owner/repo" -> installation ID
	installationIDsMu    sync.RWMutex                        // Protects installationIDs map
	executor             models.CommandExecutor
	mergeRetryDelay      time.Duration
	logger               *zap.Logger
	gitOps               *GitOps
}

// gitCommand encapsulates a git command execution with optional stdout/stderr capture.
// Buffers are allocated only when enabled, optimizing memory usage while maintaining
// clean error reporting and debug logging capabilities.
type gitCommand struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// newGitCommand creates a new gitCommand that executes the given command in the specified directory.
// Stdout and stderr are captured only when their respective flags are enabled, minimizing memory allocation.
// The returned gitCommand provides safe access to command output even when capture is disabled.
func newGitCommand(cmd *exec.Cmd, directory string, captureStdout, captureStderr bool) *gitCommand {
	f := func(used bool) *bytes.Buffer {
		if used {
			return bytes.NewBuffer(nil)
		}
		return nil
	}

	cmd.Dir = directory

	gitCmd := &gitCommand{
		cmd:    cmd,
		stdout: f(captureStdout),
		stderr: f(captureStderr),
	}

	if gitCmd.stdout != nil {
		cmd.Stdout = gitCmd.stdout
	}
	if gitCmd.stderr != nil {
		cmd.Stderr = gitCmd.stderr
	}

	return gitCmd
}

// hasStdout returns true if stdout was captured and contains data.
// Returns false if stdout capture was disabled or if no output was produced.
func (g *gitCommand) hasStdout() bool {
	return g.stdout != nil && g.stdout.Len() > 0
}

// getStdout returns the captured stdout as a string.
// Returns an empty string if stdout capture was disabled or if the buffer is nil.
// Safe to call even when stdout was not enabled.
func (g *gitCommand) getStdout() string {
	if g.stdout == nil {
		return ""
	}
	return g.stdout.String()
}

// getStderr returns the captured stderr as a string.
// Returns an empty string if stderr capture was disabled or if the buffer is nil.
// Safe to call even when stderr was not enabled.
func (g *gitCommand) getStderr() string {
	if g.stderr == nil {
		return ""
	}
	return g.stderr.String()
}

// run executes the git command and returns any error that occurred.
// Stdout and stderr are captured to their respective buffers if enabled during construction.
func (g *gitCommand) run() error {
	return g.cmd.Run()
}

// NewGitHubService creates a new GitHubServiceImpl.
func NewGitHubService(config *models.Config, logger *zap.Logger, executor ...models.CommandExecutor) *GitHubServiceImpl {
	commandExecutor := exec.Command
	if len(executor) > 0 {
		commandExecutor = executor[0]
	}

	service := &GitHubServiceImpl{
		config:              config,
		client:              &http.Client{},
		installationAuth:    make(map[int64]*ghinstallation.Transport),
		installationClients: make(map[int64]*github.Client),
		installationIDs:     make(map[string]int64),
		executor:            commandExecutor,
		mergeRetryDelay:     mergeabilityRetryDelay,
		logger:              logger,
		gitOps:              NewGitOps(commandExecutor, logger),
	}

	// Initialize GitHub App transport
	appTransport, err := ghinstallation.NewAppsTransportKeyFromFile(
		http.DefaultTransport,
		config.GitHub.AppID,
		config.GitHub.PrivateKeyPath,
	)
	if err != nil {
		logger.Fatal("Failed to create GitHub App transport", zap.Error(err))
	}
	service.appTransport = appTransport

	logger.Info("Using GitHub App authentication",
		zap.Int64("appID", config.GitHub.AppID))

	return service
}

// getInstallationGitHubClient returns a go-github client authenticated for a specific installation
// Uses double-checked locking pattern for thread-safe lazy initialization of per-installation clients
// Note: Currently no error path exists (NewFromAppsTransport doesn't return error), but error return
// is kept for defensive programming in case future go-github versions change the API
func (s *GitHubServiceImpl) getInstallationGitHubClient(installationID int64) (*github.Client, error) {
	// Fast path: check if client already exists with read lock
	s.installationClientMu.RLock()
	client, exists := s.installationClients[installationID]
	s.installationClientMu.RUnlock()

	if exists {
		return client, nil
	}

	// Get or create the installation transport FIRST (outside client lock to avoid deadlock)
	// This ensures consistent lock ordering: always acquire installationAuthMu before installationClientMu
	s.installationAuthMu.RLock()
	transport, exists := s.installationAuth[installationID]
	s.installationAuthMu.RUnlock()

	if !exists {
		// Create new installation transport
		s.installationAuthMu.Lock()
		// Double-check pattern for transport as well
		if transport, exists = s.installationAuth[installationID]; !exists {
			tr := ghinstallation.NewFromAppsTransport(s.appTransport, installationID)
			s.installationAuth[installationID] = tr
			transport = tr
		}
		s.installationAuthMu.Unlock()
	}

	// Now acquire client lock and create client
	s.installationClientMu.Lock()
	defer s.installationClientMu.Unlock()

	// Double-check: another goroutine might have created the client while we waited
	if existingClient, found := s.installationClients[installationID]; found {
		return existingClient, nil
	}

	// Create and cache the go-github client
	client = github.NewClient(&http.Client{Transport: transport})
	s.installationClients[installationID] = client

	s.logger.Debug("Created new go-github client for installation",
		zap.Int64("installationID", installationID))

	return client, nil
}

// CloneRepository clones a repository to a local directory
func (s *GitHubServiceImpl) CloneRepository(repoURL, directory string) error {
	// Ensure the directory exists
	if err := os.MkdirAll(directory, 0750); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	debugEnabled := s.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "CloneRepository")

	// Check if the directory is already a git repository
	if _, err := os.Stat(filepath.Join(directory, ".git")); err == nil {
		// Directory is already a git repository, fetch the latest changes
		cmd := newGitCommand(s.executor("git", "fetch", "origin"), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to fetch repository: %w, stderr: %s", err, cmd.getStderr())
		}

		s.logger.Debug("git fetch origin", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

		// Reset to origin/main or origin/master to ensure we're up to date
		cmd = newGitCommand(s.executor("git", "reset", "--hard", "origin/main"), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			// Try with master branch
			cmd = newGitCommand(s.executor("git", "reset", "--hard", "origin/master"), directory, debugEnabled, true)

			if err := cmd.run(); err != nil {
				return fmt.Errorf("failed to reset to origin/main or origin/master: %w, stderr: %s", err, cmd.getStderr())
			}
			s.logger.Debug("git reset --hard", fn, zap.String("ref", "origin/master"), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))
		} else {
			s.logger.Debug("git reset --hard", fn, zap.String("ref", "origin/main"), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))
		}

		// Clean the repository
		cmd = newGitCommand(s.executor("git", "clean", "-fdx"), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to clean repository: %w, stderr: %s", err, cmd.getStderr())
		}

		s.logger.Debug("git clean -fdx", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	} else {
		// Clone the repository
		cmd := newGitCommand(s.executor("git", "clone", repoURL, directory), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to clone repository: %w, stderr: %s", err, cmd.getStderr())
		}

		s.logger.Debug("git clone", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))
	}

	// Configure git user for GitHub App
	cmd := newGitCommand(s.executor("git", "config", "user.name", s.config.GitHub.BotUsername), directory, debugEnabled, true)

	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git user name: %w, stderr: %s", err, cmd.getStderr())
	}
	s.logger.Debug("git config user.name", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(s.executor("git", "config", "user.email", s.config.GetBotEmail()), directory, debugEnabled, true)

	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git user email: %w, stderr: %s", err, cmd.getStderr())
	}

	s.logger.Debug("git config user.email", fn, zap.String("stderr", cmd.getStderr()))

	// Configure SSH signing if a key is specified
	exists := false
	if s.config.GitHub.SSHKeyPath != "" {
		var err error

		exists, err = fileExists(s.config.GitHub.SSHKeyPath)
		if err != nil {
			return fmt.Errorf("failed to check if SSH key file exists: %w", err)
		}

		s.logger.Debug("SSH key file exists", fn, zap.Bool("exists", exists))
	}

	if exists {
		cmd = newGitCommand(s.executor("git", "config", "gpg.format", "ssh"), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to configure git gpg format: %w, stderr: %s", err, cmd.getStderr())
		}
		s.logger.Debug("git config gpg.format", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

		cmd = newGitCommand(s.executor("git", "config", "user.signingkey", s.config.GitHub.SSHKeyPath), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to configure git ssh signing key: %w, stderr: %s", err, cmd.getStderr())
		}

		s.logger.Debug("git config user.signingkey", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

		cmd = newGitCommand(s.executor("git", "config", "commit.gpgsign", "true"), directory, debugEnabled, true)

		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to enable git commit signing: %w, stderr: %s", err, cmd.getStderr())
		}

		s.logger.Debug("git config commit.gpgsign", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

		s.logger.Debug("Configured SSH signing for repository", fn)
	} else {
		s.logger.Info("SSH signing not configured for repository")
	}

	// Configure git to use the GitHub token for authentication
	// This prevents credential prompts during push operations
	cmd = newGitCommand(s.executor("git", "config", "credential.helper", "store"), directory, debugEnabled, true)

	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git credential helper: %w, stderr: %s", err, cmd.getStderr())
	}

	s.logger.Debug("git config credential.helper", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	// Extract owner and repo from the URL first (needed for getting auth token)
	owner, repo, err := extractRepoInfo(repoURL)
	if err != nil {
		return fmt.Errorf("failed to extract repo info: %w", err)
	}

	// Set up the credential URL with token
	token, err := s.getAuthTokenForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("failed to get auth token: %w", err)
	}

	// Set the remote URL with embedded token
	authURL := fmt.Sprintf("https://%s@github.com/%s/%s.git", token, owner, repo)
	// Don't log stdout/stderr for this command since it contains the token
	cmd = newGitCommand(s.executor("git", "remote", "set-url", "origin", authURL), directory, false, false)

	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to set remote URL with token: %w", err)
	}

	return nil
}

// getAuthTokenForRepo gets the GitHub App installation token for a repository
func (s *GitHubServiceImpl) getAuthTokenForRepo(owner, repo string) (string, error) {
	if s.appTransport == nil {
		return "", fmt.Errorf("GitHub App authentication not configured")
	}

	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return "", fmt.Errorf("failed to get installation ID: %w", err)
	}

	// Get installation-specific transport with double-check locking pattern
	s.installationAuthMu.RLock()
	transport, ok := s.installationAuth[installationID]
	s.installationAuthMu.RUnlock()

	if !ok {
		s.installationAuthMu.Lock()
		// Double-check pattern: another goroutine may have created it
		if transport, ok = s.installationAuth[installationID]; !ok {
			transport = ghinstallation.NewFromAppsTransport(s.appTransport, installationID)
			s.installationAuth[installationID] = transport
		}
		s.installationAuthMu.Unlock()
	}

	// Get token from transport
	token, err := transport.Token(context.Background())
	if err != nil {
		// Invalidate cached transport on error to allow retry with fresh transport
		s.installationAuthMu.Lock()
		delete(s.installationAuth, installationID)
		s.installationAuthMu.Unlock()
		return "", fmt.Errorf("failed to get installation token (cache invalidated): %w", err)
	}

	return token, nil
}

// CreateBranch creates a new branch in a local repository based on the latest target branch
func (s *GitHubServiceImpl) CreateBranch(directory, branchName, baseBranch string) error {
	return s.gitOps.CreateBranch(directory, branchName, baseBranch)
}

// CommitChanges creates a verified commit via the GitHub API from local
// repository state. It handles both working tree changes and local commits
// (including merge commits with multiple parents). Returns an empty string
// if there are no changes; otherwise returns the commit SHA.
//
// If coAuthor is non-nil, a Co-authored-by trailer is appended to the
// commit message using the author's Name and Email.
func (s *GitHubServiceImpl) CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string, coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error) {
	// Extract co-author name/email (empty strings when nil).
	var coAuthorName, coAuthorEmail string
	if coAuthor != nil {
		coAuthorName = coAuthor.Name
		coAuthorEmail = coAuthor.Email
	}

	fn := zap.String("function", "CommitChanges")

	// Check what kind of changes we have
	hasChanges, err := s.HasChanges(dir, baseBranch)
	if err != nil {
		return "", fmt.Errorf("failed to check for changes: %w", err)
	}

	if !hasChanges {
		s.logger.Info("No changes to commit")
		return "", nil
	}

	// Normalize: ensure all changes are committed locally so that
	// createVerifiedCommitFromLocalHEAD (which uses git diff-tree
	// against HEAD) sees everything the AI produced — whether the
	// AI committed, staged, or left changes in the working tree.
	if err := s.stageAndCommitLocal(dir, fn); err != nil {
		return "", fmt.Errorf("failed to normalize local changes: %w", err)
	}

	excludes := mergeExcludes(importExcludes)
	noFileLimit := len(skipFileGuardrail) > 0 && skipFileGuardrail[0]
	return s.createVerifiedCommitFromLocalHEAD(upstreamOwner, owner, repo, branch, message, dir, baseBranch, coAuthorName, coAuthorEmail, excludes, noFileLimit)
}

// createVerifiedCommitFromLocalHEAD creates a verified commit via API from local HEAD commit.
// Preserves merge commit structure if HEAD is a merge commit.
//
// upstreamOwner is the GitHub owner of the upstream repository (e.g.,
// "flightctl"). In non-fork workflows it equals owner. In fork
// workflows it identifies the repo where the parent commit originated
// so the tree can be resolved there when the fork API cannot find it.
func (s *GitHubServiceImpl) createVerifiedCommitFromLocalHEAD(upstreamOwner, owner, repo, branchName, message, directory, baseBranch string, coAuthorName, coAuthorEmail string, excludes []string, noFileLimit bool) (string, error) {
	token, err := s.getAuthTokenForRepo(owner, repo)
	if err != nil {
		return "", fmt.Errorf("failed to get auth token: %w", err)
	}

	// Get parent commit SHAs from local HEAD (could be 1 or 2 for merge commits)
	parentSHAs, err := s.getLocalParentSHAs(directory)
	if err != nil {
		return "", fmt.Errorf("failed to get parent SHAs from local HEAD: %w", err)
	}

	s.logger.Debug("Creating verified commit from local HEAD",
		zap.Strings("parents", parentSHAs),
		zap.Int("parent_count", len(parentSHAs)))

	// Use first parent as base tree (the branch we're merging into)
	firstParent := parentSHAs[0]

	// Get the base tree from the first parent on GitHub.
	// The parent may not exist on the remote when the AI created
	// local commits (rebase, merge, or regular commits that only
	// exist locally). In that case, fall back to the remote branch
	// HEAD so we can still create a valid API commit.
	baseTreeSHA, err := s.getTreeSHAFromCommit(owner, repo, firstParent, token)
	if err != nil {
		// In fork workflows the parent commit originated from
		// upstream — try resolving the tree there before falling
		// back to the (potentially stale) fork branch HEAD.
		if upstreamOwner != owner {
			upstreamToken, tokenErr := s.getAuthTokenForRepo(upstreamOwner, repo)
			if tokenErr == nil {
				if treeSHA, treeErr := s.getTreeSHAFromCommit(upstreamOwner, repo, firstParent, upstreamToken); treeErr == nil {
					s.logger.Info("Resolved parent tree from upstream repo",
						zap.String("upstream", upstreamOwner+"/"+repo),
						zap.String("parent", firstParent))
					baseTreeSHA = treeSHA
					err = nil
				}
			}
		}
	}
	if err != nil {
		// The local parent doesn't exist on the remote — this is
		// normal when the AI created local commits during its session.
		// Prefer git merge-base against the local remote-tracking ref
		// (deterministic, immune to concurrent pushes) over querying
		// the live GitHub API for the current branch HEAD.
		//
		// origin/<branchName> exists in feedback flows (the bot's
		// branch was previously pushed and fetched) but not for new
		// tickets (branch only exists locally). Failure here is
		// expected for new branches and falls through to the API.
		resolved := false
		if mbSHA, mbErr := s.getMergeBase(directory, "origin/"+branchName); mbErr == nil {
			if treeSHA, treeErr := s.getTreeSHAFromCommit(owner, repo, mbSHA, token); treeErr == nil {
				s.logger.Debug("Resolved parent via local merge-base",
					zap.String("localParent", firstParent),
					zap.String("mergeBase", mbSHA))
				firstParent = mbSHA
				parentSHAs = []string{mbSHA}
				baseTreeSHA = treeSHA
				resolved = true
			}
		}
		if !resolved {
			remoteSHA, _, remoteErr := s.getBranchBaseCommit(owner, repo, branchName, baseBranch, token)
			if remoteErr != nil {
				return "", fmt.Errorf("failed to get branch HEAD for parent fallback: %w", remoteErr)
			}
			s.logger.Warn("Local parent not found on remote, falling back to remote branch HEAD",
				zap.String("localParent", firstParent),
				zap.String("remoteSHA", remoteSHA),
				zap.Error(err))
			firstParent = remoteSHA
			parentSHAs = []string{remoteSHA}
			baseTreeSHA, err = s.getTreeSHAFromCommit(owner, repo, firstParent, token)
			if err != nil {
				return "", fmt.Errorf("failed to get base tree from remote branch HEAD: %w", err)
			}
		}
	}

	// Create blobs for files that changed from the first parent
	// For merge commits, this includes files that differ from the first parent
	treeEntries, err := s.createBlobsForFilesChangedFromParent(owner, repo, directory, firstParent, token, excludes, noFileLimit)
	if err != nil {
		return "", fmt.Errorf("failed to create blobs: %w", err)
	}

	if len(treeEntries) == 0 {
		s.logger.Info("No changes from first parent; nothing to commit")
		return "", ErrNoChanges
	}

	// Create a new tree on GitHub
	treeSHA, err := s.createTree(owner, repo, baseTreeSHA, treeEntries, token)
	if err != nil {
		return "", fmt.Errorf("failed to create tree: %w", err)
	}

	s.logger.Debug("Created tree on GitHub",
		zap.String("tree", treeSHA),
		zap.String("baseTree", baseTreeSHA),
		zap.Int("entries", len(treeEntries)))

	// Build commit message with optional co-author
	commitMessage := message
	if coAuthorName != "" && coAuthorEmail != "" {
		commitMessage = fmt.Sprintf("%s\n\nCo-authored-by: %s <%s>", message, coAuthorName, coAuthorEmail)
	}

	// Create the commit with the tree and all parents from local HEAD
	commitSHA, err := s.createCommitWithParents(owner, repo, commitMessage, treeSHA, parentSHAs, token)
	if err != nil {
		return "", fmt.Errorf("failed to create commit: %w", err)
	}

	// Create or update the branch reference to point to the new commit.
	// The branch exists locally but may not have been pushed to the
	// remote yet, so we must check before choosing the operation.
	_, branchExists, err := s.getBranchBaseCommit(owner, repo, branchName, baseBranch, token)
	if err != nil {
		return "", fmt.Errorf("failed to check branch existence: %w", err)
	}

	if branchExists {
		if err := s.updateReference(owner, repo, branchName, commitSHA, token); err != nil {
			return "", fmt.Errorf("failed to update reference: %w", err)
		}
	} else {
		if err := s.createReference(owner, repo, branchName, commitSHA, token); err != nil {
			return "", fmt.Errorf("failed to create reference: %w", err)
		}
	}

	s.logger.Info("Successfully created verified commit from local HEAD",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.String("branch", branchName),
		zap.String("commit_sha", commitSHA),
		zap.Bool("isMergeCommit", len(parentSHAs) > 1))

	return commitSHA, nil
}

// getMergeBase returns the merge-base of the given ref and HEAD in the
// local repository. This uses the remote-tracking ref set by the last
// git fetch, so the result is deterministic and immune to concurrent
// pushes to the remote.
func (s *GitHubServiceImpl) getMergeBase(directory, ref string) (string, error) {
	cmd := newGitCommand(s.executor("git", "merge-base", ref, "HEAD"), directory, true, true)
	if err := cmd.run(); err != nil {
		return "", fmt.Errorf("git merge-base %s HEAD failed: %w, stderr: %s", ref, err, cmd.getStderr())
	}
	sha := strings.TrimSpace(cmd.getStdout())
	if sha == "" {
		return "", fmt.Errorf("git merge-base returned empty output")
	}
	return sha, nil
}

// getLocalParentSHAs gets the parent commit SHAs from local HEAD
// Returns 1 parent for normal commits, 2 parents for merge commits.
// Octopus merges (3+ parents) are not supported and will return an error.
func (s *GitHubServiceImpl) getLocalParentSHAs(directory string) ([]string, error) {
	// Get parent SHAs (one per line)
	cmd := newGitCommand(s.executor("git", "rev-parse", "HEAD^@"), directory, true, true)
	if err := cmd.run(); err != nil {
		return nil, fmt.Errorf("failed to get parent SHAs: %w, stderr: %s", err, cmd.getStderr())
	}

	output := strings.TrimSpace(cmd.getStdout())
	if output == "" {
		return nil, fmt.Errorf("no parent commits found")
	}

	// Split by newlines to get individual parent SHAs
	parents := []string{}
	for _, line := range strings.Split(output, "\n") {
		parent := strings.TrimSpace(line)
		if parent != "" {
			parents = append(parents, parent)
		}
	}

	// Validate parent count
	parentCount := len(parents)
	if parentCount == 0 {
		return nil, fmt.Errorf("failed to parse parent SHAs")
	}
	if parentCount > 2 {
		return nil, fmt.Errorf("octopus merges (3+ parents) are not supported - found %d parents", parentCount)
	}

	return parents, nil
}

// createCommitWithParents creates a commit via GitHub API with specific parents (supports merge commits)
func (s *GitHubServiceImpl) createCommitWithParents(owner, repo, message, treeSHA string, parentSHAs []string, token string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/commits", owner, repo)

	commitRequest := struct {
		Message string   `json:"message"`
		Tree    string   `json:"tree"`
		Parents []string `json:"parents"`
	}{
		Message: message,
		Tree:    treeSHA,
		Parents: parentSHAs,
	}

	jsonPayload, err := json.Marshal(commitRequest)
	if err != nil {
		return "", fmt.Errorf("failed to marshal commit request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to create commit: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "createCommitWithParents"))
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to create commit: %s, status: %d", string(body), resp.StatusCode)
	}

	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return "", fmt.Errorf("failed to decode commit response: %w", err)
	}

	return commit.SHA, nil
}

// getTreeSHAFromCommit gets the tree SHA from a commit
func (s *GitHubServiceImpl) getTreeSHAFromCommit(owner, repo, commitSHA, token string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/commits/%s", owner, repo, commitSHA)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to get commit: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "getTreeSHAFromCommit"))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get commit: %s, status: %d", string(body), resp.StatusCode)
	}

	var commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&commit); err != nil {
		return "", fmt.Errorf("failed to decode commit response: %w", err)
	}

	return commit.Tree.SHA, nil
}

// createBlobsForFilesChangedFromParent creates tree entries for all changes from a specific parent commit
// Handles additions, modifications, deletions, and renames properly using git diff-tree
func (s *GitHubServiceImpl) createBlobsForFilesChangedFromParent(owner, repo, directory, parentSHA, token string, excludes []string, noFileLimit bool) ([]models.GitHubTreeEntry, error) {
	// Use git diff-tree with -r (recursive), --name-status (show status), -M (detect renames)
	// This shows the exact operation for each file: A (add), M (modify), D (delete), R (rename)
	cmd := newGitCommand(s.executor("git", "diff-tree", "-r", "--name-status", "-M", parentSHA, "HEAD"), directory, true, true)
	if err := cmd.run(); err != nil {
		return nil, fmt.Errorf("failed to get diff-tree from parent: %w, stderr: %s", err, cmd.getStderr())
	}

	if !cmd.hasStdout() {
		// No changes from parent
		return []models.GitHubTreeEntry{}, nil
	}

	var treeEntries []models.GitHubTreeEntry
	lines := strings.Split(strings.TrimSpace(cmd.getStdout()), "\n")

	// Check file count limit to prevent oversized commits.
	// Merge jobs skip this guardrail — merging upstream into a
	// feature branch legitimately touches hundreds of files.
	if noFileLimit {
		s.logger.Info("File count guardrail skipped for merge commit",
			zap.Int("file_count", len(lines)))
	} else {
		limit := maxMergeCommitFiles
		if s.config.Guardrails.MaxCommitFiles > 0 && s.config.Guardrails.MaxCommitFiles < limit {
			limit = s.config.Guardrails.MaxCommitFiles
		}
		if len(lines) > limit {
			if limit == maxMergeCommitFiles {
				return nil, fmt.Errorf("commit has %d changed files, exceeds hard safety cap of %d", len(lines), limit)
			}
			return nil, fmt.Errorf("commit has %d changed files, exceeds guardrails.max_commit_files limit of %d — the AI likely modified more files than intended; increase the limit in config if this is expected", len(lines), limit)
		}
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse the diff-tree output format:
		// For A/M/D: <status>\t<file>
		// For R: R<similarity>\t<old-file>\t<new-file>
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}

		status := parts[0]

		s.logger.Debug("Processing file change from parent",
			zap.String("status", status),
			zap.String("line", line),
			zap.String("parent", parentSHA))

		switch {
		case status == "A" || status == "M":
			// Added or Modified file - create blob and add to tree
			filename := parts[1]
			// Skip new root-level files for AI-authored commits — these
			// are almost always scratch files. Merge commits bypass this
			// filter since root-level additions are legitimate upstream
			// changes.
			if !noFileLimit && status == "A" && !strings.Contains(filename, "/") {
				s.logger.Info("Skipping new root-level file",
					zap.String("file", filename))
				continue
			}
			entry, err := s.createTreeEntryForFile(owner, repo, directory, filename, token, excludes)
			if errors.Is(err, errSkipEntry) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed to create tree entry for %s: %w", filename, err)
			}
			treeEntries = append(treeEntries, entry)

		case status == "D":
			// Deleted file - add entry with nil SHA to remove from tree
			filename := parts[1]
			if isExcludedPath(filename, excludes) {
				continue
			}
			s.logger.Debug("File deleted, adding deletion entry",
				zap.String("file", filename))
			treeEntries = append(treeEntries, models.GitHubTreeEntry{
				Path: filename,
				Mode: "100644",
				Type: "blob",
				SHA:  nil, // nil SHA tells GitHub to delete this file
			})

		case strings.HasPrefix(status, "R"):
			// Renamed file - delete old path and add new path
			if len(parts) < 3 {
				s.logger.Warn("Invalid rename entry, skipping", zap.String("line", line))
				continue
			}
			oldPath := parts[1]
			newPath := parts[2]
			if isExcludedPath(oldPath, excludes) && isExcludedPath(newPath, excludes) {
				continue
			}

			s.logger.Debug("File renamed",
				zap.String("old_path", oldPath),
				zap.String("new_path", newPath))

			// Delete old path
			if !isExcludedPath(oldPath, excludes) {
				treeEntries = append(treeEntries, models.GitHubTreeEntry{
					Path: oldPath,
					Mode: "100644",
					Type: "blob",
					SHA:  nil,
				})
			}

			// Add new path
			entry, err := s.createTreeEntryForFile(owner, repo, directory, newPath, token, excludes)
			if errors.Is(err, errSkipEntry) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed to create tree entry for renamed file %s: %w", newPath, err)
			}
			treeEntries = append(treeEntries, entry)

		case strings.HasPrefix(status, "C"):
			// Copied file - just add the new copy
			if len(parts) < 3 {
				s.logger.Warn("Invalid copy entry, skipping", zap.String("line", line))
				continue
			}
			newPath := parts[2]
			entry, err := s.createTreeEntryForFile(owner, repo, directory, newPath, token, excludes)
			if errors.Is(err, errSkipEntry) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("failed to create tree entry for copied file %s: %w", newPath, err)
			}
			treeEntries = append(treeEntries, entry)

		default:
			s.logger.Warn("Unknown diff-tree status, skipping",
				zap.String("status", status),
				zap.String("line", line))
		}
	}

	return treeEntries, nil
}

// errSkipEntry signals that a tree entry should be skipped (e.g.,
// the path is a directory or a bot artifact).
var errSkipEntry = errors.New("skip entry")

// builtinExcludes lists path prefixes that are always excluded from
// commits. Entries without a trailing slash are prefix matches — e.g.,
// ".ai-session" excludes .ai-session/, .ai-session.preserve/, and
// any other path starting with ".ai-session". Entries with a trailing
// slash match only that exact directory. Import-declared excludes are
// merged at call time.
var builtinExcludes = []string{".ai-session"}

// mergeExcludes combines builtin excludes with import-declared excludes.
// Builtin entries are kept as-is (no trailing slash = broad prefix match).
// Import-declared entries are normalized to have a trailing slash for
// exact directory matching.
func mergeExcludes(importExcludes []string) []string {
	all := make([]string, len(builtinExcludes), len(builtinExcludes)+len(importExcludes))
	copy(all, builtinExcludes)
	for _, e := range importExcludes {
		if !strings.HasSuffix(e, "/") {
			e += "/"
		}
		all = append(all, e)
	}
	return all
}

// isExcludedPath reports whether filename is inside any of the
// excluded directories.
func isExcludedPath(filename string, excludes []string) bool {
	for _, prefix := range excludes {
		dir := strings.TrimSuffix(prefix, "/")
		if filename == dir || strings.HasPrefix(filename, prefix) {
			return true
		}
	}
	return false
}

// createTreeEntryForFile creates a tree entry for a single file.
// Text files use inline content (GitHub creates the blob server-side),
// eliminating per-file API calls. Binary files fall back to the
// blob creation API with base64 encoding. Returns errSkipEntry if
// the path is excluded or is a directory.
func (s *GitHubServiceImpl) createTreeEntryForFile(owner, repo, directory, filename, token string, excludes []string) (models.GitHubTreeEntry, error) {
	if isExcludedPath(filename, excludes) {
		s.logger.Debug("Skipping excluded path",
			zap.String("path", filename))
		return models.GitHubTreeEntry{}, errSkipEntry
	}

	filePath := filepath.Join(directory, filename)

	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return models.GitHubTreeEntry{}, fmt.Errorf("failed to stat file %s: %w", filename, err)
	}
	if fileInfo.IsDir() {
		s.logger.Debug("Skipping directory entry",
			zap.String("path", filename))
		return models.GitHubTreeEntry{}, errSkipEntry
	}

	// #nosec G304 - filename comes from git diff-tree output in controlled repo directory
	raw, err := os.ReadFile(filePath)
	if err != nil {
		return models.GitHubTreeEntry{}, fmt.Errorf("failed to read file %s: %w", filename, err)
	}

	mode := "100644"
	if fileInfo.Mode()&0111 != 0 {
		mode = "100755"
	}

	if utf8.Valid(raw) && len(raw) <= maxInlineContentBytes {
		content := string(raw)
		return models.GitHubTreeEntry{
			Path:    filename,
			Mode:    mode,
			Type:    "blob",
			Content: &content,
		}, nil
	}

	// Binary file or file too large for inline content — create blob
	// via the API. Large text files use UTF-8 encoding; binary files
	// use base64.
	encoding := "utf-8"
	content := string(raw)
	if !utf8.Valid(raw) {
		encoding = "base64"
		content = base64.StdEncoding.EncodeToString(raw)
	}
	blobSHA, err := s.createBlobWithEncoding(owner, repo, content, encoding, token)
	if err != nil {
		return models.GitHubTreeEntry{}, fmt.Errorf("failed to create blob for %s: %w", filename, err)
	}

	s.logger.Debug("Created blob via API",
		zap.String("file", filename),
		zap.String("sha", blobSHA),
		zap.String("reason", func() string {
			if !utf8.Valid(raw) {
				return "binary"
			}
			return "large file"
		}()))

	return models.GitHubTreeEntry{
		Path: filename,
		Mode: mode,
		Type: "blob",
		SHA:  &blobSHA,
	}, nil
}

// createBlobWithEncoding creates a blob on GitHub via the Git Data
// API. Used for binary files that can't be inlined as UTF-8 content
// in tree entries. Includes retry logic for rate limiting.
func (s *GitHubServiceImpl) createBlobWithEncoding(owner, repo, content, encoding, token string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/blobs", owner, repo)

	blobReq := models.GitHubBlobRequest{
		Content:  content,
		Encoding: encoding,
	}

	jsonPayload, err := json.Marshal(blobReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal blob request: %w", err)
	}

	// Retry logic for rate limiting
	maxRetries := 3
	baseDelay := 2 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff: 2s, 4s, 8s
			// #nosec G115 - attempt is bounded by maxRetries (3), so shift is safe
			delay := baseDelay * time.Duration(1<<uint(attempt-1))
			s.logger.Warn("Rate limited, retrying blob creation",
				zap.Int("attempt", attempt),
				zap.Duration("delay", delay),
				zap.String("owner", owner),
				zap.String("repo", repo))
			time.Sleep(delay)
		}

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
		if err != nil {
			return "", fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := s.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("failed to create blob: %w", err)
		}

		// Read response body
		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if closeErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(closeErr), zap.String("operation", "createBlobWithEncoding"))
		}
		if readErr != nil {
			return "", fmt.Errorf("failed to read response body: %w", readErr)
		}

		// Success case
		if resp.StatusCode == http.StatusCreated {
			var blobResp models.GitHubBlobResponse
			if err := json.Unmarshal(body, &blobResp); err != nil {
				return "", fmt.Errorf("failed to decode blob response: %w", err)
			}
			return blobResp.SHA, nil
		}

		// Check for rate limit errors
		// Primary rate limits: HTTP 429
		// Secondary rate limits: HTTP 403 with "rate limit" in error message
		isRateLimit := resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "rate limit"))

		if isRateLimit {
			// Check if Retry-After header is present
			retryAfter := resp.Header.Get("Retry-After")
			if retryAfter != "" {
				s.logger.Warn("Rate limit hit with Retry-After header",
					zap.String("retry_after", retryAfter),
					zap.Int("attempt", attempt),
					zap.Int("status_code", resp.StatusCode))
			}

			// Log rate limit headers for debugging
			s.logger.Debug("Rate limit headers",
				zap.String("x-ratelimit-remaining", resp.Header.Get("X-RateLimit-Remaining")),
				zap.String("x-ratelimit-reset", resp.Header.Get("X-RateLimit-Reset")),
				zap.Int("status_code", resp.StatusCode))

			if attempt < maxRetries {
				// Will retry after backoff
				continue
			}
			// Max retries exceeded
			return "", fmt.Errorf("rate limit exceeded after %d retries: %s, status: %d", maxRetries, string(body), resp.StatusCode)
		}

		// Other error - don't retry
		return "", fmt.Errorf("failed to create blob: %s, status: %d", string(body), resp.StatusCode)
	}

	return "", fmt.Errorf("failed to create blob after %d attempts", maxRetries)
}

// maxTreeEntriesPerRequest is the maximum number of tree entries per
// API call. GitHub has undocumented limits on tree creation payload
// size. We chunk requests to stay under them, using each chunk's
// result as the base_tree for the next.
const maxTreeEntriesPerRequest = 900

// createTree creates a tree on GitHub via the Git Data API. When the
// number of entries exceeds maxTreeEntriesPerRequest, the request is
// chunked: each batch uses the previous batch's tree SHA as its
// base_tree, building incrementally.
func (s *GitHubServiceImpl) createTree(owner, repo, baseTree string, entries []models.GitHubTreeEntry, token string) (string, error) {
	if len(entries) <= maxTreeEntriesPerRequest {
		return s.createTreeRequest(owner, repo, baseTree, entries, token)
	}

	s.logger.Info("Chunking tree creation",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.Int("total_entries", len(entries)),
		zap.Int("chunk_size", maxTreeEntriesPerRequest))

	currentBase := baseTree
	for i := 0; i < len(entries); i += maxTreeEntriesPerRequest {
		end := i + maxTreeEntriesPerRequest
		if end > len(entries) {
			end = len(entries)
		}
		chunk := entries[i:end]

		sha, err := s.createTreeRequest(owner, repo, currentBase, chunk, token)
		if err != nil {
			return "", fmt.Errorf("tree chunk %d-%d of %d: %w", i, end, len(entries), err)
		}
		currentBase = sha

		s.logger.Debug("Tree chunk created",
			zap.Int("from", i),
			zap.Int("to", end),
			zap.String("treeSHA", sha))
	}

	return currentBase, nil
}

func (s *GitHubServiceImpl) createTreeRequest(owner, repo, baseTree string, entries []models.GitHubTreeEntry, token string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/trees", owner, repo)

	treeReq := models.GitHubTreeRequest{
		BaseTree: baseTree,
		Tree:     entries,
	}

	jsonPayload, err := json.Marshal(treeReq)
	if err != nil {
		return "", fmt.Errorf("failed to marshal tree request: %w", err)
	}

	maxRetries := 3
	baseDelay := 2 * time.Second

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// #nosec G115 - attempt is bounded by maxRetries (3), so shift is safe
			delay := baseDelay * time.Duration(1<<uint(attempt-1))
			s.logger.Warn("Rate limited, retrying tree creation",
				zap.Int("attempt", attempt),
				zap.Duration("delay", delay))
			time.Sleep(delay)
		}

		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
		if err != nil {
			return "", fmt.Errorf("failed to create request: %w", err)
		}

		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := s.client.Do(req)
		if err != nil {
			if attempt < maxRetries {
				s.logger.Warn("Transient error creating tree, retrying",
					zap.Error(err), zap.Int("attempt", attempt))
				continue
			}
			return "", fmt.Errorf("failed to create tree: %w", err)
		}

		body, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if closeErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(closeErr), zap.String("operation", "createTree"))
		}
		if readErr != nil {
			return "", fmt.Errorf("failed to read response body: %w", readErr)
		}

		if resp.StatusCode == http.StatusCreated {
			var treeResp models.GitHubTreeResponse
			if err := json.Unmarshal(body, &treeResp); err != nil {
				return "", fmt.Errorf("failed to decode tree response: %w", err)
			}
			return treeResp.SHA, nil
		}

		isRateLimit := resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode == http.StatusForbidden && strings.Contains(string(body), "rate limit"))

		if isRateLimit && attempt < maxRetries {
			continue
		}

		return "", fmt.Errorf("failed to create tree: %s, status: %d", string(body), resp.StatusCode)
	}

	return "", fmt.Errorf("failed to create tree after %d attempts", maxRetries)
}

// updateReference updates a Git reference to point to a new commit
func (s *GitHubServiceImpl) updateReference(owner, repo, branchName, commitSHA, token string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", owner, repo, branchName)

	refReq := models.GitHubReferenceRequest{
		SHA:   commitSHA,
		Force: false,
	}

	jsonPayload, err := json.Marshal(refReq)
	if err != nil {
		return fmt.Errorf("failed to marshal reference request: %w", err)
	}

	req, err := http.NewRequest("PATCH", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to update reference: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "updateReference"))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to update reference: %s, status: %d", string(body), resp.StatusCode)
	}

	return nil
}

// getBranchBaseCommit gets the base commit SHA for a branch
// If the branch doesn't exist, falls back to the target branch
// Returns: (baseSHA, branchExists, error)
func (s *GitHubServiceImpl) getBranchBaseCommit(owner, repo, branchName, baseBranch, token string) (string, bool, error) {
	// Try to get the branch reference
	refURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", owner, repo, branchName)
	req, err := http.NewRequest("GET", refURL, nil)
	if err != nil {
		return "", false, fmt.Errorf("failed to create reference request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("failed to get reference: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "getBranchBaseCommit"))
		}
	}()

	if resp.StatusCode == http.StatusOK {
		// Branch exists, return its SHA
		var refResponse models.GitHubGetReferenceResponse
		if err := json.NewDecoder(resp.Body).Decode(&refResponse); err != nil {
			return "", false, fmt.Errorf("failed to decode reference response: %w", err)
		}
		return refResponse.Object.SHA, true, nil
	}

	if resp.StatusCode == http.StatusNotFound {
		// Branch doesn't exist, fall back to target branch
		s.logger.Info("Branch does not exist on remote, using target branch as base",
			zap.String("branch", branchName),
			zap.String("targetBranch", baseBranch))

		// Get target branch reference
		targetRefURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs/heads/%s", owner, repo, baseBranch)
		targetReq, err := http.NewRequest("GET", targetRefURL, nil)
		if err != nil {
			return "", false, fmt.Errorf("failed to create target branch request: %w", err)
		}

		targetReq.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
		targetReq.Header.Set("Accept", "application/vnd.github.v3+json")

		targetResp, err := s.client.Do(targetReq)
		if err != nil {
			return "", false, fmt.Errorf("failed to get target branch reference: %w", err)
		}
		defer func() {
			if localErr := targetResp.Body.Close(); localErr != nil {
				s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "getBranchBaseCommit-target"))
			}
		}()

		if targetResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(targetResp.Body)
			return "", false, fmt.Errorf("failed to get target branch %s: %s, status: %d", baseBranch, string(body), targetResp.StatusCode)
		}

		var targetRefResponse models.GitHubGetReferenceResponse
		if err := json.NewDecoder(targetResp.Body).Decode(&targetRefResponse); err != nil {
			return "", false, fmt.Errorf("failed to decode target branch response: %w", err)
		}

		return targetRefResponse.Object.SHA, false, nil
	}

	// Unexpected status code
	body, _ := io.ReadAll(resp.Body)
	return "", false, fmt.Errorf("unexpected response when getting branch reference: %s, status: %d", string(body), resp.StatusCode)
}

// createReference creates a new Git reference
func (s *GitHubServiceImpl) createReference(owner, repo, branchName, commitSHA, token string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs", owner, repo)

	refReq := models.GitHubCreateReferenceRequest{
		Ref: fmt.Sprintf("refs/heads/%s", branchName),
		SHA: commitSHA,
	}

	jsonPayload, err := json.Marshal(refReq)
	if err != nil {
		return fmt.Errorf("failed to marshal reference request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to create reference: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "createReference"))
		}
	}()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to create reference: %s, status: %d", string(body), resp.StatusCode)
	}

	return nil
}

// CreatePR creates a pull request from the given parameters and returns
// the created PR metadata. If params.Labels is non-empty those labels are
// applied; otherwise the configured PRLabel (if any) is used as a
// backwards-compatible default.
func (s *GitHubServiceImpl) CreatePR(params models.PRParams) (*models.PR, error) {
	// Use installation-specific client
	// PRs are created on the base repository, so use the base repo's installation
	installationID, err := s.getInstallationIDForRepo(params.Owner, params.Repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation ID for %s/%s: %w", params.Owner, params.Repo, err)
	}
	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation client: %w", err)
	}

	s.logger.Info("Creating PR using base repository's installation",
		zap.String("owner", params.Owner),
		zap.String("repo", params.Repo),
		zap.Int64("installationID", installationID))

	// Set maintainer_can_modify to false explicitly
	// This is required when using GitHub App tokens to create PRs from forks
	// See: https://github.com/orgs/community/discussions/39178
	falseValue := false
	newPR := &github.NewPullRequest{
		Title:               &params.Title,
		Body:                &params.Body,
		Head:                &params.Head,
		Base:                &params.Base,
		MaintainerCanModify: &falseValue,
		Draft:               &params.Draft,
	}

	// Create PR with its own timeout
	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	pr, _, err := ghClient.PullRequests.Create(ctx, params.Owner, params.Repo, newPR)
	cancel() // Release resources immediately after PR creation
	if err != nil {
		return nil, fmt.Errorf("failed to create pull request: %w", err)
	}

	// Defensive nil check - should never occur if go-github contract is honored,
	// but protects against unexpected API behavior changes
	if pr == nil {
		return nil, fmt.Errorf("GitHub API returned nil pull request (unexpected API contract violation)")
	}

	// Determine labels to apply: prefer explicit params, fall back to config.
	labels := params.Labels
	if len(labels) == 0 && s.config.GitHub.PRLabel != "" {
		labels = []string{s.config.GitHub.PRLabel}
	}

	// Add labels with a fresh timeout to prevent cascading timeout if PR creation was slow
	if len(labels) > 0 {
		labelCtx, labelCancel := context.WithTimeout(context.Background(), githubAPITimeout)
		defer labelCancel()
		_, _, err = ghClient.Issues.AddLabelsToIssue(labelCtx, params.Owner, params.Repo, pr.GetNumber(), labels)
		if err != nil {
			return nil, fmt.Errorf("failed to add labels to PR #%d: %w (PR created but not labeled)",
				pr.GetNumber(), err)
		}
	}

	// Assign users to the PR.
	if len(params.Assignees) > 0 {
		assignCtx, assignCancel := context.WithTimeout(context.Background(), githubAPITimeout)
		defer assignCancel()
		_, _, err = ghClient.Issues.AddAssignees(assignCtx, params.Owner, params.Repo, pr.GetNumber(), params.Assignees)
		if err != nil {
			// Non-fatal: log but don't fail the PR creation.
			s.logger.Warn("Failed to assign PR",
				zap.Int("pr", pr.GetNumber()),
				zap.Strings("assignees", params.Assignees),
				zap.Error(err))
		}
	}

	return &models.PR{
		Number: pr.GetNumber(),
		URL:    pr.GetHTMLURL(),
		State:  pr.GetState(),
	}, nil
}

// SwitchBranch switches to a specific branch.
func (s *GitHubServiceImpl) SwitchBranch(directory, branchName string) error {
	return s.gitOps.SwitchBranch(directory, branchName)
}

func (s *GitHubServiceImpl) HasChanges(directory, baseBranch string) (bool, error) {
	return s.gitOps.HasChanges(directory, baseBranch)
}


func (s *GitHubServiceImpl) stageAndCommitLocal(directory string, fn zapcore.Field) error {
	return s.gitOps.StageAndCommitLocal(directory, fn)
}


// StripRemoteAuth removes authentication credentials from the
// workspace's origin remote URL. After this call, push operations
// will be rejected by the remote.
func (s *GitHubServiceImpl) StripRemoteAuth(directory string) error {
	return s.gitOps.StripRemoteAuth(directory)
}

// RestoreRemoteAuth restores authentication credentials on the
// workspace's origin remote URL using a fresh installation token.
func (s *GitHubServiceImpl) RestoreRemoteAuth(directory, owner, repo string) error {
	token, err := s.getAuthTokenForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("get auth token: %w", err)
	}

	authURL := fmt.Sprintf("https://%s@github.com/%s/%s.git", token, owner, repo)
	cmd := newGitCommand(
		s.executor("git", "remote", "set-url", "origin", authURL),
		directory, false, false)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("restore remote auth: %w, stderr: %s", err, cmd.getStderr())
	}

	s.logger.Debug("Restored remote auth", zap.String("directory", directory))
	return nil
}

func (s *GitHubServiceImpl) FetchRemote(directory string) error {
	return s.gitOps.FetchRemote(directory)
}

func (s *GitHubServiceImpl) SyncWithRemote(directory, branch string, importExcludes []string) error {
	return s.gitOps.SyncWithRemote(directory, branch, importExcludes)
}


// ReplyToComment replies to a specific PR review comment.
// For line-based review comments, this creates a threaded reply.
func (s *GitHubServiceImpl) ReplyToComment(owner, repo string, prNumber int, commentID int64, body string) error {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	_, _, err = ghClient.PullRequests.CreateCommentInReplyTo(ctx, owner, repo, prNumber, body, commentID)
	if err != nil {
		return fmt.Errorf("failed to reply to PR comment: %w", err)
	}

	s.logger.Debug("Replied to review comment",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.Int("pr_number", prNumber),
		zap.Int64("comment_id", commentID))

	return nil
}

// PostIssueComment posts a top-level comment on a PR (via the issues
// endpoint) and returns the created comment's ID. Used for replying
// to conversation comments, which do not support threading.
func (s *GitHubServiceImpl) PostIssueComment(owner, repo string, prNumber int, body string) (int64, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return 0, fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return 0, fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	comment := &github.IssueComment{Body: github.Ptr(body)}
	created, _, err := ghClient.Issues.CreateComment(ctx, owner, repo, prNumber, comment)
	if err != nil {
		return 0, fmt.Errorf("failed to post issue comment: %w", err)
	}

	s.logger.Debug("Posted issue comment",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.Int("pr_number", prNumber),
		zap.Int64("comment_id", created.GetID()))

	return created.GetID(), nil
}

// ListIssueComments returns all top-level comments on a PR (via the
// issues endpoint). Results are ordered by creation time ascending.
func (s *GitHubServiceImpl) ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	var result []models.IssueComment
	for {
		comments, resp, err := ghClient.Issues.ListComments(ctx, owner, repo, prNumber, opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list issue comments: %w", err)
		}
		for _, c := range comments {
			result = append(result, models.IssueComment{
				ID:   c.GetID(),
				Body: c.GetBody(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if result == nil {
		result = []models.IssueComment{}
	}
	return result, nil
}

// UpdateIssueComment edits an existing top-level comment on a PR.
func (s *GitHubServiceImpl) UpdateIssueComment(owner, repo string, commentID int64, body string) error {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	comment := &github.IssueComment{Body: github.Ptr(body)}
	_, _, err = ghClient.Issues.EditComment(ctx, owner, repo, commentID, comment)
	if err != nil {
		return fmt.Errorf("failed to update issue comment: %w", err)
	}

	s.logger.Debug("Updated issue comment",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.Int64("comment_id", commentID))

	return nil
}

// AddCommentReaction adds an emoji reaction to a PR comment. For review
// comments (file-level) the pull request reactions API is used; for
// conversation comments the issue comment reactions API is used.
func (s *GitHubServiceImpl) AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	if comment.IsReviewComment {
		_, _, err = ghClient.Reactions.CreatePullRequestCommentReaction(ctx, owner, repo, comment.ID, reaction)
	} else {
		_, _, err = ghClient.Reactions.CreateIssueCommentReaction(ctx, owner, repo, comment.ID, reaction)
	}
	if err != nil {
		return fmt.Errorf("failed to add %s reaction to comment %d: %w", reaction, comment.ID, err)
	}

	s.logger.Debug("Added reaction to comment",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.Int64("comment_id", comment.ID),
		zap.String("reaction", reaction),
		zap.Bool("is_review_comment", comment.IsReviewComment))

	return nil
}

// CloneImport clones an auxiliary repository into destDir. If ref is
// non-empty, the specified branch/tag/commit is checked out. This is a
// shallow clone (depth 1) since import repos are read-only references.
func (s *GitHubServiceImpl) CloneImport(url, destDir, ref string) error {
	return s.gitOps.CloneImport(url, destDir, ref)
}

// fetchPRReviewCommentsPage fetches a single page of PR review comments
func (s *GitHubServiceImpl) fetchPRReviewCommentsPage(owner, repo string, prNumber, page, perPage int) ([]models.GitHubPRComment, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	opts := &github.PullRequestListCommentsOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: perPage,
		},
	}

	ghComments, _, err := ghClient.PullRequests.ListComments(ctx, owner, repo, prNumber, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR review comments: %w", err)
	}

	// Convert go-github comments to our model
	comments := make([]models.GitHubPRComment, 0, len(ghComments))
	for _, c := range ghComments {
		// Defensive nil check for User object
		user := c.GetUser()
		login := ""
		if user != nil {
			login = user.GetLogin()
		}

		comments = append(comments, models.GitHubPRComment{
			ID:          c.GetID(),
			InReplyToID: c.GetInReplyTo(),
			User: models.GitHubUser{
				Login: login,
			},
			Body:      c.GetBody(),
			Path:      c.GetPath(),
			Line:      c.GetLine(),
			StartLine: c.GetStartLine(),
			Side:      c.GetSide(),
			StartSide: c.GetStartSide(),
			HTMLURL:   c.GetHTMLURL(),
			CreatedAt: c.GetCreatedAt().Time,
			UpdatedAt: c.GetUpdatedAt().Time,
		})
	}

	return comments, nil
}

// listPRReviewComments lists line-based review comments on a PR (from pulls endpoint)
// Handles pagination to retrieve all comments
func (s *GitHubServiceImpl) listPRReviewComments(owner, repo string, prNumber int) ([]models.GitHubPRComment, error) {
	var allComments []models.GitHubPRComment
	page := 1
	perPage := 100

	for {
		comments, err := s.fetchPRReviewCommentsPage(owner, repo, prNumber, page, perPage)
		if err != nil {
			return nil, err
		}

		allComments = append(allComments, comments...)

		// If we got fewer than perPage results, we've reached the last page
		// Also break on empty response to avoid infinite loop edge case
		if len(comments) == 0 || len(comments) < perPage {
			break
		}

		page++
		// Safety limit: prevent infinite loop if GitHub API misbehaves
		if page > maxPaginationPages {
			s.logger.Warn("Hit pagination safety limit for PR review comments",
				zap.Int("page", page),
				zap.Int("comments_retrieved", len(allComments)))
			break
		}
	}

	return allComments, nil
}

// fetchPRConversationCommentsPage fetches a single page of PR conversation comments
func (s *GitHubServiceImpl) fetchPRConversationCommentsPage(owner, repo string, prNumber, page, perPage int) ([]models.GitHubPRComment, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	opts := &github.IssueListCommentsOptions{
		ListOptions: github.ListOptions{
			Page:    page,
			PerPage: perPage,
		},
	}

	ghComments, _, err := ghClient.Issues.ListComments(ctx, owner, repo, prNumber, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get PR conversation comments: %w", err)
	}

	// Convert go-github comments to our model
	// Note: Issue comments don't have path/line/side fields (only PR review comments do)
	comments := make([]models.GitHubPRComment, 0, len(ghComments))
	for _, c := range ghComments {
		// Defensive nil check for User object
		user := c.GetUser()
		login := ""
		if user != nil {
			login = user.GetLogin()
		}

		comments = append(comments, models.GitHubPRComment{
			ID: c.GetID(),
			User: models.GitHubUser{
				Login: login,
			},
			Body:      c.GetBody(),
			HTMLURL:   c.GetHTMLURL(),
			CreatedAt: c.GetCreatedAt().Time,
			UpdatedAt: c.GetUpdatedAt().Time,
			// Path, Line, StartLine, Side, StartSide not set for conversation comments
		})
	}

	return comments, nil
}

// listPRConversationComments lists general conversation comments on a PR (from issues endpoint)
// Handles pagination to retrieve all comments
func (s *GitHubServiceImpl) listPRConversationComments(owner, repo string, prNumber int) ([]models.GitHubPRComment, error) {
	var allComments []models.GitHubPRComment
	page := 1
	perPage := 100

	for {
		comments, err := s.fetchPRConversationCommentsPage(owner, repo, prNumber, page, perPage)
		if err != nil {
			return nil, err
		}

		allComments = append(allComments, comments...)

		// If we got fewer than perPage results, we've reached the last page
		// Also break on empty response to avoid infinite loop edge case
		if len(comments) == 0 || len(comments) < perPage {
			break
		}

		page++
		// Safety limit: prevent infinite loop if GitHub API misbehaves
		if page > maxPaginationPages {
			s.logger.Warn("Hit pagination safety limit for PR conversation comments",
				zap.Int("page", page),
				zap.Int("comments_retrieved", len(allComments)))
			break
		}
	}

	return allComments, nil
}

// listPRReviewBodies fetches PR reviews and returns non-empty review
// bodies as GitHubPRComment entries. Review bodies are the top-level
// text submitted with a review (e.g., CodeRabbit's summary with
// inline nitpicks). These are distinct from line-level review comments.
func (s *GitHubServiceImpl) listPRReviewBodies(owner, repo string, prNumber int) ([]models.GitHubPRComment, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation ID: %w", err)
	}

	ghClient, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get installation client: %w", err)
	}

	var allReviews []*github.PullRequestReview
	page := 1
	perPage := 100

	for {
		ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
		opts := &github.ListOptions{Page: page, PerPage: perPage}
		reviews, _, err := ghClient.PullRequests.ListReviews(ctx, owner, repo, prNumber, opts)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("failed to list PR reviews: %w", err)
		}

		allReviews = append(allReviews, reviews...)
		if len(reviews) < perPage {
			break
		}
		page++
		if page > maxPaginationPages {
			break
		}
	}

	var result []models.GitHubPRComment
	for _, r := range allReviews {
		body := strings.TrimSpace(r.GetBody())
		if body == "" {
			continue
		}
		user := r.GetUser()
		login := ""
		if user != nil {
			login = user.GetLogin()
		}
		result = append(result, models.GitHubPRComment{
			ID:        r.GetID(),
			User:      models.GitHubUser{Login: login},
			Body:      body,
			HTMLURL:   r.GetHTMLURL(),
			CreatedAt: r.GetSubmittedAt().Time,
		})
	}

	return result, nil
}

// GetPRComments returns all PR comments (line-based review comments,
// general conversation comments, and review bodies), converted to
// models.PRComment. If since is non-zero, only comments created after
// that timestamp are returned.
func (s *GitHubServiceImpl) GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
	// Get line-based review comments from pulls endpoint
	reviewComments, err := s.listPRReviewComments(owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get review comments: %w", err)
	}

	// Get general conversation comments from issues endpoint
	conversationComments, err := s.listPRConversationComments(owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get conversation comments: %w", err)
	}

	// Get review bodies (top-level text submitted with PR reviews)
	reviewBodies, err := s.listPRReviewBodies(owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("failed to get review bodies: %w", err)
	}

	s.logger.Debug("Retrieved PR comments",
		zap.String("owner", owner),
		zap.String("repo", repo),
		zap.Int("pr_number", number),
		zap.Int("review_comments", len(reviewComments)),
		zap.Int("conversation_comments", len(conversationComments)),
		zap.Int("review_bodies", len(reviewBodies)),
		zap.Int("total_comments", len(reviewComments)+len(conversationComments)+len(reviewBodies)))

	// Convert to models.PRComment and apply the since filter.
	// Each source is converted separately so IsReviewComment is set
	// correctly.
	total := len(reviewComments) + len(conversationComments) + len(reviewBodies)
	result := make([]models.PRComment, 0, total)
	for _, c := range reviewComments {
		if !since.IsZero() && !c.CreatedAt.After(since) {
			continue
		}
		result = append(result, models.PRComment{
			ID: c.ID,
			Author: models.Author{
				Name:     c.User.Login,
				Username: c.User.Login,
			},
			Body:            c.Body,
			FilePath:        c.Path,
			Line:            c.Line,
			URL:             c.HTMLURL,
			Timestamp:       c.CreatedAt,
			InReplyTo:       c.InReplyToID,
			IsReviewComment: true,
		})
	}
	for _, c := range conversationComments {
		if !since.IsZero() && !c.CreatedAt.After(since) {
			continue
		}
		result = append(result, models.PRComment{
			ID: c.ID,
			Author: models.Author{
				Name:     c.User.Login,
				Username: c.User.Login,
			},
			Body:      c.Body,
			URL:       c.HTMLURL,
			Timestamp: c.CreatedAt,
		})
	}
	for _, c := range reviewBodies {
		if !since.IsZero() && !c.CreatedAt.After(since) {
			continue
		}
		result = append(result, models.PRComment{
			ID: c.ID,
			Author: models.Author{
				Name:     c.User.Login,
				Username: c.User.Login,
			},
			Body:      c.Body,
			URL:       c.HTMLURL,
			Timestamp: c.CreatedAt,
		})
	}

	return result, nil
}

// extractRepoInfo extracts owner and repo from a repository URL.
func extractRepoInfo(repoURL string) (owner, repo string, err error) {
	// Handle SSH URLs: git@github.com:owner/repo.git
	if strings.HasPrefix(repoURL, "git@github.com:") {
		parts := strings.Split(strings.TrimPrefix(repoURL, "git@github.com:"), "/")
		if len(parts) < 2 {
			return "", "", fmt.Errorf("invalid GitHub SSH URL: %s", repoURL)
		}
		owner = parts[0]
		repo = strings.TrimSuffix(parts[1], ".git")
		return owner, repo, nil
	}

	// Handle HTTPS URLs: https://github.com/owner/repo.git
	if strings.HasPrefix(repoURL, "https://github.com/") {
		parts := strings.Split(strings.TrimPrefix(repoURL, "https://github.com/"), "/")
		if len(parts) < 2 {
			return "", "", fmt.Errorf("invalid GitHub HTTPS URL: %s", repoURL)
		}
		owner = parts[0]
		repo = strings.TrimSuffix(parts[1], ".git")
		return owner, repo, nil
	}

	return "", "", fmt.Errorf("unsupported repository URL format: %s", repoURL)
}

// getInstallationIDForRepo discovers the GitHub App installation ID for a specific repository.
// This is used in GitHub App authentication mode to obtain installation-specific tokens.
//
// Parameters:
//   - owner: The repository owner (user or organization)
//   - repo: The repository name
//
// Returns:
//   - The installation ID if the GitHub App is installed on the repository
//   - An error if the app is not configured, not installed, or if the API request fails
//
// Example usage:
//
//	installationID, err := githubService.getInstallationIDForRepo("myorg", "myrepo")
//	if err != nil {
//	    // Handle error - app may not be installed on this repo
//	}
func (s *GitHubServiceImpl) getInstallationIDForRepo(owner, repo string) (int64, error) {
	if s.appTransport == nil {
		return 0, fmt.Errorf("GitHub App not configured")
	}

	key := fmt.Sprintf("%s/%s", owner, repo)

	// Fast path: check cache with read lock
	s.installationIDsMu.RLock()
	installationID, exists := s.installationIDs[key]
	s.installationIDsMu.RUnlock()

	if exists {
		return installationID, nil
	}

	// Slow path: fetch from API with timeout
	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/installation", owner, repo)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("failed to create request: %w", err)
	}

	// Use app transport for this request
	client := &http.Client{Transport: s.appTransport}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("failed to get installation: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body", zap.Error(localErr), zap.String("operation", "getInstallationIDForRepo"))
		}
	}()

	if resp.StatusCode == 404 {
		return 0, fmt.Errorf("GitHub App is not installed on %s/%s", owner, repo)
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("failed to get installation (status %d): %s", resp.StatusCode, string(body))
	}

	var installation struct {
		ID int64 `json:"id"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&installation); err != nil {
		return 0, fmt.Errorf("failed to decode installation response: %w", err)
	}

	s.logger.Info("Discovered installation ID",
		zap.String("repo", key),
		zap.Int64("installationID", installation.ID))

	// Cache the result
	s.installationIDsMu.Lock()
	s.installationIDs[key] = installation.ID
	s.installationIDsMu.Unlock()

	return installation.ID, nil
}

// GetPRForBranch finds the open pull request whose head branch matches
// the given name. Returns nil, nil when no matching PR is found.
func (s *GitHubServiceImpl) GetPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	prs, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open",
		Head:  head,
	})
	if err != nil {
		return nil, fmt.Errorf("list PRs for branch %s: %w", head, err)
	}

	// The head parameter may use "owner:branch" format for cross-repo
	// (fork) PRs. Extract just the branch name for comparison since
	// pr.GetHead().GetRef() returns the branch without an owner prefix.
	refToMatch := head
	if _, branch, ok := strings.Cut(head, ":"); ok {
		refToMatch = branch
	}

	for _, pr := range prs {
		if pr.GetHead().GetRef() == refToMatch {
			return &models.PRDetails{
				Number:     pr.GetNumber(),
				Title:      pr.GetTitle(),
				Branch:     pr.GetHead().GetRef(),
				BaseBranch: pr.GetBase().GetRef(),
				URL:        pr.GetHTMLURL(),
				HeadSHA:    pr.GetHead().GetSHA(),
				CreatedAt:  pr.GetCreatedAt().Time,
			}, nil
		}
	}

	return nil, nil
}

// GetClosedPRForBranch finds a closed (not merged) pull request whose
// head branch matches the given name. Returns nil, nil when no rejected
// PR exists.
func (s *GitHubServiceImpl) GetClosedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	prs, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "closed",
		Head:  head,
	})
	if err != nil {
		return nil, fmt.Errorf("list closed PRs for branch %s: %w", head, err)
	}

	refToMatch := head
	if _, branch, ok := strings.Cut(head, ":"); ok {
		refToMatch = branch
	}

	for _, pr := range prs {
		if pr.GetHead().GetRef() == refToMatch && pr.MergedAt == nil {
			return &models.PRDetails{
				Number:     pr.GetNumber(),
				Title:      pr.GetTitle(),
				Branch:     pr.GetHead().GetRef(),
				BaseBranch: pr.GetBase().GetRef(),
				URL:        pr.GetHTMLURL(),
				HeadSHA:    pr.GetHead().GetSHA(),
				CreatedAt:  pr.GetCreatedAt().Time,
			}, nil
		}
	}

	return nil, nil
}

// GetMergedPRForBranch finds a merged pull request whose head branch
// matches the given name. Returns nil, nil when no merged PR exists.
func (s *GitHubServiceImpl) GetMergedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	prs, _, err := client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "closed",
		Head:  head,
	})
	if err != nil {
		return nil, fmt.Errorf("list closed PRs for branch %s: %w", head, err)
	}

	refToMatch := head
	if _, branch, ok := strings.Cut(head, ":"); ok {
		refToMatch = branch
	}

	for _, pr := range prs {
		if pr.GetHead().GetRef() == refToMatch && pr.MergedAt != nil {
			return &models.PRDetails{
				Number:     pr.GetNumber(),
				Title:      pr.GetTitle(),
				Branch:     pr.GetHead().GetRef(),
				BaseBranch: pr.GetBase().GetRef(),
				URL:        pr.GetHTMLURL(),
				HeadSHA:    pr.GetHead().GetSHA(),
				CreatedAt:  pr.GetCreatedAt().Time,
			}, nil
		}
	}

	return nil, nil
}

// BranchHasCommits reports whether the branch has commits beyond the
// base branch. Used by crash recovery to detect completed AI work.
func (s *GitHubServiceImpl) BranchHasCommits(owner, repo, branch, base string) (bool, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return false, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return false, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	comparison, _, err := client.Repositories.CompareCommits(
		ctx, owner, repo, base, branch, nil)
	if err != nil {
		return false, fmt.Errorf("compare %s...%s: %w", base, branch, err)
	}

	return comparison.GetAheadBy() > 0, nil
}

// RemoteBranchExists reports whether the named branch exists on the
// remote repository. Used by the executor to detect when a user has
// deleted a branch (e.g., after closing a PR) so the pipeline can
// start fresh instead of reusing stale local state.
func (s *GitHubServiceImpl) RemoteBranchExists(owner, repo, branch string) (bool, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return false, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return false, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	_, _, err = client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("get ref refs/heads/%s: %w", branch, err)
	}

	return true, nil
}

// DeleteRemoteBranch deletes a branch from the remote repository via
// the GitHub Refs API. Returns nil if the branch does not exist
// (idempotent). Deleting a branch auto-closes any PR whose head
// matches the branch.
func (s *GitHubServiceImpl) DeleteRemoteBranch(owner, repo, branch string) error {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	_, err = client.Git.DeleteRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete ref refs/heads/%s: %w", branch, err)
	}

	return nil
}

// SyncFork syncs a fork's branch with its upstream parent using the
// GitHub merge-upstream API. This ensures the fork's default branch
// is current before creating feature branches, preventing PRs that
// include hundreds of unrelated commits.
func (s *GitHubServiceImpl) SyncFork(forkOwner, repo, branch string) error {
	token, err := s.getAuthTokenForRepo(forkOwner, repo)
	if err != nil {
		return fmt.Errorf("get auth token for fork %s/%s: %w", forkOwner, repo, err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/merge-upstream", forkOwner, repo)

	payload := struct {
		Branch string `json:"branch"`
	}{Branch: branch}

	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal merge-upstream request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonPayload))
	if err != nil {
		return fmt.Errorf("create merge-upstream request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("merge-upstream request failed: %w", err)
	}
	defer func() {
		if localErr := resp.Body.Close(); localErr != nil {
			s.logger.Error("Failed to close response body",
				zap.Error(localErr), zap.String("operation", "SyncFork"))
		}
	}()

	if resp.StatusCode == http.StatusOK {
		s.logger.Info("Fork synced with upstream",
			zap.String("fork", forkOwner+"/"+repo),
			zap.String("branch", branch))
		return nil
	}

	// 409 means the branch cannot be fast-forwarded (fork has diverged).
	// This is not fatal — CreateBranch will still work from origin/branch.
	if resp.StatusCode == http.StatusConflict {
		s.logger.Warn("Fork has diverged from upstream, cannot fast-forward",
			zap.String("fork", forkOwner+"/"+repo),
			zap.String("branch", branch))
		return nil
	}

	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("merge-upstream failed for %s/%s: %s, status: %d",
		forkOwner, repo, string(body), resp.StatusCode)
}

// MergeBase merges a base branch into the current branch in the
// workspace. When fetchURL is non-empty, the base branch is fetched
// from that URL into a temporary remote ref (for fork-mode merges
// where origin points to the fork but the merge target is upstream).
// When fetchURL is empty, origin is used. On a clean merge, returns
// nil and an empty slice. On conflict, returns [ErrMergeConflict]
// and the list of conflicted file paths (conflict markers are left
// in the working tree).
func (s *GitHubServiceImpl) MergeBase(dir, branch, fetchURL string) ([]string, error) {
	return s.gitOps.MergeBase(dir, branch, fetchURL)
}

// parseUntrackedBlockers extracts file paths from git's
// "untracked working tree files would be overwritten by merge" error.
func parseUntrackedBlockers(output string) []string {
	const marker = "The following untracked working tree files would be overwritten by merge:"
	idx := strings.Index(output, marker)
	if idx < 0 {
		return []string{}
	}
	block := output[idx+len(marker):]
	end := strings.Index(block, "Please move or remove them")
	if end > 0 {
		block = block[:end]
	}
	paths := []string{}
	for _, line := range strings.Split(block, "\n") {
		p := strings.TrimSpace(line)
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}


// GetPRMergeability fetches the mergeability status of a pull request.
// GitHub computes mergeability asynchronously; the first GET triggers
// the computation and may return nil. This method retries with a short
// delay to collect the result once it's ready.
func (s *GitHubServiceImpl) GetPRMergeability(owner, repo string, number int) (*models.PRMergeState, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return nil, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return nil, fmt.Errorf("get GitHub client: %w", err)
	}

	var pr *github.PullRequest
	for attempt := 0; attempt <= mergeabilityMaxRetries; attempt++ {
		if attempt > 0 {
			s.logger.Debug("Retrying mergeability check",
				zap.String("owner", owner),
				zap.String("repo", repo),
				zap.Int("pr", number),
				zap.Int("attempt", attempt),
				zap.Duration("delay", s.mergeRetryDelay))
			time.Sleep(s.mergeRetryDelay)
		}

		ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
		pr, _, err = client.PullRequests.Get(ctx, owner, repo, number)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("get PR #%d: %w", number, err)
		}

		if pr.Mergeable != nil {
			break
		}
	}

	if pr.Mergeable == nil {
		s.logger.Warn("Mergeability still unknown after retries",
			zap.String("owner", owner),
			zap.String("repo", repo),
			zap.Int("pr", number),
			zap.Int("retries", mergeabilityMaxRetries))
	}

	return &models.PRMergeState{
		Mergeable:  pr.Mergeable,
		BaseBranch: pr.GetBase().GetRef(),
	}, nil
}

// AddPRLabel adds a label to a pull request. If the label does not
// exist on the repository, GitHub creates it automatically.
func (s *GitHubServiceImpl) AddPRLabel(owner, repo string, number int, label string) error {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	_, _, err = client.Issues.AddLabelsToIssue(
		ctx, owner, repo, number, []string{label})
	if err != nil {
		return fmt.Errorf("add label %q to PR #%d: %w", label, number, err)
	}
	return nil
}

// RemovePRLabel removes a label from a pull request. Returns nil
// if the label is already absent (GitHub returns 404 in that case).
func (s *GitHubServiceImpl) RemovePRLabel(owner, repo string, number int, label string) error {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	_, err = client.Issues.RemoveLabelForIssue(
		ctx, owner, repo, number, label)
	if err != nil {
		var ghErr *github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("remove label %q from PR #%d: %w", label, number, err)
	}
	return nil
}

// HasPRLabel reports whether a pull request has the given label.
func (s *GitHubServiceImpl) HasPRLabel(owner, repo string, number int, label string) (bool, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return false, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return false, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	labels, _, err := client.Issues.ListLabelsByIssue(
		ctx, owner, repo, number, nil)
	if err != nil {
		return false, fmt.Errorf("list labels for PR #%d: %w", number, err)
	}

	for _, l := range labels {
		if l.GetName() == label {
			return true, nil
		}
	}
	return false, nil
}

// LastLabelRemoval returns the timestamp of the most recent removal of
// the given label from a pull request. Returns zero time if the label
// was never removed.
func (s *GitHubServiceImpl) LastLabelRemoval(owner, repo string, number int, label string) (time.Time, error) {
	installationID, err := s.getInstallationIDForRepo(owner, repo)
	if err != nil {
		return time.Time{}, fmt.Errorf("get installation ID: %w", err)
	}

	client, err := s.getInstallationGitHubClient(installationID)
	if err != nil {
		return time.Time{}, fmt.Errorf("get GitHub client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), githubAPITimeout)
	defer cancel()

	var latest time.Time
	opts := &github.ListOptions{PerPage: 100}
	for {
		events, resp, err := client.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return time.Time{}, fmt.Errorf("list events for PR #%d: %w", number, err)
		}
		for _, ev := range events {
			if ev.GetEvent() == "unlabeled" && ev.GetLabel().GetName() == label {
				if t := ev.GetCreatedAt().Time; t.After(latest) {
					latest = t
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}
