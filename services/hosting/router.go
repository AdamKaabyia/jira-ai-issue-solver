// Package hosting provides a multi-provider router that satisfies all
// consumer-defined interfaces (executor.GitService, scanner.PRFetcher,
// scanner.PRLabeler, scanner.CIChecker, scanner.MergeabilityChecker,
// workspace.Cloner, recovery.GitService) by dispatching method calls
// to the correct VCS backend based on owner/repo.
//
// The router holds a mapping of owner/repo → provider built from the
// application config at startup. Methods that receive only a local
// directory path (CreateBranch, SwitchBranch, etc.) use a workspace
// registry populated during clone operations to resolve the provider.
package hosting

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"jira-ai-issue-solver/models"
)

// Provider defines the union of all methods needed by any consumer in
// the application. Both GitHubServiceImpl and GitLabServiceImpl satisfy
// this interface.
type Provider interface {
	// workspace.Cloner
	CloneRepository(repoURL, directory string) error

	// executor.GitService
	SyncFork(forkOwner, repo, branch string) error
	CreateBranch(dir, name, baseBranch string) error
	SwitchBranch(dir, name string) error
	RemoteBranchExists(owner, repo, branch string) (bool, error)
	DeleteRemoteBranch(owner, repo, branch string) error
	HasChanges(dir, baseBranch string) (bool, error)
	CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string,
		coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error)
	StripRemoteAuth(dir string) error
	RestoreRemoteAuth(dir, owner, repo string) error
	FetchRemote(dir string) error
	SyncWithRemote(dir, branch string, importExcludes []string) error
	CreatePR(params models.PRParams) (*models.PR, error)
	GetPRForBranch(owner, repo, head string) (*models.PRDetails, error)
	GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error)
	ReplyToComment(owner, repo string, prNumber int, commentID int64, body string) error
	PostIssueComment(owner, repo string, prNumber int, body string) error
	ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error)
	UpdateIssueComment(owner, repo string, commentID int64, body string) error
	AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error
	MergeBase(dir, branch, fetchURL string) ([]string, error)
	CloneImport(url, destDir, ref string) error
	ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error)
	ListCheckRunAnnotations(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error)
	GetFailedJobLogs(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error)
	AddPRLabel(owner, repo string, number int, label string) error
	RemovePRLabel(owner, repo string, number int, label string) error

	// scanner.PRFetcher (additional methods beyond executor.GitService)
	GetClosedPRForBranch(owner, repo, head string) (*models.PRDetails, error)
	GetMergedPRForBranch(owner, repo, head string) (*models.PRDetails, error)

	// scanner.PRLabeler (additional methods beyond executor.GitService)
	HasPRLabel(owner, repo string, number int, label string) (bool, error)
	LastLabelRemoval(owner, repo string, number int, label string) (time.Time, error)

	// scanner.MergeabilityChecker
	GetPRMergeability(owner, repo string, number int) (*models.PRMergeState, error)

	// recovery.GitService
	BranchHasCommits(owner, repo, branch, base string) (bool, error)
}

// Router dispatches method calls to the correct VCS backend based on
// owner/repo. For methods that receive only a directory path, it uses
// a workspace registry populated during CloneRepository calls.
type Router struct {
	// repoProviders maps "owner/repo" → Provider.
	repoProviders map[string]Provider

	// workspaceDirs maps directory path → Provider, populated on
	// CloneRepository so dir-based methods can resolve the provider.
	workspaceDirs   map[string]Provider
	workspaceDirsMu sync.RWMutex

	// fallback is used when no explicit mapping exists.
	fallback Provider
}

// NewRouter creates a Router from a repo→provider mapping. The fallback
// provider is used for repos not in the map (backward compatibility:
// pass the GitHub service as fallback).
func NewRouter(repoProviders map[string]Provider, fallback Provider) *Router {
	normalized := make(map[string]Provider, len(repoProviders))
	for key, provider := range repoProviders {
		normalized[strings.ToLower(key)] = provider
	}
	return &Router{
		repoProviders: normalized,
		workspaceDirs: make(map[string]Provider),
		fallback:      fallback,
	}
}

