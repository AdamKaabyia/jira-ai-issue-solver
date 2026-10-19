// Package executortest provides test doubles for the executor package.
package executortest

import (
	"context"
	"time"

	"jira-ai-issue-solver/executor"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
)

// Compile-time checks.
var (
	_ executor.Executor        = (*Stub)(nil)
	_ executor.GitService      = (*StubGitService)(nil)
	_ executor.ProjectResolver = (*StubProjectResolver)(nil)
)

// Stub is a test double for [executor.Executor].
// Set the corresponding Func field to control each method's behavior.
// When a Func field is nil, the method returns zero values.
type Stub struct {
	ExecuteFunc func(ctx context.Context, job *jobmanager.Job) (jobmanager.JobResult, error)
}

func (s *Stub) Execute(ctx context.Context, job *jobmanager.Job) (jobmanager.JobResult, error) {
	if s.ExecuteFunc != nil {
		return s.ExecuteFunc(ctx, job)
	}
	return jobmanager.JobResult{}, nil
}

// StubGitService is a test double for [executor.GitService].
// Set the corresponding Func field to control each method's behavior.
// When a Func field is nil, the method returns zero values.
type StubGitService struct {
	SyncForkFunc                func(forkOwner, repo, branch string) error
	CreateBranchFunc            func(dir, name, baseBranch string) error
	SwitchBranchFunc            func(dir, name string) error
	RemoteBranchExistsFunc      func(owner, repo, branch string) (bool, error)
	DeleteRemoteBranchFunc      func(owner, repo, branch string) error
	HasChangesFunc              func(dir, baseBranch string) (bool, error)
	CommitChangesFunc           func(upstreamOwner, owner, repo, branch, message, dir, baseBranch string, coAuthor *models.Author, importExcludes []string, skipFileGuardrail bool) (string, error)
	StripRemoteAuthFunc         func(dir string) error
	RestoreRemoteAuthFunc       func(dir, owner, repo string) error
	FetchRemoteFunc             func(dir string) error
	SyncWithRemoteFunc          func(dir, branch string, importExcludes []string) error
	CreatePRFunc                func(params models.PRParams) (*models.PR, error)
	GetPRForBranchFunc          func(owner, repo, head string) (*models.PRDetails, error)
	GetPRCommentsFunc           func(owner, repo string, number int, since time.Time) ([]models.PRComment, error)
	ReplyToCommentFunc          func(owner, repo string, prNumber int, commentID int64, body string) error
	PostIssueCommentFunc        func(owner, repo string, prNumber int, body string) (int64, error)
	ListIssueCommentsFunc       func(owner, repo string, prNumber int) ([]models.IssueComment, error)
	UpdateIssueCommentFunc      func(owner, repo string, commentID int64, body string) error
	MergeBaseFunc               func(dir, branch, fetchURL string) ([]string, error)
	CloneImportFunc             func(url, destDir, ref string) error
	ListCheckRunsForRefFunc     func(owner, repo, ref string) ([]models.CheckRunFailure, bool, error)
	ListCheckRunAnnotationsFunc func(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error)
	GetFailedJobLogsFunc        func(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error)
	AddCommentReactionFunc      func(owner, repo string, comment models.PRComment, reaction string) error
	AddPRLabelFunc              func(owner, repo string, number int, label string) error
	RemovePRLabelFunc           func(owner, repo string, number int, label string) error
}

func (s *StubGitService) SyncFork(forkOwner, repo, branch string) error {
	if s.SyncForkFunc != nil {
		return s.SyncForkFunc(forkOwner, repo, branch)
	}
	return nil
}

func (s *StubGitService) CreateBranch(dir, name, baseBranch string) error {
	if s.CreateBranchFunc != nil {
		return s.CreateBranchFunc(dir, name, baseBranch)
	}
	return nil
}

func (s *StubGitService) SwitchBranch(dir, name string) error {
	if s.SwitchBranchFunc != nil {
		return s.SwitchBranchFunc(dir, name)
	}
	return nil
}

func (s *StubGitService) RemoteBranchExists(owner, repo, branch string) (bool, error) {
	if s.RemoteBranchExistsFunc != nil {
		return s.RemoteBranchExistsFunc(owner, repo, branch)
	}
	return false, nil
}

func (s *StubGitService) DeleteRemoteBranch(owner, repo, branch string) error {
	if s.DeleteRemoteBranchFunc != nil {
		return s.DeleteRemoteBranchFunc(owner, repo, branch)
	}
	return nil
}

func (s *StubGitService) HasChanges(dir, baseBranch string) (bool, error) {
	if s.HasChangesFunc != nil {
		return s.HasChangesFunc(dir, baseBranch)
	}
	return false, nil
}

func (s *StubGitService) CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string, coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error) {
	skip := len(skipFileGuardrail) > 0 && skipFileGuardrail[0]
	if s.CommitChangesFunc != nil {
		return s.CommitChangesFunc(upstreamOwner, owner, repo, branch, message, dir, baseBranch, coAuthor, importExcludes, skip)
	}
	return "", nil
}

