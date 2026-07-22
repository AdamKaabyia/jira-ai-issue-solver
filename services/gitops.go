// Package services provides infrastructure implementations for VCS and issue tracker operations.
//
// gitops.go contains provider-agnostic git operations shared by both
// GitHubServiceImpl and GitLabServiceImpl. These are pure local git
// commands that do not require any platform-specific API calls.
package services

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"jira-ai-issue-solver/models"
)

// GitOps provides provider-agnostic local git operations. Both
// GitHubServiceImpl and GitLabServiceImpl embed this struct to
// share implementations of CreateBranch, SwitchBranch, HasChanges,
// StripRemoteAuth, FetchRemote, SyncWithRemote, MergeBase, and
// CloneImport.
type GitOps struct {
	executor models.CommandExecutor
	logger   *zap.Logger
}

// NewGitOps creates a GitOps instance with the given command executor and logger.
func NewGitOps(executor models.CommandExecutor, logger *zap.Logger) *GitOps {
	return &GitOps{
		executor: executor,
		logger:   logger,
	}
}

// CreateBranch creates a new git branch in the workspace and switches to it.
// baseBranch is the branch to fork from (e.g., "main"). If the branch
// already exists locally it is deleted and recreated.
func (g *GitOps) CreateBranch(directory, branchName, baseBranch string) error {
	debugEnabled := g.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "CreateBranch")

	resetCmd := newGitCommand(g.executor("git", "reset", "--hard", "HEAD"), directory, debugEnabled, true)
	if err := resetCmd.run(); err != nil {
		g.logger.Warn("git reset --hard HEAD failed (non-fatal)",
			fn, zap.Error(err), zap.String("stderr", resetCmd.getStderr()))
	}

	cmd := newGitCommand(g.executor("git", "fetch", "origin"), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to fetch origin: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git fetch origin", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "checkout", baseBranch), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to checkout target branch %s: %w, stderr: %s", baseBranch, err, cmd.getStderr())
	}
	g.logger.Debug("git checkout", fn, zap.String("branch", baseBranch), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "reset", "--hard", "origin/"+baseBranch), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to reset to latest commit on target branch %s: %w, stderr: %s", baseBranch, err, cmd.getStderr())
	}
	g.logger.Debug("git reset --hard", fn, zap.String("ref", "origin/"+baseBranch), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName), directory, debugEnabled, true)
	if err := cmd.run(); err == nil {
		g.logger.Info("Branch already exists locally, deleting it", zap.String("branchName", branchName))
		cmd = newGitCommand(g.executor("git", "branch", "-D", branchName), directory, debugEnabled, true)
		if err := cmd.run(); err != nil {
			return fmt.Errorf("failed to delete existing branch %s: %w, stderr: %s", branchName, err, cmd.getStderr())
		}
		g.logger.Debug("git branch -D", fn, zap.String("branch", branchName), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))
	}

	cmd = newGitCommand(g.executor("git", "checkout", "-b", branchName), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to create branch: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git checkout", fn, zap.String("operation", "-b"), zap.String("branch", branchName), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	return nil
}

// SwitchBranch switches to an existing branch. Used when a workspace
// is reused on retry or when checking out a PR branch for feedback
// processing.
func (g *GitOps) SwitchBranch(directory, branchName string) error {
	debugEnabled := g.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "SwitchBranch")

	resetCmd := newGitCommand(g.executor("git", "reset", "--hard", "HEAD"), directory, debugEnabled, true)
	if err := resetCmd.run(); err != nil {
		g.logger.Warn("git reset --hard HEAD failed (non-fatal)",
			fn, zap.Error(err), zap.String("stderr", resetCmd.getStderr()))
	}

	cmd := newGitCommand(g.executor("git", "fetch", "origin"), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to fetch origin: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git fetch origin", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "checkout", branchName), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to checkout branch %s: %w, stderr: %s", branchName, err, cmd.getStderr())
	}
	g.logger.Debug("git checkout", fn, zap.String("branch", branchName), zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	return nil
}

