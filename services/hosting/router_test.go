package hosting

import (
	"testing"
	"time"

	"jira-ai-issue-solver/models"
)

// stubProvider is a minimal test double for the Provider interface.
type stubProvider struct {
	name string
	// Track which methods were called.
	calls []string
}

func (p *stubProvider) CloneRepository(repoURL, directory string) error {
	p.calls = append(p.calls, "CloneRepository")
	return nil
}
func (p *stubProvider) SyncFork(forkOwner, repo, branch string) error {
	p.calls = append(p.calls, "SyncFork")
	return nil
}
func (p *stubProvider) CreateBranch(dir, name, baseBranch string) error {
	p.calls = append(p.calls, "CreateBranch")
	return nil
}
func (p *stubProvider) SwitchBranch(dir, name string) error {
	p.calls = append(p.calls, "SwitchBranch")
	return nil
}
func (p *stubProvider) RemoteBranchExists(owner, repo, branch string) (bool, error) {
	p.calls = append(p.calls, "RemoteBranchExists")
	return false, nil
}
func (p *stubProvider) DeleteRemoteBranch(owner, repo, branch string) error {
	p.calls = append(p.calls, "DeleteRemoteBranch")
	return nil
}
func (p *stubProvider) HasChanges(dir, baseBranch string) (bool, error) {
	p.calls = append(p.calls, "HasChanges")
	return false, nil
}
func (p *stubProvider) CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string, coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error) {
	p.calls = append(p.calls, "CommitChanges")
	return "sha123", nil
}
func (p *stubProvider) StripRemoteAuth(dir string) error {
	p.calls = append(p.calls, "StripRemoteAuth")
	return nil
}
func (p *stubProvider) RestoreRemoteAuth(dir, owner, repo string) error {
	p.calls = append(p.calls, "RestoreRemoteAuth")
	return nil
}
func (p *stubProvider) FetchRemote(dir string) error {
	p.calls = append(p.calls, "FetchRemote")
	return nil
}
func (p *stubProvider) SyncWithRemote(dir, branch string, importExcludes []string) error {
	p.calls = append(p.calls, "SyncWithRemote")
	return nil
}
func (p *stubProvider) CreatePR(params models.PRParams) (*models.PR, error) {
	p.calls = append(p.calls, "CreatePR")
	return &models.PR{Number: 1}, nil
}
func (p *stubProvider) GetPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	p.calls = append(p.calls, "GetPRForBranch")
	return nil, nil
}
func (p *stubProvider) GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
	p.calls = append(p.calls, "GetPRComments")
	return []models.PRComment{}, nil
}
func (p *stubProvider) ReplyToComment(owner, repo string, prNumber int, commentID int64, body string) error {
	p.calls = append(p.calls, "ReplyToComment")
	return nil
}
func (p *stubProvider) PostIssueComment(owner, repo string, prNumber int, body string) error {
	p.calls = append(p.calls, "PostIssueComment")
	return nil
}
func (p *stubProvider) ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error) {
	p.calls = append(p.calls, "ListIssueComments")
	return []models.IssueComment{}, nil
}
func (p *stubProvider) UpdateIssueComment(owner, repo string, commentID int64, body string) error {
	p.calls = append(p.calls, "UpdateIssueComment")
	return nil
}
func (p *stubProvider) AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error {
	p.calls = append(p.calls, "AddCommentReaction")
	return nil
}
func (p *stubProvider) MergeBase(dir, branch, fetchURL string) ([]string, error) {
	p.calls = append(p.calls, "MergeBase")
	return []string{}, nil
}
func (p *stubProvider) CloneImport(url, destDir, ref string) error {
	p.calls = append(p.calls, "CloneImport")
	return nil
}
func (p *stubProvider) ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error) {
	p.calls = append(p.calls, "ListCheckRunsForRef")
	return []models.CheckRunFailure{}, true, nil
}
func (p *stubProvider) ListCheckRunAnnotations(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error) {
	p.calls = append(p.calls, "ListCheckRunAnnotations")
	return []models.CheckAnnotation{}, nil
}
func (p *stubProvider) GetFailedJobLogs(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error) {
	p.calls = append(p.calls, "GetFailedJobLogs")
	return map[string][]models.FailedStep{}, nil
}
func (p *stubProvider) AddPRLabel(owner, repo string, number int, label string) error {
	p.calls = append(p.calls, "AddPRLabel")
	return nil
}
func (p *stubProvider) RemovePRLabel(owner, repo string, number int, label string) error {
	p.calls = append(p.calls, "RemovePRLabel")
	return nil
}
func (p *stubProvider) GetClosedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	p.calls = append(p.calls, "GetClosedPRForBranch")
	return nil, nil
}
func (p *stubProvider) GetMergedPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	p.calls = append(p.calls, "GetMergedPRForBranch")
	return nil, nil
}
func (p *stubProvider) HasPRLabel(owner, repo string, number int, label string) (bool, error) {
	p.calls = append(p.calls, "HasPRLabel")
	return false, nil
}
func (p *stubProvider) LastLabelRemoval(owner, repo string, number int, label string) (time.Time, error) {
	p.calls = append(p.calls, "LastLabelRemoval")
	return time.Time{}, nil
}
func (p *stubProvider) GetPRMergeability(owner, repo string, number int) (*models.PRMergeState, error) {
	p.calls = append(p.calls, "GetPRMergeability")
	return nil, nil
}
func (p *stubProvider) BranchHasCommits(owner, repo, branch, base string) (bool, error) {
	p.calls = append(p.calls, "BranchHasCommits")
	return false, nil
}

