package executor_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/executor"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/services"
)

func TestExecuteMerge_CleanMerge(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()

	var mergedBranch string
	d.git.MergeBaseFunc = func(_, branch, _ string) ([]string, error) {
		mergedBranch = branch
		return []string{}, nil
	}

	committed := false
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, skip bool) (string, error) {
		if !skip {
			t.Error("expected skipFileGuardrail=true for merge commit")
		}
		committed = true
		return "abc123", nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")

	result, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mergedBranch != "main" {
		t.Errorf("expected merge of main, got %s", mergedBranch)
	}
	if !committed {
		t.Error("expected commit after clean merge")
	}
	if result.PRURL == "" {
		t.Error("expected PR URL in result")
	}
}

func TestExecuteMerge_CleanMerge_NoChanges(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()

	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return false, nil
	}

	committed := false
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		committed = true
		return "abc123", nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")

	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if committed {
		t.Error("should not commit when no changes after merge")
	}
}

func TestExecuteMerge_ConflictInvokesAI(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()

	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{"file.go"}, fmt.Errorf("%w: CONFLICT in file.go", services.ErrMergeConflict)
	}

	aiInvoked := false
	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, _ []string) (string, int, error) {
		aiInvoked = true
		return "", 0, nil
	}

	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, skip bool) (string, error) {
		if !skip {
			t.Error("expected skipFileGuardrail=true for merge commit")
		}
		return "def456", nil
	}

	taskWritten := false
	d.taskWriter.WriteMergeConflictTaskFunc = func(_ models.PRDetails, _ []string, _, _ string) error {
		taskWritten = true
		return nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")

	writeSessionOutput(t, d.wsDir, executor.SessionOutput{CostUSD: 0.50})

	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !aiInvoked {
		t.Error("expected AI container to be invoked for conflict resolution")
	}
	if !taskWritten {
		t.Error("expected merge conflict task file to be written")
	}
}

func TestExecuteMerge_ConflictAINoChanges(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()

	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{"file.go"}, fmt.Errorf("%w: CONFLICT in file.go", services.ErrMergeConflict)
	}

	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return false, nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")

	writeSessionOutput(t, d.wsDir, executor.SessionOutput{})

	_, err := p.Execute(context.Background(), job)
	if err == nil {
		t.Fatal("expected error when AI produces no changes")
	}
}

func TestExecuteMerge_NoPRFound(t *testing.T) {
	d := newTestDeps(t)

	d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")

	_, err := p.Execute(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "no open PR found") {
		t.Fatalf("expected 'no open PR found' error, got %v", err)
	}
}

func TestExecuteMerge_MultiRepo_CleanMerge(t *testing.T) {
	d := newMultiRepoTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()
	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}

	committed := 0
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, skip bool) (string, error) {
		if !skip {
			t.Error("expected skipFileGuardrail=true for merge commit")
		}
		committed++
		return "abc123", nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), mergeJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if committed == 0 {
		t.Error("expected at least one commit for multi-repo clean merge")
	}
	if result.PRURL == "" {
		t.Error("expected PR URL in result")
	}
}

func TestExecuteMerge_MultiRepo_ConflictRestoresAuth(t *testing.T) {
	d := newMultiRepoTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()
	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{"conflict.go"}, fmt.Errorf("%w: CONFLICT", services.ErrMergeConflict)
	}

	stripped := make(map[string]bool)
	restored := make(map[string]bool)
	d.git.StripRemoteAuthFunc = func(dir string) error {
		stripped[dir] = true
		return nil
	}
	d.git.RestoreRemoteAuthFunc = func(dir, _, _ string) error {
		restored[dir] = true
		return nil
	}

	// Simulate AI failure after auth is stripped.
	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, _ []string) (string, int, error) {
		return "", 1, fmt.Errorf("container exec failed")
	}

	p := d.pipeline(t)
	writeSessionOutput(t, d.wsDir, executor.SessionOutput{})

	_, err := p.Execute(context.Background(), mergeJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error from failed container exec")
	}

	// Verify auth was restored for all stripped repos via defer.
	for dir := range stripped {
		if !restored[dir] {
			t.Errorf("auth not restored for %s after failure", dir)
		}
	}
}