func (s *StubGitService) StripRemoteAuth(dir string) error {
	if s.StripRemoteAuthFunc != nil {
		return s.StripRemoteAuthFunc(dir)
	}
	return nil
}

func (s *StubGitService) RestoreRemoteAuth(dir, owner, repo string) error {
	if s.RestoreRemoteAuthFunc != nil {
		return s.RestoreRemoteAuthFunc(dir, owner, repo)
	}
	return nil
}

func (s *StubGitService) FetchRemote(dir string) error {
	if s.FetchRemoteFunc != nil {
		return s.FetchRemoteFunc(dir)
	}
	return nil
}

func (s *StubGitService) SyncWithRemote(dir, branch string, importExcludes []string) error {
	if s.SyncWithRemoteFunc != nil {
		return s.SyncWithRemoteFunc(dir, branch, importExcludes)
	}
	return nil
}

func (s *StubGitService) CreatePR(params models.PRParams) (*models.PR, error) {
	if s.CreatePRFunc != nil {
		return s.CreatePRFunc(params)
	}
	return &models.PR{}, nil
}

func (s *StubGitService) GetPRForBranch(owner, repo, head string) (*models.PRDetails, error) {
	if s.GetPRForBranchFunc != nil {
		return s.GetPRForBranchFunc(owner, repo, head)
	}
	return &models.PRDetails{}, nil
}

func (s *StubGitService) GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
	if s.GetPRCommentsFunc != nil {
		return s.GetPRCommentsFunc(owner, repo, number, since)
	}
	return []models.PRComment{}, nil
}

func (s *StubGitService) ReplyToComment(owner, repo string, prNumber int, commentID int64, body string) error {
	if s.ReplyToCommentFunc != nil {
		return s.ReplyToCommentFunc(owner, repo, prNumber, commentID, body)
	}
	return nil
}

func (s *StubGitService) PostIssueComment(owner, repo string, prNumber int, body string) (int64, error) {
	if s.PostIssueCommentFunc != nil {
		return s.PostIssueCommentFunc(owner, repo, prNumber, body)
	}
	return 0, nil
}

func (s *StubGitService) ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error) {
	if s.ListIssueCommentsFunc != nil {
		return s.ListIssueCommentsFunc(owner, repo, prNumber)
	}
	return []models.IssueComment{}, nil
}

func (s *StubGitService) UpdateIssueComment(owner, repo string, commentID int64, body string) error {
	if s.UpdateIssueCommentFunc != nil {
		return s.UpdateIssueCommentFunc(owner, repo, commentID, body)
	}
	return nil
}

func (s *StubGitService) MergeBase(dir, branch, fetchURL string) ([]string, error) {
	if s.MergeBaseFunc != nil {
		return s.MergeBaseFunc(dir, branch, fetchURL)
	}
	return []string{}, nil
}

func (s *StubGitService) CloneImport(url, destDir, ref string) error {
	if s.CloneImportFunc != nil {
		return s.CloneImportFunc(url, destDir, ref)
	}
	return nil
}

func (s *StubGitService) ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error) {
	if s.ListCheckRunsForRefFunc != nil {
		return s.ListCheckRunsForRefFunc(owner, repo, ref)
	}
	return []models.CheckRunFailure{}, true, nil
}

func (s *StubGitService) ListCheckRunAnnotations(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error) {
	if s.ListCheckRunAnnotationsFunc != nil {
		return s.ListCheckRunAnnotationsFunc(owner, repo, checkRunID)
	}
	return []models.CheckAnnotation{}, nil
}

func (s *StubGitService) GetFailedJobLogs(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error) {
	if s.GetFailedJobLogsFunc != nil {
		return s.GetFailedJobLogsFunc(owner, repo, headSHA, maxBytesPerStep)
	}
	return map[string][]models.FailedStep{}, nil
}

func (s *StubGitService) AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error {
	if s.AddCommentReactionFunc != nil {
		return s.AddCommentReactionFunc(owner, repo, comment, reaction)
	}
	return nil
}

func (s *StubGitService) AddPRLabel(owner, repo string, number int, label string) error {
	if s.AddPRLabelFunc != nil {
		return s.AddPRLabelFunc(owner, repo, number, label)
	}
	return nil
}

func (s *StubGitService) RemovePRLabel(owner, repo string, number int, label string) error {
	if s.RemovePRLabelFunc != nil {
		return s.RemovePRLabelFunc(owner, repo, number, label)
	}
	return nil
}

// StubProjectResolver is a test double for [executor.ProjectResolver].
// Set the corresponding Func field to control each method's behavior.
// When a Func field is nil, the method returns zero values.
type StubProjectResolver struct {
	ResolveProjectFunc func(workItem models.WorkItem) (*models.ProjectSettings, error)
}

func (s *StubProjectResolver) ResolveProject(workItem models.WorkItem) (*models.ProjectSettings, error) {
	if s.ResolveProjectFunc != nil {
		return s.ResolveProjectFunc(workItem)
	}
	return &models.ProjectSettings{Repos: []models.RepoSettings{{}}}, nil
}