func TestRouter_RoutesToCorrectProvider(t *testing.T) {
	github := &stubProvider{name: "github"}
	gitlab := &stubProvider{name: "gitlab"}

	repoProviders := map[string]Provider{
		"gitlab-org/repo": gitlab,
		"github-org/repo": github,
	}

	router := NewRouter(repoProviders, github)

	// API-based call should route to gitlab for gitlab-org/repo.
	router.GetPRForBranch("gitlab-org", "repo", "feature")
	if len(gitlab.calls) != 1 || gitlab.calls[0] != "GetPRForBranch" {
		t.Errorf("expected gitlab to receive GetPRForBranch, got %v", gitlab.calls)
	}
	if len(github.calls) != 0 {
		t.Errorf("expected github to receive no calls, got %v", github.calls)
	}

	// API-based call should route to github for github-org/repo.
	router.GetPRForBranch("github-org", "repo", "feature")
	if len(github.calls) != 1 || github.calls[0] != "GetPRForBranch" {
		t.Errorf("expected github to receive GetPRForBranch, got %v", github.calls)
	}
}

func TestRouter_FallbackToDefault(t *testing.T) {
	github := &stubProvider{name: "github"}
	gitlab := &stubProvider{name: "gitlab"}

	repoProviders := map[string]Provider{
		"gitlab-org/repo": gitlab,
	}

	router := NewRouter(repoProviders, github)

	// Unknown repo should fall back to the default (github).
	router.GetPRForBranch("unknown-org", "unknown-repo", "feature")
	if len(github.calls) != 1 || github.calls[0] != "GetPRForBranch" {
		t.Errorf("expected github (fallback) to receive call, got %v", github.calls)
	}
}

func TestRouter_DirBasedMethodsUseWorkspaceRegistry(t *testing.T) {
	github := &stubProvider{name: "github"}
	gitlab := &stubProvider{name: "gitlab"}

	repoProviders := map[string]Provider{
		"gitlab-org/repo": gitlab,
	}

	router := NewRouter(repoProviders, github)

	// Register a workspace directory with the gitlab provider.
	router.RegisterWorkspace("/tmp/workspaces/ticket-1/repo", "gitlab-org", "repo")

	// Dir-based call should route to gitlab.
	router.CreateBranch("/tmp/workspaces/ticket-1/repo", "feature", "main")
	if len(gitlab.calls) != 1 || gitlab.calls[0] != "CreateBranch" {
		t.Errorf("expected gitlab to receive CreateBranch, got %v", gitlab.calls)
	}

	// Unknown dir should fall back.
	router.CreateBranch("/tmp/workspaces/ticket-2/repo", "feature", "main")
	if len(github.calls) != 1 || github.calls[0] != "CreateBranch" {
		t.Errorf("expected github (fallback) for unknown dir, got %v", github.calls)
	}
}

func TestRouter_CloneRegistersWorkspace(t *testing.T) {
	github := &stubProvider{name: "github"}
	gitlab := &stubProvider{name: "gitlab"}

	repoProviders := map[string]Provider{
		"gitlab-org/repo": gitlab,
	}

	router := NewRouter(repoProviders, github)

	// Clone registers the workspace directory.
	router.CloneRepository("https://gitlab.com/gitlab-org/repo.git", "/tmp/workspaces/ws1")

	if len(gitlab.calls) != 1 || gitlab.calls[0] != "CloneRepository" {
		t.Errorf("expected gitlab to receive CloneRepository, got %v", gitlab.calls)
	}

	// Subsequent dir-based call should route to gitlab.
	router.HasChanges("/tmp/workspaces/ws1", "main")
	if len(gitlab.calls) != 2 || gitlab.calls[1] != "HasChanges" {
		t.Errorf("expected gitlab to receive HasChanges after clone, got %v", gitlab.calls)
	}
}

func TestRouter_CaseInsensitiveRouting(t *testing.T) {
	github := &stubProvider{name: "github"}
	gitlab := &stubProvider{name: "gitlab"}

	repoProviders := map[string]Provider{
		"GitLab-Org/Repo": gitlab,
	}

	router := NewRouter(repoProviders, github)

	// Lowercase lookup should still find the provider.
	router.GetPRForBranch("gitlab-org", "repo", "feature")
	if len(gitlab.calls) != 1 {
		t.Errorf("expected case-insensitive match to gitlab, got %v", gitlab.calls)
	}
}

func TestExtractOwnerRepo(t *testing.T) {
	tests := []struct {
		url       string
		wantOwner string
		wantRepo  string
	}{
		{"https://github.com/org/repo.git", "org", "repo"},
		{"https://gitlab.com/group/subgroup/repo.git", "group/subgroup", "repo"},
		{"git@github.com:org/repo.git", "org", "repo"},
		{"https://gitlab.cee.redhat.com/a/b/c.git", "a/b", "c"},
	}

	for _, tt := range tests {
		owner, repo := extractOwnerRepo(tt.url)
		if owner != tt.wantOwner {
			t.Errorf("extractOwnerRepo(%s): owner=%q, want %q", tt.url, owner, tt.wantOwner)
		}
		if repo != tt.wantRepo {
			t.Errorf("extractOwnerRepo(%s): repo=%q, want %q", tt.url, repo, tt.wantRepo)
		}
	}
}