func TestExecuteMerge_MultiRepo_NoPRs(t *testing.T) {
	d := newMultiRepoTestDeps(t)
	d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), mergeJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error when no PRs found in any repo")
	}
}

func TestExecuteMerge_ForkMode_PassesUpstreamURL(t *testing.T) {
	d := newTestDeps(t)

	d.projects.ResolveProjectFunc = func(_ models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{{
				Owner:      "upstream-org",
				Repo:       "backend",
				CloneURL:   "https://github.com/upstream-org/backend.git",
				BaseBranch: "main",
			}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "fork-bot",
		}, nil
	}

	d.git.GetPRForBranchFunc = mergePRFunc()

	var receivedURL string
	d.git.MergeBaseFunc = func(_, _, fetchURL string) ([]string, error) {
		receivedURL = fetchURL
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, skip bool) (string, error) {
		if !skip {
			t.Error("expected skipFileGuardrail=true for merge commit")
		}
		return "abc123", nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), mergeJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedURL != "https://github.com/upstream-org/backend.git" {
		t.Errorf("MergeBase fetchURL = %q, want upstream clone URL", receivedURL)
	}
}

func TestExecuteMerge_NoFork_PassesEmptyURL(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()

	var receivedURL string
	d.git.MergeBaseFunc = func(_, _, fetchURL string) ([]string, error) {
		receivedURL = fetchURL
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return false, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), mergeJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedURL != "" {
		t.Errorf("MergeBase fetchURL = %q, want empty for non-fork mode", receivedURL)
	}
}

func TestExecuteMerge_MultiRepo_ForkMode_PassesUpstreamURL(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	d.projects.ResolveProjectFunc = func(_ models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{
				{Name: "svc-a", Owner: "org", Repo: "svc-a", CloneURL: "https://github.com/org/svc-a.git", BaseBranch: "main"},
				{Name: "svc-b", Owner: "org", Repo: "svc-b", CloneURL: "https://github.com/org/svc-b.git", BaseBranch: "main"},
				{Name: "svc-c", Owner: "org", Repo: "svc-c", CloneURL: "https://github.com/org/svc-c.git", BaseBranch: "main"},
			},
			Container:        models.ContainerSettings{Image: "fat-container:latest"},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "fork-bot",
		}, nil
	}

	d.git.GetPRForBranchFunc = mergePRFunc()

	receivedURLs := map[string]string{}
	d.git.MergeBaseFunc = func(dir, _, fetchURL string) ([]string, error) {
		receivedURLs[dir] = fetchURL
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, skip bool) (string, error) {
		if !skip {
			t.Error("expected skipFileGuardrail=true for merge commit")
		}
		return "abc123", nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), mergeJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := map[string]string{
		filepath.Join(d.wsDir, "svc-a"): "https://github.com/org/svc-a.git",
		filepath.Join(d.wsDir, "svc-b"): "https://github.com/org/svc-b.git",
		filepath.Join(d.wsDir, "svc-c"): "https://github.com/org/svc-c.git",
	}
	for dir, want := range expected {
		got, ok := receivedURLs[dir]
		if !ok {
			t.Errorf("MergeBase was not called for %s", dir)
			continue
		}
		if got != want {
			t.Errorf("MergeBase for %s: fetchURL = %q, want %q", dir, got, want)
		}
	}
}

func mergePRFunc() func(string, string, string) (*models.PRDetails, error) {
	return func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number:     42,
			Title:      "Fix auth flow",
			Branch:     "ai-bot/PROJ-1",
			BaseBranch: "main",
			URL:        "https://github.com/org/repo/pull/42",
		}, nil
	}
}