// HasChanges reports whether the workspace has uncommitted changes or
// unpushed commits relative to the remote branch.
func (g *GitOps) HasChanges(directory, baseBranch string) (bool, error) {
	fn := zap.String("function", "HasChanges")

	hasWT, err := g.hasWorkingTreeChanges(directory, fn)
	if err != nil {
		return false, err
	}
	if hasWT {
		return true, nil
	}

	return g.hasUnpushedCommits(directory, baseBranch, fn)
}

func (g *GitOps) hasWorkingTreeChanges(directory string, fn zapcore.Field) (bool, error) {
	cmd := newGitCommand(g.executor("git", "status", "--porcelain"), directory, true, true)
	if err := cmd.run(); err != nil {
		return false, fmt.Errorf("failed to check git status: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git status --porcelain", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))
	return cmd.hasStdout(), nil
}

func (g *GitOps) hasUnpushedCommits(directory, baseBranch string, fn zapcore.Field) (bool, error) {
	remoteCmd := newGitCommand(g.executor("git", "remote", "get-url", "origin"), directory, false, false)
	if err := remoteCmd.run(); err != nil {
		g.logger.Debug("No origin remote configured", fn)
		return false, nil
	}

	branchCmd := newGitCommand(g.executor("git", "rev-parse", "--abbrev-ref", "HEAD"), directory, true, true)
	if err := branchCmd.run(); err != nil {
		return false, fmt.Errorf("failed to get current branch: %w, stderr: %s", err, branchCmd.getStderr())
	}

	branchName := strings.TrimSpace(branchCmd.getStdout())
	if branchName == "" {
		return false, fmt.Errorf("unable to determine current branch")
	}
	g.logger.Debug("Current branch", fn, zap.String("branch", branchName))

	remoteRef := fmt.Sprintf("origin/%s", branchName)
	remoteExistsCmd := newGitCommand(g.executor("git", "rev-parse", "--verify", remoteRef), directory, false, false)
	if err := remoteExistsCmd.run(); err != nil {
		remoteRef = fmt.Sprintf("origin/%s", baseBranch)
		g.logger.Debug("Remote branch does not exist, comparing against target branch", fn,
			zap.String("branch", branchName),
			zap.String("targetRef", remoteRef))
	}

	logCmd := newGitCommand(g.executor("git", "log", fmt.Sprintf("%s..HEAD", remoteRef), "--oneline"), directory, true, true)
	if err := logCmd.run(); err != nil {
		return false, fmt.Errorf("failed to check unpushed commits: %w, stderr: %s", err, logCmd.getStderr())
	}
	g.logger.Debug("git log ref..HEAD", fn,
		zap.String("ref", remoteRef),
		zap.String("stdout", logCmd.getStdout()),
		zap.String("stderr", logCmd.getStderr()))

	return logCmd.hasStdout(), nil
}

// StageAndCommitLocal ensures all working tree and staged changes are
// committed locally. Normalizes AI session output (committed, staged,
// or unstaged) into a single committed state.
func (g *GitOps) StageAndCommitLocal(directory string, fn zapcore.Field) error {
	statusCmd := newGitCommand(g.executor("git", "status", "--porcelain"), directory, true, true)
	if err := statusCmd.run(); err != nil {
		return fmt.Errorf("git status: %w, stderr: %s", err, statusCmd.getStderr())
	}
	if !statusCmd.hasStdout() {
		return nil
	}

	g.logger.Debug("Staging uncommitted changes", fn)

	addCmd := newGitCommand(g.executor("git", "add", "-A"), directory, false, true)
	if err := addCmd.run(); err != nil {
		return fmt.Errorf("git add -A: %w, stderr: %s", err, addCmd.getStderr())
	}

	commitCmd := newGitCommand(
		g.executor("git", "commit", "-m", "AI changes (local only)"),
		directory, false, true)
	if err := commitCmd.run(); err != nil {
		return fmt.Errorf("git commit: %w, stderr: %s", err, commitCmd.getStderr())
	}

	g.logger.Debug("Committed local changes", fn)
	return nil
}