// RegisterWorkspace associates a directory path with a provider so
// dir-based methods can dispatch correctly.
func (r *Router) RegisterWorkspace(dir string, owner, repo string) {
	r.workspaceDirsMu.Lock()
	defer r.workspaceDirsMu.Unlock()
	r.workspaceDirs[dir] = r.forRepo(owner, repo)
}

func (r *Router) forRepo(owner, repo string) Provider {
	key := strings.ToLower(owner + "/" + repo)
	if p, ok := r.repoProviders[key]; ok {
		return p
	}
	return r.fallback
}

func (r *Router) forDir(dir string) Provider {
	r.workspaceDirsMu.RLock()
	defer r.workspaceDirsMu.RUnlock()
	if p, ok := r.workspaceDirs[dir]; ok {
		return p
	}
	return r.fallback
}

// --- workspace.Cloner ---

func (r *Router) CloneRepository(repoURL, directory string) error {
	owner, repo := extractOwnerRepo(repoURL)
	p := r.forRepo(owner, repo)
	err := p.CloneRepository(repoURL, directory)
	if err == nil {
		r.workspaceDirsMu.Lock()
		r.workspaceDirs[directory] = p
		r.workspaceDirsMu.Unlock()
	}
	return err
}

// --- Dir-based methods (use workspace registry) ---

func (r *Router) CreateBranch(dir, name, baseBranch string) error {
	return r.forDir(dir).CreateBranch(dir, name, baseBranch)
}

func (r *Router) SwitchBranch(dir, name string) error {
	return r.forDir(dir).SwitchBranch(dir, name)
}

func (r *Router) HasChanges(dir, baseBranch string) (bool, error) {
	return r.forDir(dir).HasChanges(dir, baseBranch)
}

func (r *Router) StripRemoteAuth(dir string) error {
	return r.forDir(dir).StripRemoteAuth(dir)
}

func (r *Router) FetchRemote(dir string) error {
	return r.forDir(dir).FetchRemote(dir)
}

func (r *Router) SyncWithRemote(dir, branch string, importExcludes []string) error {
	return r.forDir(dir).SyncWithRemote(dir, branch, importExcludes)
}

func (r *Router) MergeBase(dir, branch, fetchURL string) ([]string, error) {
	return r.forDir(dir).MergeBase(dir, branch, fetchURL)
}

func (r *Router) CloneImport(url, destDir, ref string) error {
	return r.forDir(destDir).CloneImport(url, destDir, ref)
}

// --- Owner/repo-based methods (use repo registry) ---

func (r *Router) SyncFork(forkOwner, repo, branch string) error {
	return r.forRepo(forkOwner, repo).SyncFork(forkOwner, repo, branch)
}

func (r *Router) RemoteBranchExists(owner, repo, branch string) (bool, error) {
	return r.forRepo(owner, repo).RemoteBranchExists(owner, repo, branch)
}

func (r *Router) DeleteRemoteBranch(owner, repo, branch string) error {
	return r.forRepo(owner, repo).DeleteRemoteBranch(owner, repo, branch)
}

func (r *Router) CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string,
	coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error) {
	return r.forRepo(owner, repo).CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch, coAuthor, importExcludes, skipFileGuardrail...)
}

func (r *Router) RestoreRemoteAuth(dir, owner, repo string) error {
	return r.forRepo(owner, repo).RestoreRemoteAuth(dir, owner, repo)
}

func (r *Router) CreatePR(params models.PRParams) (*models.PR, error) {
	return r.forRepo(params.Owner, params.Repo).CreatePR(params)
}

func (r *Router) GetPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	return r.forRepo(owner, repo).GetPRForBranch(owner, repo, head)
}