func mergeJob(ticketKey string) *jobmanager.Job {
	return &jobmanager.Job{
		ID:         "merge-job-1",
		TicketKey:  ticketKey,
		Type:       jobmanager.JobTypeMerge,
		AttemptNum: 1,
	}
}

// --- Command reply lifecycle ---

func TestExecuteMerge_CommandSource_PostsAndUpdatesReply(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()
	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc123", nil
	}

	var postedBody string
	d.git.PostIssueCommentFunc = func(_, _ string, _ int, body string) (int64, error) {
		postedBody = body
		return 999, nil
	}
	var updatedID int64
	var updatedBody string
	d.git.UpdateIssueCommentFunc = func(_, _ string, id int64, body string) error {
		updatedID = id
		updatedBody = body
		return nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")
	job.CommandSource = &jobmanager.CommandSource{
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  42,
		CommentID: 100,
	}

	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(postedBody, "Merging from main") {
		t.Errorf("expected initial reply with merge message, got %q", postedBody)
	}
	if !strings.Contains(postedBody, "<!-- addressed: 100 -->") {
		t.Errorf("expected addressed marker in initial reply, got %q", postedBody)
	}
	if updatedID != 999 {
		t.Errorf("expected update on reply comment 999, got %d", updatedID)
	}
	if !strings.Contains(updatedBody, "Successfully merged") {
		t.Errorf("expected success message in updated reply, got %q", updatedBody)
	}
	if !strings.Contains(updatedBody, "abc123") {
		t.Errorf("expected commit SHA in updated reply, got %q", updatedBody)
	}
}

func TestExecuteMerge_CommandSource_FailureUpdatesReply(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	d.git.PostIssueCommentFunc = func(_, _ string, _ int, _ string) (int64, error) {
		return 999, nil
	}
	var updatedBody string
	d.git.UpdateIssueCommentFunc = func(_, _ string, _ int64, body string) error {
		updatedBody = body
		return nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")
	job.CommandSource = &jobmanager.CommandSource{
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  42,
		CommentID: 100,
	}

	_, err := p.Execute(context.Background(), job)
	if err == nil {
		t.Fatal("expected error when no PR found")
	}

	if !strings.Contains(updatedBody, "Failed to merge") {
		t.Errorf("expected failure message in updated reply, got %q", updatedBody)
	}
}

func TestExecuteMerge_NoCommandSource_NoReply(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = mergePRFunc()
	d.git.MergeBaseFunc = func(_, _, _ string) ([]string, error) {
		return []string{}, nil
	}
	d.git.HasChangesFunc = func(_, _ string) (bool, error) {
		return true, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc123", nil
	}

	commentPosted := false
	d.git.PostIssueCommentFunc = func(_, _ string, _ int, _ string) (int64, error) {
		commentPosted = true
		return 0, nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")

	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if commentPosted {
		t.Error("expected no comment post when CommandSource is nil")
	}
}

func TestExecuteMerge_CommandSource_PreflightFailure_PostsAddressedReply(t *testing.T) {
	d := newTestDeps(t)
	d.tracker.GetWorkItemFunc = func(_ string) (*models.WorkItem, error) {
		return nil, fmt.Errorf("ticket not found")
	}

	var postedBody string
	d.git.PostIssueCommentFunc = func(_, _ string, _ int, body string) (int64, error) {
		postedBody = body
		return 999, nil
	}

	p := d.pipeline(t)
	job := mergeJob("PROJ-1")
	job.CommandSource = &jobmanager.CommandSource{
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  42,
		CommentID: 100,
	}

	_, err := p.Execute(context.Background(), job)
	if err == nil {
		t.Fatal("expected error from preflight failure")
	}

	if !strings.Contains(postedBody, "<!-- addressed: 100 -->") {
		t.Errorf("expected addressed marker in preflight failure reply, got %q", postedBody)
	}
	if !strings.Contains(postedBody, "Failed") {
		t.Errorf("expected failure message in preflight reply, got %q", postedBody)
	}
}