// StripRemoteAuth removes authentication credentials from the
// workspace's origin remote URL, preventing push operations.
func (g *GitOps) StripRemoteAuth(directory string) error {
	cmd := newGitCommand(g.executor("git", "remote", "get-url", "origin"), directory, true, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("get remote URL: %w, stderr: %s", err, cmd.getStderr())
	}

	rawURL := strings.TrimSpace(cmd.getStdout())
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse remote URL: %w", err)
	}
	parsed.User = nil

	setCmd := newGitCommand(
		g.executor("git", "remote", "set-url", "origin", parsed.String()),
		directory, false, false)
	if err := setCmd.run(); err != nil {
		return fmt.Errorf("strip remote auth: %w, stderr: %s", err, setCmd.getStderr())
	}

	g.logger.Debug("Stripped remote auth", zap.String("directory", directory))
	return nil
}

// FetchRemote fetches all refs from the origin remote.
func (g *GitOps) FetchRemote(directory string) error {
	debugEnabled := g.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "FetchRemote")

	fetchCmd := newGitCommand(g.executor("git", "fetch", "origin"), directory, debugEnabled, true)
	if err := fetchCmd.run(); err != nil {
		return fmt.Errorf("failed to fetch from origin: %w, stderr: %s", err, fetchCmd.getStderr())
	}
	g.logger.Debug("git fetch origin", fn, zap.String("stdout", fetchCmd.getStdout()), zap.String("stderr", fetchCmd.getStderr()))

	return nil
}

// SyncWithRemote reconciles the local workspace with the remote branch
// after an API-created commit. importExcludes lists directories to
// preserve across the hard reset.
func (g *GitOps) SyncWithRemote(directory, branch string, importExcludes []string) error {
	fn := zap.String("function", "SyncWithRemote")

	excludes := mergeExcludes(importExcludes)
	preserved := g.preserveExcludedDirs(directory, excludes, fn)

	if err := g.FetchRemote(directory); err != nil {
		return err
	}

	debugEnabled := g.logger.Core().Enabled(zapcore.DebugLevel)
	ref := "origin/" + branch
	resetCmd := newGitCommand(g.executor("git", "reset", "--hard", ref), directory, debugEnabled, true)
	if err := resetCmd.run(); err != nil {
		return fmt.Errorf("failed to reset to %s: %w, stderr: %s", ref, err, resetCmd.getStderr())
	}
	g.logger.Debug("git reset --hard", fn, zap.String("ref", ref), zap.String("stdout", resetCmd.getStdout()), zap.String("stderr", resetCmd.getStderr()))

	g.restoreExcludedDirs(directory, preserved, fn)

	return nil
}

// MergeBase merges a base branch into the current branch in the
// workspace. When fetchURL is non-empty, the base branch is fetched
// from that URL. On conflict, returns ErrMergeConflict and the list
// of conflicted file paths.
func (g *GitOps) MergeBase(dir, branch, fetchURL string) ([]string, error) {
	remote := "origin"
	mergeRef := "origin/" + branch
	if fetchURL != "" {
		remote = fetchURL
		mergeRef = "FETCH_HEAD"
	}
	fetchCmd := g.executor("git", "fetch", remote, branch)
	fetchCmd.Dir = dir
	if _, err := fetchCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git fetch %s %s: %w", remote, branch, err)
	}

	mergeOut, err := g.runMerge(dir, mergeRef)
	if blockers := parseUntrackedBlockers(string(mergeOut)); err != nil && len(blockers) > 0 {
		g.logger.Info("Removing untracked files blocking merge",
			zap.Strings("files", blockers))
		args := append([]string{"clean", "-f", "--"}, blockers...)
		cleanCmd := g.executor("git", args...)
		cleanCmd.Dir = dir
		if cleanOut, cleanErr := cleanCmd.CombinedOutput(); cleanErr != nil {
			return nil, fmt.Errorf("git clean blocking paths: %w, output: %s", cleanErr, string(cleanOut))
		}
		mergeOut, err = g.runMerge(dir, mergeRef)
	}
	if err != nil {
		conflictFiles := g.listConflictFiles(dir)
		if len(conflictFiles) > 0 {
			return conflictFiles, fmt.Errorf("%w: conflicted files: %v", ErrMergeConflict, conflictFiles)
		}
		return nil, fmt.Errorf("git merge %s failed: %w, output: %s", mergeRef, err, string(mergeOut))
	}

	return []string{}, nil
}

func (g *GitOps) runMerge(dir, mergeRef string) ([]byte, error) {
	mergeCmd := g.executor("git", "merge", "--no-edit", mergeRef)
	mergeCmd.Dir = dir
	out, err := mergeCmd.CombinedOutput()
	return out, err
}