func (r *Router) GetClosedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	return r.forRepo(owner, repo).GetClosedPRForBranch(owner, repo, head)
}

func (r *Router) GetMergedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	return r.forRepo(owner, repo).GetMergedPRForBranch(owner, repo, head)
}

func (r *Router) GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
	return r.forRepo(owner, repo).GetPRComments(owner, repo, number, since)
}

func (r *Router) ReplyToComment(owner, repo string, prNumber int, commentID int64, body string) error {
	return r.forRepo(owner, repo).ReplyToComment(owner, repo, prNumber, commentID, body)
}

func (r *Router) PostIssueComment(owner, repo string, prNumber int, body string) error {
	return r.forRepo(owner, repo).PostIssueComment(owner, repo, prNumber, body)
}

func (r *Router) ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error) {
	return r.forRepo(owner, repo).ListIssueComments(owner, repo, prNumber)
}

func (r *Router) UpdateIssueComment(owner, repo string, commentID int64, body string) error {
	// commentID-based calls don't have owner/repo; fallback for now.
	return r.fallback.UpdateIssueComment(owner, repo, commentID, body)
}

func (r *Router) AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error {
	return r.forRepo(owner, repo).AddCommentReaction(owner, repo, comment, reaction)
}

func (r *Router) ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error) {
	return r.forRepo(owner, repo).ListCheckRunsForRef(owner, repo, ref)
}

func (r *Router) ListCheckRunAnnotations(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error) {
	return r.forRepo(owner, repo).ListCheckRunAnnotations(owner, repo, checkRunID)
}

func (r *Router) GetFailedJobLogs(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error) {
	return r.forRepo(owner, repo).GetFailedJobLogs(owner, repo, headSHA, maxBytesPerStep)
}

func (r *Router) AddPRLabel(owner, repo string, number int, label string) error {
	return r.forRepo(owner, repo).AddPRLabel(owner, repo, number, label)
}

func (r *Router) RemovePRLabel(owner, repo string, number int, label string) error {
	return r.forRepo(owner, repo).RemovePRLabel(owner, repo, number, label)
}

func (r *Router) HasPRLabel(owner, repo string, number int, label string) (bool, error) {
	return r.forRepo(owner, repo).HasPRLabel(owner, repo, number, label)
}

func (r *Router) LastLabelRemoval(owner, repo string, number int, label string) (time.Time, error) {
	return r.forRepo(owner, repo).LastLabelRemoval(owner, repo, number, label)
}

func (r *Router) GetPRMergeability(owner, repo string, number int) (*models.PRMergeState, error) {
	return r.forRepo(owner, repo).GetPRMergeability(owner, repo, number)
}

func (r *Router) BranchHasCommits(owner, repo, branch, base string) (bool, error) {
	return r.forRepo(owner, repo).BranchHasCommits(owner, repo, branch, base)
}

// extractOwnerRepo parses a clone URL into owner and repo.
// Handles https://host/owner/repo.git and git@host:owner/repo.git formats.
func extractOwnerRepo(repoURL string) (string, string) {
	repoURL = strings.TrimSuffix(repoURL, ".git")

	// SSH format: git@host:owner/repo
	if strings.Contains(repoURL, ":") && !strings.Contains(repoURL, "://") {
		parts := strings.SplitN(repoURL, ":", 2)
		if len(parts) == 2 {
			pathParts := strings.Split(parts[1], "/")
			if len(pathParts) >= 2 {
				return strings.Join(pathParts[:len(pathParts)-1], "/"), pathParts[len(pathParts)-1]
			}
		}
	}

	// HTTPS format: https://host/owner/[subgroups/]repo
	parts := strings.Split(repoURL, "/")
	if len(parts) >= 5 {
		repo := parts[len(parts)-1]
		owner := strings.Join(parts[3:len(parts)-1], "/")
		return owner, repo
	}

	return "", fmt.Sprintf("unknown-%s", repoURL)
}