func (g *GitOps) listConflictFiles(dir string) []string {
	cmd := g.executor("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return []string{}
	}

	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		if xy == "UU" || xy == "AA" || xy == "DD" ||
			xy == "AU" || xy == "UA" || xy == "DU" || xy == "UD" {
			files = append(files, strings.TrimSpace(line[3:]))
		}
	}
	if files == nil {
		files = []string{}
	}
	return files
}

// CloneImport clones an auxiliary repository into destDir. If ref is
// non-empty, that branch/tag/commit is checked out after cloning.
func (g *GitOps) CloneImport(repoURL, destDir, ref string) error {
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, repoURL, destDir)

	cmd := g.executor("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s into %s: %w, stderr: %s", repoURL, destDir, err, stderr.String())
	}

	g.logger.Debug("Cloned import repo",
		zap.String("url", repoURL),
		zap.String("dest", destDir),
		zap.String("ref", ref))

	return nil
}

func (g *GitOps) preserveExcludedDirs(directory string, excludes []string, fn zapcore.Field) []string {
	var preserved []string
	for _, prefix := range excludes {
		dir := strings.TrimSuffix(prefix, "/")
		src := filepath.Join(directory, dir)
		dst := filepath.Join(directory, dir+".preserve")

		if _, err := os.Stat(src); err != nil {
			continue
		}
		_ = os.RemoveAll(dst)
		if err := os.Rename(src, dst); err != nil {
			g.logger.Warn("Failed to preserve directory", fn,
				zap.String("dir", dir), zap.Error(err))
			continue
		}
		preserved = append(preserved, dir)
	}
	return preserved
}

func (g *GitOps) restoreExcludedDirs(directory string, preserved []string, fn zapcore.Field) {
	for _, dir := range preserved {
		src := filepath.Join(directory, dir+".preserve")
		dst := filepath.Join(directory, dir)
		if err := os.Rename(src, dst); err != nil {
			g.logger.Warn("Failed to restore directory", fn,
				zap.String("dir", dir), zap.Error(err))
		}
	}
}

// SetRemoteURL sets the origin remote URL for a workspace directory.
func (g *GitOps) SetRemoteURL(directory, remoteURL string) error {
	cmd := newGitCommand(
		g.executor("git", "remote", "set-url", "origin", remoteURL),
		directory, false, false)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("set remote URL: %w", err)
	}
	return nil
}

// ConfigureUser sets git user.name and user.email in the workspace.
func (g *GitOps) ConfigureUser(directory, name, email string) error {
	debugEnabled := g.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "ConfigureUser")

	cmd := newGitCommand(g.executor("git", "config", "user.name", name), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git user name: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git config user.name", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "config", "user.email", email), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git user email: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git config user.email", fn, zap.String("stderr", cmd.getStderr()))

	return nil
}

// ConfigureSSHSigning configures SSH key signing in the workspace.
// Does nothing if sshKeyPath is empty or the key file does not exist.
func (g *GitOps) ConfigureSSHSigning(directory, sshKeyPath string) error {
	if sshKeyPath == "" {
		return nil
	}

	exists, err := fileExists(sshKeyPath)
	if err != nil {
		return fmt.Errorf("failed to check if SSH key file exists: %w", err)
	}
	if !exists {
		g.logger.Info("SSH signing not configured for repository")
		return nil
	}

	debugEnabled := g.logger.Core().Enabled(zapcore.DebugLevel)
	fn := zap.String("function", "ConfigureSSHSigning")

	cmd := newGitCommand(g.executor("git", "config", "gpg.format", "ssh"), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git gpg format: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git config gpg.format", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "config", "user.signingkey", sshKeyPath), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to configure git ssh signing key: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git config user.signingkey", fn, zap.String("stdout", cmd.getStdout()), zap.String("stderr", cmd.getStderr()))

	cmd = newGitCommand(g.executor("git", "config", "commit.gpgsign", "true"), directory, debugEnabled, true)
	if err := cmd.run(); err != nil {
		return fmt.Errorf("failed to enable git commit signing: %w, stderr: %s", err, cmd.getStderr())
	}
	g.logger.Debug("git config commit.gpgsign", fn)

	return nil
}
