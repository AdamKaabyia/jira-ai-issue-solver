package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/container/containertest"
	"jira-ai-issue-solver/executor"
	"jira-ai-issue-solver/executor/executortest"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/repoconfig"
	"jira-ai-issue-solver/services"
	"jira-ai-issue-solver/taskfile"
	"jira-ai-issue-solver/taskfile/taskfiletest"
	"jira-ai-issue-solver/tracker/trackertest"
	"jira-ai-issue-solver/workspace"
	"jira-ai-issue-solver/workspace/workspacetest"
)

// --- NewPipeline validation ---

func TestNewPipeline_RejectsEmptyBotUsername(t *testing.T) {
	_, err := executor.NewPipeline(
		executor.Config{DefaultProvider: "claude"},
		&trackertest.Stub{}, &executortest.StubGitService{},
		&containertest.StubManager{}, &workspacetest.Stub{},
		&taskfiletest.Stub{}, &executortest.StubProjectResolver{},
		zap.NewNop())
	if err == nil {
		t.Fatal("expected error for empty bot username")
	}
}

func TestNewPipeline_RejectsEmptyProvider(t *testing.T) {
	_, err := executor.NewPipeline(
		executor.Config{BotUsername: "bot"},
		&trackertest.Stub{}, &executortest.StubGitService{},
		&containertest.StubManager{}, &workspacetest.Stub{},
		&taskfiletest.Stub{}, &executortest.StubProjectResolver{},
		zap.NewNop())
	if err == nil {
		t.Fatal("expected error for empty default provider")
	}
}

func TestNewPipeline_RejectsNilDependencies(t *testing.T) {
	cfg := executor.Config{BotUsername: "bot", DefaultProvider: "claude"}
	deps := []struct {
		name string
		fn   func() error
	}{
		{"tracker", func() error {
			_, err := executor.NewPipeline(cfg, nil,
				&executortest.StubGitService{}, &containertest.StubManager{},
				&workspacetest.Stub{}, &taskfiletest.Stub{},
				&executortest.StubProjectResolver{}, zap.NewNop())
			return err
		}},
		{"git", func() error {
			_, err := executor.NewPipeline(cfg, &trackertest.Stub{},
				nil, &containertest.StubManager{},
				&workspacetest.Stub{}, &taskfiletest.Stub{},
				&executortest.StubProjectResolver{}, zap.NewNop())
			return err
		}},
		{"containers", func() error {
			_, err := executor.NewPipeline(cfg, &trackertest.Stub{},
				&executortest.StubGitService{}, nil,
				&workspacetest.Stub{}, &taskfiletest.Stub{},
				&executortest.StubProjectResolver{}, zap.NewNop())
			return err
		}},
		{"workspaces", func() error {
			_, err := executor.NewPipeline(cfg, &trackertest.Stub{},
				&executortest.StubGitService{}, &containertest.StubManager{},
				nil, &taskfiletest.Stub{},
				&executortest.StubProjectResolver{}, zap.NewNop())
			return err
		}},
		{"taskWriter", func() error {
			_, err := executor.NewPipeline(cfg, &trackertest.Stub{},
				&executortest.StubGitService{}, &containertest.StubManager{},
				&workspacetest.Stub{}, nil,
				&executortest.StubProjectResolver{}, zap.NewNop())
			return err
		}},
		{"projects", func() error {
			_, err := executor.NewPipeline(cfg, &trackertest.Stub{},
				&executortest.StubGitService{}, &containertest.StubManager{},
				&workspacetest.Stub{}, &taskfiletest.Stub{},
				nil, zap.NewNop())
			return err
		}},
		{"logger", func() error {
			_, err := executor.NewPipeline(cfg, &trackertest.Stub{},
				&executortest.StubGitService{}, &containertest.StubManager{},
				&workspacetest.Stub{}, &taskfiletest.Stub{},
				&executortest.StubProjectResolver{}, nil)
			return err
		}},
	}

	for _, d := range deps {
		if d.fn() == nil {
			t.Errorf("expected error for nil %s", d.name)
		}
	}
}

func TestNewPipeline_ValidConfig(t *testing.T) {
	p, err := executor.NewPipeline(
		executor.Config{BotUsername: "bot", DefaultProvider: "claude"},
		&trackertest.Stub{}, &executortest.StubGitService{},
		&containertest.StubManager{}, &workspacetest.Stub{},
		&taskfiletest.Stub{}, &executortest.StubProjectResolver{},
		zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected non-nil pipeline")
	}
}

// --- Execute dispatch ---

func TestExecute_FeedbackDispatch(t *testing.T) {
	d := newTestDeps(t)
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Title: "Fix a bug",
			Branch: head, URL: "https://github.com/org/repo/pull/42",
		}, nil
	}
	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})
	// Should not return "not yet implemented".
	if err != nil && strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("feedback pipeline should be implemented, got %v", err)
	}
}

func TestExecute_UnknownTypeReturnsError(t *testing.T) {
	d := newTestDeps(t)
	p := d.pipeline(t)

	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: "unknown",
	})
	if err == nil || !strings.Contains(err.Error(), "unknown job type") {
		t.Fatalf("expected unknown type error, got %v", err)
	}
}

// --- Happy path ---

func TestExecuteNewTicket_HappyPath(t *testing.T) {
	d := newTestDeps(t)

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	var prParams models.PRParams
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prParams = params
		return &models.PR{Number: 42, URL: "https://github.com/org/repo/pull/42"}, nil
	}

	var commentPosted string
	d.tracker.AddCommentFunc = func(key, body string) error {
		commentPosted = body
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-123"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify result.
	if result.PRURL != "https://github.com/org/repo/pull/42" {
		t.Errorf("PRURL = %q, want github URL", result.PRURL)
	}
	if result.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", result.PRNumber)
	}
	if result.Draft {
		t.Error("expected non-draft PR")
	}
	if !result.ValidationPassed {
		t.Error("expected ValidationPassed = true")
	}

	// Verify status transitions: in-progress, then in-review.
	if len(transitions) != 2 {
		t.Fatalf("transitions = %v, want 2 entries", transitions)
	}
	if transitions[0] != "In Progress" {
		t.Errorf("first transition = %q, want In Progress", transitions[0])
	}
	if transitions[1] != "In Review" {
		t.Errorf("second transition = %q, want In Review", transitions[1])
	}

	// Verify PR was created with correct params.
	if prParams.Owner != "org" || prParams.Repo != "repo" {
		t.Errorf("PR owner/repo = %s/%s, want org/repo", prParams.Owner, prParams.Repo)
	}
	if prParams.Base != "main" {
		t.Errorf("PR base = %q, want main", prParams.Base)
	}
	if !strings.Contains(prParams.Head, "PROJ-123") {
		t.Errorf("PR head = %q, should contain ticket key", prParams.Head)
	}

	// Verify PR URL posted as comment (no PRURLFieldName configured).
	if !strings.Contains(commentPosted, "https://github.com/org/repo/pull/42") {
		t.Errorf("comment = %q, should contain PR URL", commentPosted)
	}
}

// --- No changes ---

func TestExecuteNewTicket_NoChanges(t *testing.T) {
	d := newTestDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	var errorComment string
	d.tracker.AddCommentFunc = func(key, body string) error {
		errorComment = body
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("expected no-changes error, got %v", err)
	}

	// Verify status reverted: in-progress, then back to todo.
	if len(transitions) < 2 {
		t.Fatalf("transitions = %v, want at least 2", transitions)
	}
	if last := transitions[len(transitions)-1]; last != "To Do" {
		t.Errorf("last transition = %q, want To Do (revert)", last)
	}

	// Verify error comment posted.
	if !strings.Contains(errorComment, "no changes") {
		t.Errorf("error comment = %q, should mention no changes", errorComment)
	}
}

// --- AI timeout ---

func TestExecuteNewTicket_SessionTimeout(t *testing.T) {
	d := newTestDeps(t)

	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		<-ctx.Done() // wait for session timeout
		return "", 0, ctx.Err()
	}

	var reverted bool
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		if status == "To Do" {
			reverted = true
		}
		return nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		SessionTimeout:  100 * time.Millisecond,
		AIAPIKeys:       map[string]string{"claude": "test-key"},
	})

	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if !strings.Contains(err.Error(), "session timeout exceeded") {
		t.Errorf("expected session timeout error, got %v", err)
	}
	if !reverted {
		t.Error("expected status to be reverted to todo")
	}
}

func TestExecuteNewTicket_ParentContextCancelled(t *testing.T) {
	d := newTestDeps(t)

	ctx, cancel := context.WithCancel(context.Background())
	d.containers.ExecFunc = func(execCtx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		cancel() // simulate shutdown
		return "", 0, context.Canceled
	}

	p := d.pipeline(t)
	_, err := p.Execute(ctx, newTicketJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "job cancelled") {
		t.Fatalf("expected job cancelled error, got %v", err)
	}
}

// --- Container start failure ---

func TestExecuteNewTicket_ContainerStartFails(t *testing.T) {
	d := newTestDeps(t)
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		return nil, errors.New("image not found")
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "start container") {
		t.Fatalf("expected container start error, got %v", err)
	}
}

// --- Commit failure ---

func TestExecuteNewTicket_CommitFails(t *testing.T) {
	d := newTestDeps(t)
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "", errors.New("API rate limit")
	}

	var reverted bool
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		if status == "To Do" {
			reverted = true
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "commit changes") {
		t.Fatalf("expected commit error, got %v", err)
	}
	if !reverted {
		t.Error("expected status to be reverted")
	}
}

// --- PR creation failure ---

func TestExecuteNewTicket_PRCreationFails(t *testing.T) {
	d := newTestDeps(t)
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		return nil, errors.New("PR creation failed")
	}

	var reverted bool
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		if status == "To Do" {
			reverted = true
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "create PR") {
		t.Fatalf("expected PR error, got %v", err)
	}
	if !reverted {
		t.Error("expected status to be reverted")
	}
}

// --- Validation label paths ---

func TestExecuteNewTicket_NonZeroExitCode_AppliesLabel(t *testing.T) {
	d := newTestDeps(t)
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		return "", 1, nil // non-zero exit, no exec error
	}

	var prDraft bool
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prDraft = params.Draft
		return &models.PR{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var addedLabels []string
	d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error {
		addedLabels = append(addedLabels, label)
		return nil
	}

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prDraft {
		t.Error("expected non-draft PR")
	}
	if result.Draft {
		t.Error("expected result.Draft = false")
	}
	if result.ValidationPassed {
		t.Error("expected ValidationPassed = false for nonzero exit")
	}
	if len(addedLabels) != 1 || addedLabels[0] != "ai-nonzero-exit" {
		t.Errorf("addedLabels = %v, want [ai-nonzero-exit]", addedLabels)
	}
	// Should still transition to In Review.
	found := false
	for _, s := range transitions {
		if s == "In Review" {
			found = true
		}
	}
	if !found {
		t.Errorf("transitions = %v, want In Review", transitions)
	}
}

func TestExecuteNewTicket_ValidationFailed_AppliesLabel(t *testing.T) {
	d := newTestDeps(t)

	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		writeSessionOutput(t, d.wsDir, executor.SessionOutput{
			ExitCode:         0,
			ValidationPassed: boolPtr(false),
		})
		return "", 0, nil
	}

	var prDraft bool
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prDraft = params.Draft
		return &models.PR{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var addedLabels []string
	d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error {
		addedLabels = append(addedLabels, label)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prDraft {
		t.Error("expected non-draft PR")
	}
	if result.ValidationPassed {
		t.Error("expected ValidationPassed = false")
	}
	if len(addedLabels) != 1 || addedLabels[0] != "ai-validation-failed" {
		t.Errorf("addedLabels = %v, want [ai-validation-failed]", addedLabels)
	}
}

func TestExecuteNewTicket_RepoConfigForcesDraft(t *testing.T) {
	d := newTestDeps(t)

	cfgDir := filepath.Join(d.wsDir, ".ai-bot")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfgContent := "pr:\n  draft: true\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	var prDraft bool
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prDraft = params.Draft
		return &models.PR{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !prDraft {
		t.Error("expected draft PR from repo config")
	}
	if !result.Draft {
		t.Error("expected result.Draft = true")
	}
	// Should still transition to In Review even for repo-config drafts.
	found := false
	for _, s := range transitions {
		if s == "In Review" {
			found = true
		}
	}
	if !found {
		t.Errorf("transitions = %v, want In Review", transitions)
	}
}

// --- Security-level tickets ---

func TestExecuteNewTicket_SecurityLevel_RedactedPR(t *testing.T) {
	d := newTestDeps(t)
	d.tracker.GetWorkItemFunc = func(key string) (*models.WorkItem, error) {
		return &models.WorkItem{
			Key:           key,
			Summary:       "Fix auth bypass vulnerability",
			Description:   "Critical security vulnerability details...",
			Type:          "Bug",
			SecurityLevel: "Embargoed",
			Components:    []string{},
			Labels:        []string{},
		}, nil
	}

	var prParams models.PRParams
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prParams = params
		return &models.PR{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("SEC-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Title should be redacted.
	if strings.Contains(prParams.Title, "auth bypass") {
		t.Errorf("PR title should be redacted, got %q", prParams.Title)
	}
	if !strings.Contains(prParams.Title, "Security fix") {
		t.Errorf("PR title should contain 'Security fix', got %q", prParams.Title)
	}

	// Body should be redacted.
	if strings.Contains(prParams.Body, "vulnerability details") {
		t.Errorf("PR body should be redacted, got %q", prParams.Body)
	}
	if !strings.Contains(prParams.Body, "redacted") {
		t.Errorf("PR body should mention redaction, got %q", prParams.Body)
	}
}

// --- Co-author attribution ---

func TestExecuteNewTicket_CoAuthorAttribution(t *testing.T) {
	d := newTestDeps(t)
	d.tracker.GetWorkItemFunc = func(key string) (*models.WorkItem, error) {
		return &models.WorkItem{
			Key:     key,
			Summary: "Fix bug",
			Type:    "Bug",
			Assignee: &models.Author{
				Name:  "Jane Doe",
				Email: "jane@example.com",
			},
			Components: []string{},
			Labels:     []string{},
		}, nil
	}

	var receivedCoAuthor *models.Author
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, coAuthor *models.Author, _ []string, _ bool) (string, error) {
		receivedCoAuthor = coAuthor
		return "abc123", nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedCoAuthor == nil {
		t.Fatal("expected co-author to be set")
	}
	if receivedCoAuthor.Name != "Jane Doe" {
		t.Errorf("co-author name = %q, want Jane Doe", receivedCoAuthor.Name)
	}
	if receivedCoAuthor.Email != "jane@example.com" {
		t.Errorf("co-author email = %q, want jane@example.com", receivedCoAuthor.Email)
	}
}

func TestExecuteNewTicket_NoAssignee_NilCoAuthor(t *testing.T) {
	d := newTestDeps(t)

	var receivedCoAuthor *models.Author
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, coAuthor *models.Author, _ []string, _ bool) (string, error) {
		receivedCoAuthor = coAuthor
		return "abc123", nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedCoAuthor != nil {
		t.Errorf("expected nil co-author, got %+v", receivedCoAuthor)
	}
}

// --- Error comments disabled ---

func TestExecuteNewTicket_ErrorCommentsDisabled(t *testing.T) {
	d := newTestDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil // trigger failure
	}

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:                []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus:     "In Progress",
			InReviewStatus:       "In Review",
			TodoStatus:           "To Do",
			DisableErrorComments: true,
		}, nil
	}

	commentPosted := false
	d.tracker.AddCommentFunc = func(key, body string) error {
		commentPosted = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil {
		t.Fatal("expected error")
	}
	if commentPosted {
		t.Error("expected no error comment when disabled")
	}
}

// --- Status comment upsert ---

func TestExecuteNewTicket_StatusComment_CreatedOnFirstFailure(t *testing.T) {
	d := newTestDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	var addedComment string
	d.tracker.AddCommentFunc = func(key, body string) error {
		addedComment = body
		return nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		MaxRetries:      2,
	})
	_, _ = p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if !strings.Contains(addedComment, "[AI-BOT-STATUS]") {
		t.Errorf("comment missing status marker, got:\n%s", addedComment)
	}
	if !strings.Contains(addedComment, "attempt 1 of 3") {
		t.Errorf("comment missing attempt info, got:\n%s", addedComment)
	}
}

func TestExecuteNewTicket_StatusComment_UpdatedOnRetry(t *testing.T) {
	d := newTestDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	d.tracker.GetCommentsFunc = func(key string) ([]models.Comment, error) {
		return []models.Comment{
			{ID: "100", Body: "[AI-BOT-STATUS] AI processing failed (attempt 1 of 3)\n\nError: old"},
		}, nil
	}

	var updatedID, updatedBody string
	d.tracker.UpdateCommentFunc = func(key, commentID, body string) error {
		updatedID = commentID
		updatedBody = body
		return nil
	}

	addCalled := false
	d.tracker.AddCommentFunc = func(key, body string) error {
		addCalled = true
		return nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		MaxRetries:      2,
	})
	job := newTicketJob("PROJ-1")
	job.AttemptNum = 2
	_, _ = p.Execute(context.Background(), job)

	if updatedID != "100" {
		t.Errorf("expected comment 100 to be updated, got %q", updatedID)
	}
	if !strings.Contains(updatedBody, "attempt 2 of 3") {
		t.Errorf("updated comment missing attempt info, got:\n%s", updatedBody)
	}
	if addCalled {
		t.Error("should update existing comment, not add a new one")
	}
}

func TestExecuteNewTicket_StatusComment_DeletedOnPRCreation(t *testing.T) {
	d := newTestDeps(t)

	d.tracker.GetCommentsFunc = func(key string) ([]models.Comment, error) {
		return []models.Comment{
			{ID: "200", Body: "[AI-BOT-STATUS] AI processing failed (attempt 1 of 3)\n\nError: old"},
		}, nil
	}

	var deletedID string
	d.tracker.DeleteCommentFunc = func(key, commentID string) error {
		deletedID = commentID
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if deletedID != "200" {
		t.Errorf("expected status comment 200 to be deleted, got %q", deletedID)
	}
}

// --- Workspace reuse (retry scenario) ---

func TestExecuteNewTicket_WorkspaceReused_SwitchesBranch(t *testing.T) {
	d := newTestDeps(t)
	d.workspaces.FindOrCreateFunc = func(ticketKey, repoURL string) (string, bool, error) {
		return d.wsDir, true, nil // reused
	}
	d.git.RemoteBranchExistsFunc = func(owner, repo, branch string) (bool, error) {
		return true, nil // remote branch still exists
	}

	branchSwitched := false
	d.git.SwitchBranchFunc = func(dir, name string) error {
		branchSwitched = true
		if !strings.Contains(name, "PROJ-1") {
			t.Errorf("branch name = %q, should contain ticket key", name)
		}
		return nil
	}

	branchCreated := false
	d.git.CreateBranchFunc = func(dir, name, baseBranch string) error {
		branchCreated = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !branchSwitched {
		t.Error("expected SwitchBranch to be called on reuse")
	}
	if branchCreated {
		t.Error("expected CreateBranch NOT to be called on reuse")
	}
}

func TestExecuteNewTicket_WorkspaceReused_RemoteBranchDeleted_RecreatesBranch(t *testing.T) {
	d := newTestDeps(t)
	d.workspaces.FindOrCreateFunc = func(ticketKey, repoURL string) (string, bool, error) {
		return d.wsDir, true, nil // reused
	}
	d.git.RemoteBranchExistsFunc = func(owner, repo, branch string) (bool, error) {
		return false, nil // remote branch was deleted
	}

	branchSwitched := false
	d.git.SwitchBranchFunc = func(dir, name string) error {
		branchSwitched = true
		return nil
	}

	branchCreated := false
	d.git.CreateBranchFunc = func(dir, name, baseBranch string) error {
		branchCreated = true
		if !strings.Contains(name, "PROJ-1") {
			t.Errorf("branch name = %q, should contain ticket key", name)
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branchSwitched {
		t.Error("expected SwitchBranch NOT to be called when remote branch deleted")
	}
	if !branchCreated {
		t.Error("expected CreateBranch to be called when remote branch deleted")
	}
}

// --- PR URL field ---

func TestExecuteNewTicket_PRURLViaField(t *testing.T) {
	d := newTestDeps(t)
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			PRURLFieldName:   "Git Pull Request",
		}, nil
	}

	var fieldSet bool
	d.tracker.SetFieldValueFunc = func(key, field, value string) error {
		if field == "Git Pull Request" && strings.Contains(value, "github.com") {
			fieldSet = true
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fieldSet {
		t.Error("expected PR URL to be set via field")
	}
}

// --- Container stopped on all paths ---

func TestExecuteNewTicket_ContainerStoppedOnSuccess(t *testing.T) {
	d := newTestDeps(t)

	stopped := false
	d.containers.StopFunc = func(ctx context.Context, ctr *container.Container) error {
		stopped = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stopped {
		t.Error("expected container to be stopped")
	}
}

func TestExecuteNewTicket_ContainerStoppedOnFailure(t *testing.T) {
	d := newTestDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil // trigger failure
	}

	stopped := false
	d.containers.StopFunc = func(ctx context.Context, ctr *container.Container) error {
		stopped = true
		return nil
	}

	p := d.pipeline(t)
	_, _ = p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if !stopped {
		t.Error("expected container to be stopped on failure")
	}
}

// --- Session cost ---

func TestExecuteNewTicket_SessionCostCaptured(t *testing.T) {
	d := newTestDeps(t)

	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		writeSessionOutput(t, d.wsDir, executor.SessionOutput{
			ExitCode: 0,
			CostUSD:  1.25,
		})
		return "", 0, nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CostUSD != 1.25 {
		t.Errorf("CostUSD = %f, want 1.25", result.CostUSD)
	}
}

// --- Branch naming ---

func TestExecuteNewTicket_BranchNameFormat(t *testing.T) {
	d := newTestDeps(t)

	var branchName string
	d.git.CreateBranchFunc = func(dir, name, baseBranch string) error {
		branchName = name
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-123"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if branchName != "ai-bot/PROJ-123" {
		t.Errorf("branch name = %q, want ai-bot/PROJ-123", branchName)
	}
}

// --- PR title prefix from repo config ---

func TestExecuteNewTicket_TitlePrefixFromRepoConfig(t *testing.T) {
	d := newTestDeps(t)

	cfgDir := filepath.Join(d.wsDir, ".ai-bot")
	if err := os.MkdirAll(cfgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfgContent := "pr:\n  title_prefix: \"[AI]\"\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}

	var prTitle string
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prTitle = params.Title
		return &models.PR{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(prTitle, "[AI] ") {
		t.Errorf("PR title = %q, should start with [AI]", prTitle)
	}
}

// --- Provider resolution ---

func TestExecuteNewTicket_ProjectOverridesDefaultProvider(t *testing.T) {
	d := newTestDeps(t)
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			AIProvider:       "gemini",
		}, nil
	}

	var envVars map[string]string
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		envVars = env
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "c-key", "gemini": "g-key"},
	})
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if envVars["AI_PROVIDER"] != "gemini" {
		t.Errorf("AI_PROVIDER = %q, want gemini", envVars["AI_PROVIDER"])
	}
	if _, ok := envVars["GEMINI_API_KEY"]; !ok {
		t.Error("expected GEMINI_API_KEY in container env")
	}
}

// --- Vertex AI authentication ---

func TestExecuteNewTicket_VertexAI_SetsEnvVars(t *testing.T) {
	d := newTestDeps(t)

	var envVars map[string]string
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		envVars = env
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	credsFile := createTempCredsFile(t)

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		ClaudeVertex: &executor.ClaudeVertexConfig{
			ProjectID:       "my-gcp-project",
			Region:          "us-east5",
			CredentialsFile: credsFile,
		},
	})
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if envVars["CLAUDE_CODE_USE_VERTEX"] != "1" {
		t.Errorf("CLAUDE_CODE_USE_VERTEX = %q, want 1", envVars["CLAUDE_CODE_USE_VERTEX"])
	}
	if envVars["ANTHROPIC_VERTEX_PROJECT_ID"] != "my-gcp-project" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID = %q, want my-gcp-project", envVars["ANTHROPIC_VERTEX_PROJECT_ID"])
	}
	if envVars["CLOUD_ML_REGION"] != "us-east5" {
		t.Errorf("CLOUD_ML_REGION = %q, want us-east5", envVars["CLOUD_ML_REGION"])
	}
	if envVars["GOOGLE_APPLICATION_CREDENTIALS"] != executor.ContainerCredsMountTarget {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q, want %q", envVars["GOOGLE_APPLICATION_CREDENTIALS"], executor.ContainerCredsMountTarget)
	}
	if _, ok := envVars["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY should not be set when using Vertex AI")
	}
}

func TestExecuteNewTicket_VertexAI_MountsCredentialsFile(t *testing.T) {
	d := newTestDeps(t)

	credsFile := createTempCredsFile(t)

	var receivedCfg *container.Config
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		receivedCfg = cfg
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		ClaudeVertex: &executor.ClaudeVertexConfig{
			ProjectID:       "my-gcp-project",
			Region:          "us-east5",
			CredentialsFile: credsFile,
		},
	})
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the credentials mount in ExtraMounts.
	var found bool
	for _, m := range receivedCfg.ExtraMounts {
		if m.Target == executor.ContainerCredsMountTarget {
			found = true
			if m.Source != credsFile {
				t.Errorf("mount source = %q, want %q", m.Source, credsFile)
			}
			if m.Options != "ro" {
				t.Errorf("mount options = %q, want ro", m.Options)
			}
		}
	}
	if !found {
		t.Error("expected credentials file mount in ExtraMounts")
	}
}

func TestExecuteNewTicket_VertexAI_OverridesAPIKey(t *testing.T) {
	d := newTestDeps(t)

	var envVars map[string]string
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		envVars = env
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	credsFile := createTempCredsFile(t)

	// Both AIAPIKeys and ClaudeVertex set — Vertex should take precedence.
	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "sk-ant-should-not-be-used"},
		ClaudeVertex: &executor.ClaudeVertexConfig{
			ProjectID:       "my-gcp-project",
			Region:          "us-east5",
			CredentialsFile: credsFile,
		},
	})
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := envVars["ANTHROPIC_API_KEY"]; ok {
		t.Error("ANTHROPIC_API_KEY should not be set when Vertex is configured")
	}
	if envVars["ANTHROPIC_VERTEX_PROJECT_ID"] != "my-gcp-project" {
		t.Errorf("ANTHROPIC_VERTEX_PROJECT_ID = %q, want my-gcp-project", envVars["ANTHROPIC_VERTEX_PROJECT_ID"])
	}
}

func TestExecuteNewTicket_VertexAI_NotUsedForGemini(t *testing.T) {
	d := newTestDeps(t)

	var envVars map[string]string
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		envVars = env
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	var receivedCfg *container.Config
	origResolve := d.containers.ResolveConfigFunc
	d.containers.ResolveConfigFunc = func(repoDir string, projectOverride *container.SettingsOverride) (*container.Config, error) {
		cfg, err := origResolve(repoDir, projectOverride)
		if err != nil {
			return nil, err
		}
		receivedCfg = cfg
		return cfg, nil
	}

	credsFile := createTempCredsFile(t)

	// Vertex is configured but provider is gemini — vertex should not apply.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			AIProvider:       "gemini",
		}, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"gemini": "g-key"},
		ClaudeVertex: &executor.ClaudeVertexConfig{
			ProjectID:       "my-gcp-project",
			Region:          "us-east5",
			CredentialsFile: credsFile,
		},
	})
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should use gemini env, not vertex.
	if envVars["AI_PROVIDER"] != "gemini" {
		t.Errorf("AI_PROVIDER = %q, want gemini", envVars["AI_PROVIDER"])
	}
	if _, ok := envVars["ANTHROPIC_VERTEX_PROJECT_ID"]; ok {
		t.Error("ANTHROPIC_VERTEX_PROJECT_ID should not be set for gemini provider")
	}
	if envVars["GEMINI_API_KEY"] != "g-key" {
		t.Errorf("GEMINI_API_KEY = %q, want g-key", envVars["GEMINI_API_KEY"])
	}

	// Credentials file should not be mounted.
	for _, m := range receivedCfg.ExtraMounts {
		if m.Target == executor.ContainerCredsMountTarget {
			t.Error("credentials file should not be mounted for gemini provider")
		}
	}
}

// --- ErrNoChanges handling ---

func TestExecuteNewTicket_ErrNoChanges_ReturnsError(t *testing.T) {
	d := newTestDeps(t)
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "", services.ErrNoChanges
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil {
		t.Fatal("expected error when CommitChanges returns ErrNoChanges")
	}
	if !strings.Contains(err.Error(), "no committable changes") {
		t.Errorf("error = %q, should mention 'no committable changes'", err.Error())
	}
}

// --- Auth strip/restore ---

func TestExecuteNewTicket_AuthStrippedBeforeAI(t *testing.T) {
	d := newTestDeps(t)

	var order []string
	d.git.StripRemoteAuthFunc = func(dir string) error {
		order = append(order, "strip")
		return nil
	}
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		order = append(order, "restore")
		return nil
	}
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		order = append(order, "exec")
		return "", 0, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify ordering: strip must happen before exec, restore after exec.
	stripIdx, execIdx, restoreIdx := -1, -1, -1
	for i, v := range order {
		switch v {
		case "strip":
			stripIdx = i
		case "exec":
			execIdx = i
		case "restore":
			if restoreIdx == -1 {
				restoreIdx = i
			}
		}
	}

	if stripIdx < 0 {
		t.Fatal("StripRemoteAuth was not called")
	}
	if execIdx < 0 {
		t.Fatal("container Exec was not called")
	}
	if restoreIdx < 0 {
		t.Fatal("RestoreRemoteAuth was not called")
	}
	if stripIdx >= execIdx {
		t.Error("StripRemoteAuth must be called before container Exec")
	}
	if execIdx >= restoreIdx {
		t.Error("RestoreRemoteAuth must be called after container Exec")
	}
}

func TestExecuteNewTicket_AuthRestoredOnExecFailure(t *testing.T) {
	d := newTestDeps(t)

	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		return "", 1, errors.New("exec failed")
	}

	restored := false
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		restored = true
		return nil
	}

	p := d.pipeline(t)
	_, _ = p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if !restored {
		t.Error("RestoreRemoteAuth should be called even when exec fails")
	}
}

// --- Container settings override ---

func TestExecuteNewTicket_ContainerSettingsPassedToResolve(t *testing.T) {
	d := newTestDeps(t)
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			Container: models.ContainerSettings{
				Image: "project-image:v1",
				ResourceLimits: models.ContainerResourceLimits{
					Memory: "16g",
				},
			},
		}, nil
	}

	var receivedOverride *container.SettingsOverride
	d.containers.ResolveConfigFunc = func(repoDir string, projectOverride *container.SettingsOverride) (*container.Config, error) {
		receivedOverride = projectOverride
		return &container.Config{Image: "project-image:v1"}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedOverride == nil {
		t.Fatal("expected project override to be passed to ResolveConfig")
	}
	if receivedOverride.Image != "project-image:v1" {
		t.Errorf("override image = %q, want %q", receivedOverride.Image, "project-image:v1")
	}
	if receivedOverride.Limits.Memory != "16g" {
		t.Errorf("override memory = %q, want %q", receivedOverride.Limits.Memory, "16g")
	}
}

func TestExecuteNewTicket_EmptyContainerSettingsNoOverride(t *testing.T) {
	d := newTestDeps(t)

	var receivedOverride *container.SettingsOverride
	d.containers.ResolveConfigFunc = func(repoDir string, projectOverride *container.SettingsOverride) (*container.Config, error) {
		receivedOverride = projectOverride
		return &container.Config{Image: "default:latest"}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedOverride != nil {
		t.Error("expected nil override when project has no container settings")
	}
}

// --- helpers ---

// --- mergeImports ---

func TestMergeImports_ProjectOnly(t *testing.T) {
	settings := &models.ProjectSettings{
		Repos: []models.RepoSettings{{
			Imports: []models.ImportConfig{
				{Repo: "https://github.com/org/workflows", Path: ".workflows", Ref: "main"},
			},
		}},
	}
	repoCfg := repoconfig.Default()

	result := executor.MergeImports(settings, repoCfg)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0].Repo != "https://github.com/org/workflows" {
		t.Errorf("Repo = %q, want %q", result[0].Repo, "https://github.com/org/workflows")
	}
	if result[0].Path != ".workflows" {
		t.Errorf("Path = %q, want %q", result[0].Path, ".workflows")
	}
}

func TestMergeImports_RepoOnly(t *testing.T) {
	settings := &models.ProjectSettings{Repos: []models.RepoSettings{{}}}
	repoCfg := &repoconfig.Config{
		ValidationCommands: []string{},
		Imports: []repoconfig.Import{
			{Repo: "https://github.com/org/tools", Path: ".tools"},
		},
		PR: repoconfig.PRConfig{Labels: []string{}},
	}

	result := executor.MergeImports(settings, repoCfg)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if result[0].Path != ".tools" {
		t.Errorf("Path = %q, want %q", result[0].Path, ".tools")
	}
}

func TestMergeImports_RepoOverridesProjectOnPathConflict(t *testing.T) {
	settings := &models.ProjectSettings{
		Repos: []models.RepoSettings{{
			Imports: []models.ImportConfig{
				{Repo: "https://github.com/org/workflows-v1", Path: ".workflows", Ref: "v1"},
			},
		}},
	}
	repoCfg := &repoconfig.Config{
		ValidationCommands: []string{},
		Imports: []repoconfig.Import{
			{Repo: "https://github.com/team/workflows-v2", Path: ".workflows", Ref: "v2"},
		},
		PR: repoconfig.PRConfig{Labels: []string{}},
	}

	result := executor.MergeImports(settings, repoCfg)

	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	// Repo-level should win.
	if result[0].Repo != "https://github.com/team/workflows-v2" {
		t.Errorf("Repo = %q, want repo-level override", result[0].Repo)
	}
	if result[0].Ref != "v2" {
		t.Errorf("Ref = %q, want %q", result[0].Ref, "v2")
	}
}

func TestMergeImports_BothSourcesDifferentPaths(t *testing.T) {
	settings := &models.ProjectSettings{
		Repos: []models.RepoSettings{{
			Imports: []models.ImportConfig{
				{Repo: "https://github.com/org/alpha", Path: ".alpha"},
			},
		}},
	}
	repoCfg := &repoconfig.Config{
		ValidationCommands: []string{},
		Imports: []repoconfig.Import{
			{Repo: "https://github.com/org/beta", Path: ".beta"},
		},
		PR: repoconfig.PRConfig{Labels: []string{}},
	}

	result := executor.MergeImports(settings, repoCfg)

	if len(result) != 2 {
		t.Fatalf("len(result) = %d, want 2", len(result))
	}
	// Sorted by path.
	if result[0].Path != ".alpha" {
		t.Errorf("result[0].Path = %q, want .alpha", result[0].Path)
	}
	if result[1].Path != ".beta" {
		t.Errorf("result[1].Path = %q, want .beta", result[1].Path)
	}
}

func TestMergeImports_Empty(t *testing.T) {
	settings := &models.ProjectSettings{Repos: []models.RepoSettings{{}}}
	repoCfg := repoconfig.Default()

	result := executor.MergeImports(settings, repoCfg)

	if len(result) != 0 {
		t.Fatalf("len(result) = %d, want 0", len(result))
	}
}

func TestMergeImports_PathNormalized(t *testing.T) {
	settings := &models.ProjectSettings{
		Repos: []models.RepoSettings{{
			Imports: []models.ImportConfig{
				{Repo: "https://github.com/org/a", Path: ".workflows/"},
			},
		}},
	}
	repoCfg := &repoconfig.Config{
		ValidationCommands: []string{},
		Imports: []repoconfig.Import{
			{Repo: "https://github.com/org/b", Path: ".workflows"},
		},
		PR: repoconfig.PRConfig{Labels: []string{}},
	}

	result := executor.MergeImports(settings, repoCfg)

	// Trailing slash should be normalized, so both map to same path.
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1 (trailing slash normalized)", len(result))
	}
	// Repo-level wins.
	if result[0].Repo != "https://github.com/org/b" {
		t.Errorf("Repo = %q, want repo-level", result[0].Repo)
	}
}

// --- Clone imports via pipeline ---

func TestExecuteNewTicket_ClonesImports(t *testing.T) {
	d := newTestDeps(t)

	// Configure project-level imports.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{{
				Owner:      "org",
				Repo:       "repo",
				CloneURL:   "https://github.com/org/repo.git",
				Imports:    []models.ImportConfig{{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Ref: "main"}},
				BaseBranch: "main",
			}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	var clonedURL, clonedDest, clonedRef string
	d.git.CloneImportFunc = func(url, destDir, ref string) error {
		clonedURL = url
		clonedDest = destDir
		clonedRef = ref
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if clonedURL != "https://github.com/org/workflows" {
		t.Errorf("CloneImport URL = %q, want %q", clonedURL, "https://github.com/org/workflows")
	}
	expectedDest := filepath.Join(d.wsDir, ".ai-workflows")
	if clonedDest != expectedDest {
		t.Errorf("CloneImport destDir = %q, want %q", clonedDest, expectedDest)
	}
	if clonedRef != "main" {
		t.Errorf("CloneImport ref = %q, want %q", clonedRef, "main")
	}
}

func TestExecuteNewTicket_SkipsExistingImportDir(t *testing.T) {
	d := newTestDeps(t)

	// Pre-create the import directory (simulates workspace reuse).
	importDir := filepath.Join(d.wsDir, ".ai-workflows")
	if err := os.MkdirAll(importDir, 0o750); err != nil {
		t.Fatal(err)
	}

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{{
				Owner:      "org",
				Repo:       "repo",
				CloneURL:   "https://github.com/org/repo.git",
				Imports:    []models.ImportConfig{{Repo: "https://github.com/org/workflows", Path: ".ai-workflows"}},
				BaseBranch: "main",
			}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	cloneCalled := false
	d.git.CloneImportFunc = func(url, destDir, ref string) error {
		cloneCalled = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cloneCalled {
		t.Error("CloneImport should not be called when directory exists")
	}
}

func TestExecuteNewTicket_ImportCloneFailure(t *testing.T) {
	d := newTestDeps(t)

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{{
				Owner:      "org",
				Repo:       "repo",
				CloneURL:   "https://github.com/org/repo.git",
				Imports:    []models.ImportConfig{{Repo: "https://github.com/org/broken", Path: ".broken"}},
				BaseBranch: "main",
			}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	d.git.CloneImportFunc = func(url, destDir, ref string) error {
		return errors.New("clone failed: repository not found")
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil {
		t.Fatal("expected error when import clone fails")
	}
	if !strings.Contains(err.Error(), "clone import") {
		t.Errorf("error = %q, should mention clone import", err.Error())
	}
}

func TestExecuteNewTicket_NoImports(t *testing.T) {
	d := newTestDeps(t)

	cloneCalled := false
	d.git.CloneImportFunc = func(url, destDir, ref string) error {
		cloneCalled = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if cloneCalled {
		t.Error("CloneImport should not be called when no imports configured")
	}
}

// --- runImportInstalls ---

func TestRunImportInstalls_RunsInstallCommands(t *testing.T) {
	d := newTestDeps(t)

	var execCmds [][]string
	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, cmd []string) (string, int, error) {
		execCmds = append(execCmds, cmd)
		return "", 0, nil
	}

	p := d.pipeline(t)
	ctr := &container.Container{ID: "c1", Name: "test-c1"}
	imports := []executor.ImportEntry{
		{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Ref: "main", Install: ".ai-workflows/install.sh"},
		{Repo: "https://github.com/org/tools", Path: "tools", Ref: "", Install: "tools/setup.sh --flag"},
	}

	err := p.RunImportInstalls(context.Background(), zap.NewNop(), ctr, imports)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(execCmds) != 2 {
		t.Fatalf("expected 2 exec calls, got %d", len(execCmds))
	}

	// Verify command structure: sh -c "cd /workspace && <install>"
	wantCmd0 := []string{"sh", "-c", "cd /workspace && .ai-workflows/install.sh"}
	if !equalSlice(execCmds[0], wantCmd0) {
		t.Errorf("exec cmd[0] = %v, want %v", execCmds[0], wantCmd0)
	}
	wantCmd1 := []string{"sh", "-c", "cd /workspace && tools/setup.sh --flag"}
	if !equalSlice(execCmds[1], wantCmd1) {
		t.Errorf("exec cmd[1] = %v, want %v", execCmds[1], wantCmd1)
	}
}

func TestRunImportInstalls_SkipsEmptyInstall(t *testing.T) {
	d := newTestDeps(t)

	execCalled := false
	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, _ []string) (string, int, error) {
		execCalled = true
		return "", 0, nil
	}

	p := d.pipeline(t)
	ctr := &container.Container{ID: "c1", Name: "test-c1"}
	imports := []executor.ImportEntry{
		{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Ref: "main"},
		{Repo: "https://github.com/org/tools", Path: "tools", Ref: "v1"},
	}

	err := p.RunImportInstalls(context.Background(), zap.NewNop(), ctr, imports)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if execCalled {
		t.Error("Exec should not be called when no import has an install command")
	}
}

func TestRunImportInstalls_ExecError(t *testing.T) {
	d := newTestDeps(t)

	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, _ []string) (string, int, error) {
		return "", 0, errors.New("exec failed")
	}

	p := d.pipeline(t)
	ctr := &container.Container{ID: "c1", Name: "test-c1"}
	imports := []executor.ImportEntry{
		{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Install: "./install.sh"},
	}

	err := p.RunImportInstalls(context.Background(), zap.NewNop(), ctr, imports)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "install command for import .ai-workflows") {
		t.Errorf("error should reference import path, got: %v", err)
	}
}

func TestRunImportInstalls_NonZeroExitCode(t *testing.T) {
	d := newTestDeps(t)

	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, _ []string) (string, int, error) {
		return "some output", 1, nil
	}

	p := d.pipeline(t)
	ctr := &container.Container{ID: "c1", Name: "test-c1"}
	imports := []executor.ImportEntry{
		{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Install: "./install.sh"},
	}

	err := p.RunImportInstalls(context.Background(), zap.NewNop(), ctr, imports)
	if err == nil {
		t.Fatal("expected error on non-zero exit code")
	}
	if !strings.Contains(err.Error(), "exited with code 1") {
		t.Errorf("error should include exit code, got: %v", err)
	}
}

func TestRunImportInstalls_NilImports(t *testing.T) {
	d := newTestDeps(t)
	p := d.pipeline(t)
	ctr := &container.Container{ID: "c1", Name: "test-c1"}

	err := p.RunImportInstalls(context.Background(), zap.NewNop(), ctr, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMergeImports_InstallFieldPropagated(t *testing.T) {
	settings := &models.ProjectSettings{
		Repos: []models.RepoSettings{{
			Imports: []models.ImportConfig{
				{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Ref: "main", Install: "proj-install.sh"},
			},
		}},
	}
	repoCfg := &repoconfig.Config{
		Imports: []repoconfig.Import{
			{Repo: "https://github.com/org/tools", Path: "tools", Ref: "v1", Install: "tools/setup.sh"},
		},
	}

	result := executor.MergeImports(settings, repoCfg)
	if len(result) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(result))
	}

	// Sorted by path: .ai-workflows before tools.
	if result[0].Install != "proj-install.sh" {
		t.Errorf("result[0].Install = %q, want %q", result[0].Install, "proj-install.sh")
	}
	if result[1].Install != "tools/setup.sh" {
		t.Errorf("result[1].Install = %q, want %q", result[1].Install, "tools/setup.sh")
	}
}

func TestMergeImports_RepoOverridesInstall(t *testing.T) {
	settings := &models.ProjectSettings{
		Repos: []models.RepoSettings{{
			Imports: []models.ImportConfig{
				{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Install: "old-install.sh"},
			},
		}},
	}
	repoCfg := &repoconfig.Config{
		Imports: []repoconfig.Import{
			{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Install: "new-install.sh"},
		},
	}

	result := executor.MergeImports(settings, repoCfg)
	if len(result) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(result))
	}
	if result[0].Install != "new-install.sh" {
		t.Errorf("Install = %q, want %q (repo-level should override)", result[0].Install, "new-install.sh")
	}
}

func TestExecuteNewTicket_RunsImportInstallAfterContainerStart(t *testing.T) {
	d := newTestDeps(t)

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{{
				Owner:      "org",
				Repo:       "repo",
				CloneURL:   "https://github.com/org/repo.git",
				Imports:    []models.ImportConfig{{Repo: "https://github.com/org/wf", Path: ".ai-workflows", Ref: "main", Install: ".ai-workflows/install.sh"}},
				BaseBranch: "main",
			}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	d.git.CloneImportFunc = func(_, _, _ string) error { return nil }

	// Track the order: container start, then install exec, then AI exec.
	var events []string
	d.containers.StartFunc = func(_ context.Context, _ *container.Config, _, _ string, _ map[string]string) (*container.Container, error) {
		events = append(events, "start")
		return &container.Container{ID: "c1", Name: "test-c1"}, nil
	}
	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, cmd []string) (string, int, error) {
		if len(cmd) == 3 && strings.Contains(cmd[2], "install.sh") {
			events = append(events, "install")
		} else {
			events = append(events, "ai-exec")
		}
		return "", 0, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify ordering: start → install → ai-exec.
	if len(events) < 3 {
		t.Fatalf("expected at least 3 events, got %d: %v", len(events), events)
	}
	if events[0] != "start" {
		t.Errorf("events[0] = %q, want start", events[0])
	}
	if events[1] != "install" {
		t.Errorf("events[1] = %q, want install", events[1])
	}
	if events[2] != "ai-exec" {
		t.Errorf("events[2] = %q, want ai-exec", events[2])
	}
}

func TestExecuteNewTicket_ImportInstallFailure_StopsContainer(t *testing.T) {
	d := newTestDeps(t)

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{{
				Owner:      "org",
				Repo:       "repo",
				CloneURL:   "https://github.com/org/repo.git",
				Imports:    []models.ImportConfig{{Repo: "https://github.com/org/wf", Path: ".ai-workflows", Install: "install.sh"}},
				BaseBranch: "main",
			}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	d.git.CloneImportFunc = func(_, _, _ string) error { return nil }

	d.containers.ExecFunc = func(_ context.Context, _ *container.Container, cmd []string) (string, int, error) {
		if len(cmd) == 3 && strings.Contains(cmd[2], "install.sh") {
			return "install failed", 1, nil
		}
		return "", 0, nil
	}

	stopCalled := false
	d.containers.StopFunc = func(_ context.Context, _ *container.Container) error {
		stopCalled = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error from install failure")
	}
	if !strings.Contains(err.Error(), "import install") {
		t.Errorf("error should mention import install, got: %v", err)
	}
	if !stopCalled {
		t.Error("container should be stopped on install failure")
	}
}

// --- readPRDescription ---

func TestReadPRDescription_FileExists(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, ".ai-session")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := "Fix NPE in UserService\n\n## Summary\nAdded null check for photo field.\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "pr.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	pr := executor.ReadPRDescription(dir)
	if pr == nil {
		t.Fatal("expected non-nil PRDescription")
	}
	if pr.Title != "Fix NPE in UserService" {
		t.Errorf("Title = %q, want %q", pr.Title, "Fix NPE in UserService")
	}
	if !strings.Contains(pr.Body, "Added null check") {
		t.Errorf("Body should contain 'Added null check', got: %s", pr.Body)
	}
}

func TestReadPRDescription_FileMissing(t *testing.T) {
	dir := t.TempDir()
	pr := executor.ReadPRDescription(dir)
	if pr != nil {
		t.Errorf("expected nil when file missing, got %+v", pr)
	}
}

func TestReadPRDescription_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, ".ai-session")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "pr.md"), []byte("  \n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pr := executor.ReadPRDescription(dir)
	if pr != nil {
		t.Errorf("expected nil for whitespace-only file, got %+v", pr)
	}
}

func TestReadPRDescription_TitleOnly(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, ".ai-session")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "pr.md"), []byte("Just a title\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pr := executor.ReadPRDescription(dir)
	if pr == nil {
		t.Fatal("expected non-nil PRDescription")
	}
	if pr.Title != "Just a title" {
		t.Errorf("Title = %q, want %q", pr.Title, "Just a title")
	}
	if pr.Body != "" {
		t.Errorf("Body = %q, want empty", pr.Body)
	}
}

func TestReadPRDescription_StripsMarkdownHeading(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"h1 prefix", "# Fix the bug\n\nBody text.", "Fix the bug"},
		{"h2 prefix", "## Fix the bug\n\nBody text.", "Fix the bug"},
		{"h1 with ticket key", "# EDM-2747: Fix compose apps\n\nBody.", "EDM-2747: Fix compose apps"},
		{"no heading", "Fix the bug\n\nBody text.", "Fix the bug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			sessionDir := filepath.Join(dir, ".ai-session")
			if err := os.MkdirAll(sessionDir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessionDir, "pr.md"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}

			pr := executor.ReadPRDescription(dir)
			if pr == nil {
				t.Fatal("expected non-nil PRDescription")
			}
			if pr.Title != tt.want {
				t.Errorf("Title = %q, want %q", pr.Title, tt.want)
			}
		})
	}
}

// --- parsePRContent ---

func TestParsePRContent_LabeledTitle(t *testing.T) {
	content := "# PR Description\n\n**Title:** Fix AAP OAuth app configuration precedence\n\n**Jira Ticket:** EDM-3430"
	title, body := executor.ParsePRContent(content)

	if title != "Fix AAP OAuth app configuration precedence" {
		t.Errorf("Title = %q, want %q", title, "Fix AAP OAuth app configuration precedence")
	}
	// Body should exclude metadata lines.
	if strings.Contains(body, "PR Description") {
		t.Error("body should not contain generic heading")
	}
	if strings.Contains(body, "Jira Ticket") {
		t.Error("body should not contain Jira ticket metadata")
	}
}

func TestParsePRContent_LabeledTitlePlainText(t *testing.T) {
	content := "Title: Fix the bug\n\nSome body content."
	title, body := executor.ParsePRContent(content)

	if title != "Fix the bug" {
		t.Errorf("Title = %q, want %q", title, "Fix the bug")
	}
	if body != "Some body content." {
		t.Errorf("Body = %q, want %q", body, "Some body content.")
	}
}

func TestParsePRContent_HeadingSection(t *testing.T) {
	content := "## Title\n\n**[EDM-3430]: Fix OAuth config**\n\n## Summary\nThe fix addresses the issue."
	title, body := executor.ParsePRContent(content)

	if title != "[EDM-3430]: Fix OAuth config" {
		t.Errorf("Title = %q, want %q", title, "[EDM-3430]: Fix OAuth config")
	}
	if !strings.Contains(body, "The fix addresses") {
		t.Errorf("Body should contain summary content, got: %q", body)
	}
	if strings.Contains(body, "## Title") {
		t.Error("body should not contain the ## Title heading")
	}
}

func TestParsePRContent_FallbackFirstLine(t *testing.T) {
	content := "Fix NPE in UserService\n\n## Summary\nAdded null check."
	title, body := executor.ParsePRContent(content)

	if title != "Fix NPE in UserService" {
		t.Errorf("Title = %q, want %q", title, "Fix NPE in UserService")
	}
	if !strings.Contains(body, "Added null check") {
		t.Errorf("Body should contain 'Added null check', got: %q", body)
	}
}

func TestParsePRContent_FallbackStripsHeadingPrefix(t *testing.T) {
	content := "# Fix the bug\n\nBody text."
	title, body := executor.ParsePRContent(content)

	if title != "Fix the bug" {
		t.Errorf("Title = %q, want %q", title, "Fix the bug")
	}
	if body != "Body text." {
		t.Errorf("Body = %q, want %q", body, "Body text.")
	}
}

func TestParsePRContent_LabeledTitleWithBody(t *testing.T) {
	content := "# PR Description\n\n**Title:** Fix the bug\n\n## Summary\nDetailed explanation here."
	title, body := executor.ParsePRContent(content)

	if title != "Fix the bug" {
		t.Errorf("Title = %q, want %q", title, "Fix the bug")
	}
	if !strings.Contains(body, "Detailed explanation") {
		t.Errorf("Body should contain summary, got: %q", body)
	}
}

func TestParsePRContent_GenericHeadingReturnsEmptyTitle(t *testing.T) {
	headings := []string{
		"## Summary",
		"# Summary",
		"## Description",
		"## Test plan",
		"## Overview",
		"## Changes",
	}
	for _, h := range headings {
		t.Run(h, func(t *testing.T) {
			content := h + "\n\nSome body content."
			title, body := executor.ParsePRContent(content)
			if title != "" {
				t.Errorf("Title = %q, want empty for generic heading", title)
			}
			if !strings.Contains(body, "Some body content") {
				t.Errorf("Body should contain content, got: %q", body)
			}
		})
	}
}

func TestParsePRContent_LabeledTitleIgnoredAfterHeader(t *testing.T) {
	// A "Title:" line deep in the body should not be picked up as the PR title.
	var lines []string
	for i := range 15 {
		lines = append(lines, fmt.Sprintf("Line %d of body content", i+1))
	}
	lines = append(lines, "Title: This should NOT be the title")
	content := strings.Join(lines, "\n")

	title, _ := executor.ParsePRContent(content)
	if title == "This should NOT be the title" {
		t.Error("Strategy 1 matched a Title: line outside the header region")
	}
}

func TestReadPRDescription_GenericHeadingNoTitle(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, ".ai-session")
	if err := os.MkdirAll(sessionDir, 0o750); err != nil {
		t.Fatal(err)
	}
	content := "## Summary\n- Fixed the bug\n- Added tests\n\n## Test plan\n- [ ] Manual test"
	if err := os.WriteFile(filepath.Join(sessionDir, "pr.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	pr := executor.ReadPRDescription(dir)
	if pr == nil {
		t.Fatal("expected non-nil PRDescription with body")
	}
	if pr.Title != "" {
		t.Errorf("Title = %q, want empty", pr.Title)
	}
	if !strings.Contains(pr.Body, "Fixed the bug") {
		t.Errorf("Body should contain content, got: %q", pr.Body)
	}
}

// --- buildPRContent ---

func TestBuildPRContent_Default(t *testing.T) {
	workItem := &models.WorkItem{
		Key:         "PROJ-1",
		Summary:     "Fix bug",
		Description: "Detailed description.",
	}
	title, body := executor.BuildPRContent(workItem, "PROJ-1", "", nil)
	if title != "PROJ-1: Fix bug" {
		t.Errorf("Title = %q, want %q", title, "PROJ-1: Fix bug")
	}
	if !strings.Contains(body, "Resolves PROJ-1") {
		t.Error("body should contain 'Resolves PROJ-1'")
	}
	if !strings.Contains(body, "Detailed description.") {
		t.Error("body should contain description")
	}
}

func TestBuildPRContent_WithAIPR(t *testing.T) {
	workItem := &models.WorkItem{Key: "PROJ-1", Summary: "Fix bug"}
	aiPR := &executor.PRDescription{
		Title: "Fix null pointer in getProfile()",
		Body:  "## Summary\nAdded null check.",
	}
	title, body := executor.BuildPRContent(workItem, "PROJ-1", "", aiPR)
	if title != "PROJ-1: Fix null pointer in getProfile()" {
		t.Errorf("Title = %q, want %q", title, "PROJ-1: Fix null pointer in getProfile()")
	}
	if !strings.Contains(body, "Added null check") {
		t.Error("body should contain AI-generated content")
	}
	// Jira description should NOT be present.
	if strings.Contains(body, "Fix bug") {
		t.Error("body should not contain Jira summary when AI PR is used")
	}
}

func TestBuildPRContent_SecurityLevelIgnoresAIPR(t *testing.T) {
	workItem := &models.WorkItem{
		Key:           "SEC-1",
		Summary:       "Fix auth bypass",
		SecurityLevel: "Embargoed",
	}
	aiPR := &executor.PRDescription{
		Title: "Fix auth bypass in login handler",
		Body:  "The auth handler skips validation when...",
	}
	title, body := executor.BuildPRContent(workItem, "SEC-1", "", aiPR)
	if !strings.Contains(title, "Security fix") {
		t.Errorf("Title should be redacted, got: %s", title)
	}
	if strings.Contains(body, "auth bypass") {
		t.Error("body should not contain AI content for security-level tickets")
	}
	if !strings.Contains(body, "redacted") {
		t.Error("body should mention redaction")
	}
}

func TestBuildPRContent_AIPREmptyTitle_UsesJiraTitleWithAIBody(t *testing.T) {
	workItem := &models.WorkItem{Key: "PROJ-1", Summary: "Fix bug"}
	aiPR := &executor.PRDescription{Title: "", Body: "## Summary\nAI-generated body."}
	title, body := executor.BuildPRContent(workItem, "PROJ-1", "", aiPR)
	if title != "PROJ-1: Fix bug" {
		t.Errorf("Title = %q, want Jira fallback %q", title, "PROJ-1: Fix bug")
	}
	if !strings.Contains(body, "AI-generated body") {
		t.Errorf("Body should contain AI content, got: %q", body)
	}
}

func TestBuildPRContent_AIPREmptyTitleAndBody_FallsBackCompletely(t *testing.T) {
	workItem := &models.WorkItem{Key: "PROJ-1", Summary: "Fix bug", Description: "Details."}
	aiPR := &executor.PRDescription{Title: "", Body: ""}
	title, body := executor.BuildPRContent(workItem, "PROJ-1", "", aiPR)
	if title != "PROJ-1: Fix bug" {
		t.Errorf("Title = %q, want Jira fallback %q", title, "PROJ-1: Fix bug")
	}
	if !strings.Contains(body, "Details.") {
		t.Errorf("Body should contain Jira description, got: %q", body)
	}
}

func TestBuildPRContent_AITitleWithTicketKey(t *testing.T) {
	workItem := &models.WorkItem{Key: "EDM-2747", Summary: "Fix bug"}
	aiPR := &executor.PRDescription{
		Title: "EDM-2747: Differentiate manual stop from completion",
		Body:  "Root cause analysis.",
	}
	title, _ := executor.BuildPRContent(workItem, "EDM-2747", "", aiPR)
	want := "EDM-2747: Differentiate manual stop from completion"
	if title != want {
		t.Errorf("Title = %q, want %q (no duplicate key)", title, want)
	}
}

func TestBuildPRContent_AITitleWithBracketedTicketKey(t *testing.T) {
	workItem := &models.WorkItem{Key: "EDM-3430", Summary: "Fix bug"}
	aiPR := &executor.PRDescription{
		Title: "[EDM-3430]: Fix OAuth config precedence",
		Body:  "Root cause analysis.",
	}
	title, _ := executor.BuildPRContent(workItem, "EDM-3430", "", aiPR)
	want := "EDM-3430: Fix OAuth config precedence"
	if title != want {
		t.Errorf("Title = %q, want %q (no duplicate key)", title, want)
	}
}

func TestBuildPRContent_WithTitlePrefix(t *testing.T) {
	workItem := &models.WorkItem{Key: "PROJ-1", Summary: "Fix bug"}
	aiPR := &executor.PRDescription{Title: "AI title", Body: "AI body"}
	title, _ := executor.BuildPRContent(workItem, "PROJ-1", "[bot]", aiPR)
	if title != "[bot] PROJ-1: AI title" {
		t.Errorf("Title = %q, want %q", title, "[bot] PROJ-1: AI title")
	}
}

// --- Attachment download ---

func TestExecuteNewTicket_DownloadsAttachments(t *testing.T) {
	d := newTestDeps(t)

	// Return work item with attachments.
	d.tracker.GetWorkItemFunc = func(key string) (*models.WorkItem, error) {
		return &models.WorkItem{
			Key:        key,
			Summary:    "Bug with logs",
			Type:       "Bug",
			Components: []string{},
			Labels:     []string{},
			Attachments: []models.Attachment{
				{Filename: "crash.log", MimeType: "text/plain", Size: 100, URL: "https://jira.example.com/att/1"},
				{Filename: "config.yaml", MimeType: "text/yaml", Size: 50, URL: "https://jira.example.com/att/2"},
			},
		}, nil
	}

	// Track download calls.
	var downloadedURLs []string
	d.tracker.DownloadAttachmentFunc = func(url string) ([]byte, error) {
		downloadedURLs = append(downloadedURLs, url)
		if strings.Contains(url, "att/1") {
			return []byte("stack trace here"), nil
		}
		return []byte("key: value"), nil
	}

	// Capture WriteIssue call to verify attachment files are passed.
	var gotAttachments []string
	d.taskWriter.WriteIssueFunc = func(workItem models.WorkItem, dir string, attachmentFiles []string, _ []models.Comment) error {
		gotAttachments = attachmentFiles
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-ATT"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify both attachments were downloaded.
	if len(downloadedURLs) != 2 {
		t.Fatalf("expected 2 downloads, got %d", len(downloadedURLs))
	}

	// Verify WriteIssue received the attachment filenames.
	if len(gotAttachments) != 2 {
		t.Fatalf("expected 2 attachment files, got %d: %v", len(gotAttachments), gotAttachments)
	}
	if gotAttachments[0] != "crash.log" || gotAttachments[1] != "config.yaml" {
		t.Errorf("attachment files = %v, want [crash.log config.yaml]", gotAttachments)
	}

	// Verify files exist on disk.
	for _, name := range []string{"crash.log", "config.yaml"} {
		path := filepath.Join(d.wsDir, ".ai-session", "attachments", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected attachment file %s to exist: %v", name, err)
		}
	}
}

func TestExecuteNewTicket_SkipsLargeAttachments(t *testing.T) {
	d := newTestDeps(t)

	d.tracker.GetWorkItemFunc = func(key string) (*models.WorkItem, error) {
		return &models.WorkItem{
			Key:        key,
			Summary:    "Large file",
			Type:       "Bug",
			Components: []string{},
			Labels:     []string{},
			Attachments: []models.Attachment{
				{Filename: "small.log", MimeType: "text/plain", Size: 100, URL: "https://jira.example.com/att/1"},
				{Filename: "huge.bin", MimeType: "application/octet-stream", Size: 10 << 20, URL: "https://jira.example.com/att/2"},
			},
		}, nil
	}

	var downloadedURLs []string
	d.tracker.DownloadAttachmentFunc = func(url string) ([]byte, error) {
		downloadedURLs = append(downloadedURLs, url)
		return []byte("data"), nil
	}

	var gotAttachments []string
	d.taskWriter.WriteIssueFunc = func(_ models.WorkItem, _ string, attachmentFiles []string, _ []models.Comment) error {
		gotAttachments = attachmentFiles
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-BIG"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the small file should have been downloaded.
	if len(downloadedURLs) != 1 {
		t.Fatalf("expected 1 download, got %d", len(downloadedURLs))
	}
	if len(gotAttachments) != 1 || gotAttachments[0] != "small.log" {
		t.Errorf("attachment files = %v, want [small.log]", gotAttachments)
	}
}

func TestExecuteNewTicket_NoAttachments_NoSection(t *testing.T) {
	d := newTestDeps(t)

	var gotAttachments []string
	d.taskWriter.WriteIssueFunc = func(_ models.WorkItem, _ string, attachmentFiles []string, _ []models.Comment) error {
		gotAttachments = attachmentFiles
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-NONE"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No attachments means nil passed to WriteIssue.
	if gotAttachments != nil {
		t.Errorf("expected nil attachment files, got %v", gotAttachments)
	}
}

func TestExecuteNewTicket_SanitizesAttachmentFilename(t *testing.T) {
	d := newTestDeps(t)

	d.tracker.GetWorkItemFunc = func(key string) (*models.WorkItem, error) {
		return &models.WorkItem{
			Key:        key,
			Summary:    "Path traversal test",
			Type:       "Bug",
			Components: []string{},
			Labels:     []string{},
			Attachments: []models.Attachment{
				{Filename: "../../etc/passwd", MimeType: "text/plain", Size: 50, URL: "https://jira.example.com/att/1"},
			},
		}, nil
	}

	d.tracker.DownloadAttachmentFunc = func(string) ([]byte, error) {
		return []byte("safe content"), nil
	}

	var gotAttachments []string
	d.taskWriter.WriteIssueFunc = func(_ models.WorkItem, _ string, attachmentFiles []string, _ []models.Comment) error {
		gotAttachments = attachmentFiles
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-SEC"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Filename should be sanitized to just "passwd".
	if len(gotAttachments) != 1 || gotAttachments[0] != "passwd" {
		t.Errorf("attachment files = %v, want [passwd]", gotAttachments)
	}

	// File should be in the attachments dir, not in ../../etc/.
	path := filepath.Join(d.wsDir, ".ai-session", "attachments", "passwd")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected sanitized file at %s: %v", path, err)
	}
}

func TestExecuteNewTicket_SkipsExistingAttachments(t *testing.T) {
	d := newTestDeps(t)

	d.tracker.GetWorkItemFunc = func(key string) (*models.WorkItem, error) {
		return &models.WorkItem{
			Key:        key,
			Summary:    "Rerun with cached attachments",
			Type:       "Bug",
			Components: []string{},
			Labels:     []string{},
			Attachments: []models.Attachment{
				{Filename: "existing.log", MimeType: "text/plain", Size: 100, URL: "https://jira.example.com/att/1"},
				{Filename: "new.log", MimeType: "text/plain", Size: 80, URL: "https://jira.example.com/att/2"},
			},
		}, nil
	}

	// Pre-create the first attachment on disk.
	attDir := filepath.Join(d.wsDir, ".ai-session", "attachments")
	if err := os.MkdirAll(attDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attDir, "existing.log"), []byte("old data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Track download calls — only new.log should be downloaded.
	var downloadedURLs []string
	d.tracker.DownloadAttachmentFunc = func(url string) ([]byte, error) {
		downloadedURLs = append(downloadedURLs, url)
		return []byte("new data"), nil
	}

	var gotAttachments []string
	d.taskWriter.WriteIssueFunc = func(_ models.WorkItem, _ string, attachmentFiles []string, _ []models.Comment) error {
		gotAttachments = attachmentFiles
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-CACHE"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the new attachment should have been downloaded.
	if len(downloadedURLs) != 1 {
		t.Fatalf("expected 1 download, got %d: %v", len(downloadedURLs), downloadedURLs)
	}
	if !strings.Contains(downloadedURLs[0], "att/2") {
		t.Errorf("expected att/2 to be downloaded, got %s", downloadedURLs[0])
	}

	// Both attachments should be reported (existing + new).
	if len(gotAttachments) != 2 {
		t.Fatalf("expected 2 attachment files, got %d: %v", len(gotAttachments), gotAttachments)
	}
	if gotAttachments[0] != "existing.log" || gotAttachments[1] != "new.log" {
		t.Errorf("attachment files = %v, want [existing.log new.log]", gotAttachments)
	}
}

// --- Fork-based workflow tests ---

func TestNewTicketPipeline_ForkMode(t *testing.T) {
	d := newTestDeps(t)

	// Configure fork mode via ForkMode + GitHubUsername.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "upstream-org", Repo: "repo", CloneURL: "https://github.com/upstream-org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "bot-fork",
		}, nil
	}

	// Capture arguments to verify fork owner and upstream owner.
	var commitUpstreamOwner, commitOwner, commitRepo string
	d.git.CommitChangesFunc = func(upstreamOwner, owner, repo, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		commitUpstreamOwner = upstreamOwner
		commitOwner = owner
		commitRepo = repo
		return "abc123", nil
	}

	var restoreOwner, restoreRepo string
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		restoreOwner = owner
		restoreRepo = repo
		return nil
	}

	var prParams models.PRParams
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prParams = params
		return &models.PR{Number: 42, URL: "https://github.com/upstream-org/repo/pull/42"}, nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-123"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify CommitChanges uses fork owner.
	if commitOwner != "bot-fork" {
		t.Errorf("CommitChanges owner = %q, want bot-fork", commitOwner)
	}
	if commitRepo != "repo" {
		t.Errorf("CommitChanges repo = %q, want repo", commitRepo)
	}

	// Verify RestoreRemoteAuth uses fork owner.
	if restoreOwner != "bot-fork" {
		t.Errorf("RestoreRemoteAuth owner = %q, want bot-fork", restoreOwner)
	}
	if restoreRepo != "repo" {
		t.Errorf("RestoreRemoteAuth repo = %q, want repo", restoreRepo)
	}

	// Verify PR params: Owner/Repo should be upstream, Head should be "bot-fork:branch".
	if prParams.Owner != "upstream-org" {
		t.Errorf("PR Owner = %q, want upstream-org", prParams.Owner)
	}
	if prParams.Repo != "repo" {
		t.Errorf("PR Repo = %q, want repo", prParams.Repo)
	}
	if !strings.HasPrefix(prParams.Head, "bot-fork:") {
		t.Errorf("PR Head = %q, should start with bot-fork:", prParams.Head)
	}
	if !strings.Contains(prParams.Head, "PROJ-123") {
		t.Errorf("PR Head = %q, should contain ticket key", prParams.Head)
	}

	// Verify upstream owner is passed for tree resolution.
	if commitUpstreamOwner != "upstream-org" {
		t.Errorf("CommitChanges upstreamOwner = %q, want upstream-org", commitUpstreamOwner)
	}

	// Verify result is valid.
	if result.PRURL == "" {
		t.Error("expected non-empty PRURL")
	}
}

func TestPrepareBranch_ForkMode(t *testing.T) {
	d := newTestDeps(t)

	// Configure fork mode.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "upstream-org", Repo: "repo", CloneURL: "https://github.com/upstream-org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "bot-fork",
		}, nil
	}

	// Workspace is reused and remote branch exists.
	d.workspaces.FindOrCreateFunc = func(ticketKey, repoURL string) (string, bool, error) {
		return d.wsDir, true, nil // reused
	}

	var remoteCheckOwner, remoteCheckRepo string
	d.git.RemoteBranchExistsFunc = func(owner, repo, branch string) (bool, error) {
		remoteCheckOwner = owner
		remoteCheckRepo = repo
		return true, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify RemoteBranchExists was called with fork owner.
	if remoteCheckOwner != "bot-fork" {
		t.Errorf("RemoteBranchExists owner = %q, want bot-fork", remoteCheckOwner)
	}
	if remoteCheckRepo != "repo" {
		t.Errorf("RemoteBranchExists repo = %q, want repo", remoteCheckRepo)
	}
}

func TestPrepareBranch_ForkMode_SyncsForkBeforeCreateBranch(t *testing.T) {
	d := newTestDeps(t)

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "upstream-org", Repo: "backend", CloneURL: "https://github.com/upstream-org/backend.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "adalton",
		}, nil
	}

	// Fresh workspace (not reused) triggers CreateBranch path.
	d.workspaces.FindOrCreateFunc = func(ticketKey, repoURL string) (string, bool, error) {
		return d.wsDir, false, nil
	}

	var syncOwner, syncRepo, syncBranch string
	var syncCalled bool
	d.git.SyncForkFunc = func(forkOwner, repo, branch string) error {
		syncCalled = true
		syncOwner = forkOwner
		syncRepo = repo
		syncBranch = branch
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !syncCalled {
		t.Fatal("SyncFork was not called")
	}
	if syncOwner != "adalton" {
		t.Errorf("SyncFork forkOwner = %q, want adalton", syncOwner)
	}
	if syncRepo != "backend" {
		t.Errorf("SyncFork repo = %q, want backend", syncRepo)
	}
	if syncBranch != "main" {
		t.Errorf("SyncFork branch = %q, want main", syncBranch)
	}
}

func TestPrepareBranch_NoFork_SkipsSyncFork(t *testing.T) {
	d := newTestDeps(t)

	// No GitHubUsername = no fork mode.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	d.git.SyncForkFunc = func(forkOwner, repo, branch string) error {
		t.Fatal("SyncFork should not be called without fork mode")
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrepareBranch_ForkMode_SyncForkErrorIsNonFatal(t *testing.T) {
	d := newTestDeps(t)

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "upstream-org", Repo: "repo", CloneURL: "https://github.com/upstream-org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "adalton",
		}, nil
	}

	d.git.SyncForkFunc = func(forkOwner, repo, branch string) error {
		return fmt.Errorf("merge-upstream failed: 403 Forbidden")
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("SyncFork error should be non-fatal, got: %v", err)
	}
}

func TestFeedbackPipeline_ForkMode(t *testing.T) {
	d := newTestDeps(t)

	// Configure fork mode.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "upstream-org", Repo: "repo", CloneURL: "https://github.com/upstream-org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
			ForkMode:         true,
			GitHubUsername:   "bot-fork",
		}, nil
	}

	// Track the head format passed to GetPRForBranch.
	var prHead string
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		prHead = head
		return &models.PRDetails{
			Number: 42,
			Title:  "Fix a bug",
			Branch: "ai-bot/PROJ-1",
			URL:    "https://github.com/upstream-org/repo/pull/42",
		}, nil
	}

	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	// Track RestoreRemoteAuth and FetchRemote calls.
	var restoreCalled, fetchCalled bool
	var restoreOwner, restoreRepo string
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		if !restoreCalled {
			// First call is the fork setup.
			restoreCalled = true
			restoreOwner = owner
			restoreRepo = repo
		}
		return nil
	}
	d.git.FetchRemoteFunc = func(dir string) error {
		fetchCalled = true
		return nil
	}

	// Track CommitChanges owner.
	var commitUpstreamOwner, commitOwner, commitRepo string
	d.git.CommitChangesFunc = func(upstreamOwner, owner, repo, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		commitUpstreamOwner = upstreamOwner
		commitOwner = owner
		commitRepo = repo
		return "abc123", nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify GetPRForBranch received "bot-fork:ai-bot/PROJ-1" head format.
	if prHead != "bot-fork:ai-bot/PROJ-1" {
		t.Errorf("GetPRForBranch head = %q, want bot-fork:ai-bot/PROJ-1", prHead)
	}

	// Verify RestoreRemoteAuth was called with fork owner before switching branch.
	if !restoreCalled {
		t.Fatal("RestoreRemoteAuth should be called to set fork remote")
	}
	if restoreOwner != "bot-fork" {
		t.Errorf("RestoreRemoteAuth owner = %q, want bot-fork", restoreOwner)
	}
	if restoreRepo != "repo" {
		t.Errorf("RestoreRemoteAuth repo = %q, want repo", restoreRepo)
	}

	// Verify FetchRemote was called.
	if !fetchCalled {
		t.Error("FetchRemote should be called in fork mode")
	}

	// Verify CommitChanges uses fork owner with upstream for tree lookup.
	if commitUpstreamOwner != "upstream-org" {
		t.Errorf("CommitChanges upstreamOwner = %q, want upstream-org", commitUpstreamOwner)
	}
	if commitOwner != "bot-fork" {
		t.Errorf("CommitChanges owner = %q, want bot-fork", commitOwner)
	}
	if commitRepo != "repo" {
		t.Errorf("CommitChanges repo = %q, want repo", commitRepo)
	}
}

// --- Feedback ErrNoChanges final attempt ---

func TestFeedbackPipeline_ErrNoChanges_NonFinalAttempt_ReturnsError(t *testing.T) {
	d := newTestDeps(t)

	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Title: "Fix a bug",
			Branch: head, URL: "https://github.com/org/repo/pull/42",
		}, nil
	}
	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 10, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "", services.ErrNoChanges
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "test-key"},
		MaxRetries:      3,
	})

	// AttemptNum 1 (of 4 total) — not the final attempt.
	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})

	if err == nil {
		t.Fatal("expected error on non-final attempt with ErrNoChanges")
	}
	if !strings.Contains(err.Error(), "no committable changes") {
		t.Errorf("error = %q, should mention 'no committable changes'", err.Error())
	}
}

func TestFeedbackPipeline_ErrNoChanges_FinalAttempt_PostsUnableToAddress(t *testing.T) {
	d := newTestDeps(t)

	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Title: "Fix a bug",
			Branch: head, URL: "https://github.com/org/repo/pull/42",
		}, nil
	}

	reviewComment := models.PRComment{
		ID: 10, Author: models.Author{Username: "reviewer"},
		Body: "Fix this", IsReviewComment: true,
	}
	conversationComment := models.PRComment{
		ID: 20, Author: models.Author{Username: "reviewer"},
		Body: "Also fix that", IsReviewComment: false,
	}

	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{reviewComment, conversationComment}, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "", services.ErrNoChanges
	}

	var reviewReplies []int64
	d.git.ReplyToCommentFunc = func(owner, repo string, prNumber int, commentID int64, body string) error {
		reviewReplies = append(reviewReplies, commentID)
		if !strings.Contains(body, "unable to produce code changes") {
			t.Errorf("review reply body = %q, should mention inability", body)
		}
		return nil
	}

	var issueCommentBodies []string
	d.git.PostIssueCommentFunc = func(owner, repo string, prNumber int, body string) (int64, error) {
		issueCommentBodies = append(issueCommentBodies, body)
		return 0, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "test-key"},
		MaxRetries:      3,
	})

	// AttemptNum 4 (> MaxRetries 3) — final attempt.
	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 4,
	})

	if err != nil {
		t.Fatalf("expected nil error on final attempt, got %v", err)
	}

	// Verify review comment got a threaded reply.
	if len(reviewReplies) != 1 || reviewReplies[0] != 10 {
		t.Errorf("review replies = %v, want [10]", reviewReplies)
	}

	// Verify conversation comment got an issue comment with addressed marker.
	if len(issueCommentBodies) != 1 {
		t.Fatalf("expected 1 issue comment, got %d", len(issueCommentBodies))
	}
	if !strings.Contains(issueCommentBodies[0], "unable to produce code changes") {
		t.Error("issue comment should mention inability")
	}
	if !strings.Contains(issueCommentBodies[0], "<!-- addressed: 20 -->") {
		t.Errorf("issue comment should contain addressed marker for comment 20, got: %q", issueCommentBodies[0])
	}
}

func TestFeedbackPipeline_NonForkMode_SkipsFetchRemote(t *testing.T) {
	d := newTestDeps(t)

	// No GitHubUsername set (non-fork mode).
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42,
			Title:  "Fix a bug",
			Branch: "ai-bot/PROJ-1",
			URL:    "https://github.com/org/repo/pull/42",
		}, nil
	}

	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	// Track FetchRemote — should NOT be called in non-fork mode.
	fetchCalled := false
	d.git.FetchRemoteFunc = func(dir string) error {
		fetchCalled = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fetchCalled {
		t.Error("FetchRemote should NOT be called in non-fork mode")
	}
}

// --- Feedback validation label paths ---

func TestFeedbackPipeline_NonZeroExit_AppliesLabel(t *testing.T) {
	d := newTestDeps(t)

	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Title: "Fix a bug",
			Branch: "ai-bot/PROJ-1", URL: "https://github.com/org/repo/pull/42",
		}, nil
	}
	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		return "", 1, nil
	}

	var addedLabels []string
	d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error {
		addedLabels = append(addedLabels, label)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ValidationPassed {
		t.Error("expected ValidationPassed = false")
	}
	if len(addedLabels) != 1 || addedLabels[0] != "ai-nonzero-exit" {
		t.Errorf("addedLabels = %v, want [ai-nonzero-exit]", addedLabels)
	}
}

func TestFeedbackPipeline_ValidationPassed_ClearsLabels(t *testing.T) {
	d := newTestDeps(t)

	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Title: "Fix a bug",
			Branch: "ai-bot/PROJ-1", URL: "https://github.com/org/repo/pull/42",
		}, nil
	}
	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	var removedLabels []string
	d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error {
		removedLabels = append(removedLabels, label)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.ValidationPassed {
		t.Error("expected ValidationPassed = true")
	}
	// Should attempt to remove both validation labels (clearing prior-round labels).
	want := map[string]bool{"ai-validation-failed": true, "ai-nonzero-exit": true}
	for _, l := range removedLabels {
		if want[l] {
			delete(want, l)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing label removals: %v", want)
	}
}

func TestFeedbackPipeline_NoChanges_LabelsUntouched(t *testing.T) {
	d := newTestDeps(t)

	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Title: "Fix a bug",
			Branch: "ai-bot/PROJ-1", URL: "https://github.com/org/repo/pull/42",
		}, nil
	}
	d.git.GetPRCommentsFunc = func(owner, repo string, number int, since time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	labelTouched := false
	d.git.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labelTouched = true
		return nil
	}
	d.git.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error {
		labelTouched = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), &jobmanager.Job{
		ID: "j1", TicketKey: "PROJ-1", Type: jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	})

	// Non-final attempt with no changes returns an error — that's expected.
	if err == nil {
		t.Fatal("expected error for no-changes on non-final attempt, got nil")
	}
	if labelTouched {
		t.Error("validation labels should not be touched when no changes are pushed")
	}
}

// --- Multi-repo new ticket pipeline ---

func newMultiRepoTestDeps(t *testing.T) *testDeps {
	t.Helper()
	wsDir := t.TempDir()

	// Create repo subdirectories so HasChanges can stat them.
	for _, name := range []string{"svc-a", "svc-b", "svc-c"} {
		if err := os.MkdirAll(filepath.Join(wsDir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	return &testDeps{
		tracker: &trackertest.Stub{
			GetWorkItemFunc: func(key string) (*models.WorkItem, error) {
				return &models.WorkItem{
					Key:        key,
					Summary:    "Multi-repo task",
					Type:       "Story",
					Components: []string{},
					Labels:     []string{},
				}, nil
			},
		},
		git: &executortest.StubGitService{
			HasChangesFunc: func(dir, baseBranch string) (bool, error) {
				return true, nil
			},
			CommitChangesFunc: func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
				return "abc123", nil
			},
			CreatePRFunc: func(params models.PRParams) (*models.PR, error) {
				return &models.PR{
					Number: 1,
					URL:    fmt.Sprintf("https://github.com/%s/%s/pull/1", params.Owner, params.Repo),
				}, nil
			},
		},
		containers: &containertest.StubManager{
			ResolveConfigFunc: func(repoDir string, projectOverride *container.SettingsOverride) (*container.Config, error) {
				return &container.Config{Image: "fat-container:latest"}, nil
			},
			StartFunc: func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
				return &container.Container{ID: "c1", Name: "test-c1"}, nil
			},
		},
		workspaces: &workspacetest.Stub{
			FindOrCreateMultiRepoFunc: func(ticketKey string, repos []workspace.RepoEntry, rootRepoURL string) (string, bool, error) {
				return wsDir, false, nil
			},
		},
		taskWriter: &taskfiletest.Stub{},
		projects: &executortest.StubProjectResolver{
			ResolveProjectFunc: func(workItem models.WorkItem) (*models.ProjectSettings, error) {
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
					PRValidationLabels: models.PRValidationLabels{
						ValidationFailed: "ai-validation-failed",
						NonzeroExit:      "ai-nonzero-exit",
					},
				}, nil
			},
		},
		wsDir: wsDir,
	}
}

func TestMultiRepoNewTicket_HappyPath(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	var prRepos []string
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prRepos = append(prRepos, params.Repo)
		return &models.PR{
			Number: len(prRepos),
			URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", params.Owner, params.Repo, len(prRepos)),
		}, nil
	}

	var comments []string
	d.tracker.AddCommentFunc = func(key, body string) error {
		comments = append(comments, body)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 3 repos with changes → 3 PRs.
	if len(prRepos) != 3 {
		t.Fatalf("PRs created = %d, want 3", len(prRepos))
	}
	if !equalSlice(prRepos, []string{"svc-a", "svc-b", "svc-c"}) {
		t.Errorf("PR repos = %v, want [svc-a svc-b svc-c]", prRepos)
	}

	// 3 PR URL comments posted.
	if len(comments) != 3 {
		t.Fatalf("comments = %d, want 3", len(comments))
	}
	for i, c := range comments {
		if !strings.Contains(c, "[AI-BOT-PR]") {
			t.Errorf("comment[%d] = %q, want [AI-BOT-PR] marker", i, c)
		}
	}

	// Status transitions: in-progress, then in-review.
	if len(transitions) != 2 {
		t.Fatalf("transitions = %v, want 2", transitions)
	}
	if transitions[0] != "In Progress" {
		t.Errorf("first transition = %q, want In Progress", transitions[0])
	}
	if transitions[1] != "In Review" {
		t.Errorf("second transition = %q, want In Review", transitions[1])
	}

	// Result uses first PR.
	if result.PRURL == "" {
		t.Error("result.PRURL should not be empty")
	}
	if result.Draft {
		t.Error("expected non-draft")
	}
}

func TestMultiRepoNewTicket_PRURLFieldConfigured(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
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
			PRURLFieldName:   "Git Pull Request",
			PRValidationLabels: models.PRValidationLabels{
				ValidationFailed: "ai-validation-failed",
				NonzeroExit:      "ai-nonzero-exit",
			},
		}, nil
	}

	var prCount int
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prCount++
		return &models.PR{
			Number: prCount,
			URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", params.Owner, params.Repo, prCount),
		}, nil
	}

	var fieldName, fieldValue string
	d.tracker.SetFieldValueFunc = func(key, field, value string) error {
		fieldName = field
		fieldValue = value
		return nil
	}

	var comments []string
	d.tracker.AddCommentFunc = func(key, body string) error {
		comments = append(comments, body)
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if fieldName != "Git Pull Request" {
		t.Errorf("field name = %q, want %q", fieldName, "Git Pull Request")
	}
	wantURL := "https://github.com/org/svc-a/pull/1"
	if fieldValue != wantURL {
		t.Errorf("field value = %q, want %q", fieldValue, wantURL)
	}

	// Remaining 2 PRs posted as comments; first goes in custom field.
	wantComments := []string{
		"[AI-BOT-PR] https://github.com/org/svc-b/pull/2",
		"[AI-BOT-PR] https://github.com/org/svc-c/pull/3",
	}
	if len(comments) != len(wantComments) {
		t.Fatalf("comments = %d, want %d", len(comments), len(wantComments))
	}
	for i, c := range comments {
		if c != wantComments[i] {
			t.Errorf("comment[%d] = %q, want %q", i, c, wantComments[i])
		}
	}
}

func TestMultiRepoNewTicket_PartialChanges(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	// Only svc-b has changes.
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return strings.Contains(dir, "svc-b"), nil
	}

	var prRepos []string
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prRepos = append(prRepos, params.Repo)
		return &models.PR{Number: 1, URL: "https://github.com/org/svc-b/pull/1"}, nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(prRepos) != 1 || prRepos[0] != "svc-b" {
		t.Errorf("PR repos = %v, want [svc-b]", prRepos)
	}

	if result.PRURL != "https://github.com/org/svc-b/pull/1" {
		t.Errorf("result.PRURL = %q, want svc-b URL", result.PRURL)
	}
}

func TestMultiRepoNewTicket_NoChangesInAnyRepo(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	var reverted bool
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		if status == "To Do" {
			reverted = true
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "no changes in any repository") {
		t.Fatalf("expected no-changes error, got %v", err)
	}
	if !reverted {
		t.Error("expected status reverted to todo")
	}
}

func TestMultiRepoNewTicket_UsesWorkspaceLevelContainer(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	var resolvedOverride *container.SettingsOverride
	d.containers.ResolveConfigFunc = func(repoDir string, override *container.SettingsOverride) (*container.Config, error) {
		resolvedOverride = override
		return &container.Config{Image: "fat-container:latest"}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolvedOverride == nil {
		t.Fatal("expected non-nil settings override")
	}
	if resolvedOverride.Image != "fat-container:latest" {
		t.Errorf("override image = %q, want fat-container:latest", resolvedOverride.Image)
	}
}

func TestMultiRepoNewTicket_BranchCreatedPerRepo(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	var branchDirs []string
	d.git.CreateBranchFunc = func(dir, name, baseBranch string) error {
		branchDirs = append(branchDirs, filepath.Base(dir))
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(branchDirs) != 3 {
		t.Fatalf("branches created = %d, want 3 (one per repo)", len(branchDirs))
	}

	want := []string{"svc-a", "svc-b", "svc-c"}
	if !equalSlice(branchDirs, want) {
		t.Errorf("branch dirs = %v, want %v", branchDirs, want)
	}
}

func TestMultiRepoNewTicket_WritesMultiRepoTaskFile(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	var taskWritten bool
	d.taskWriter.WriteMultiRepoNewTicketTaskFunc = func(
		workItem models.WorkItem, wsDir string, repos []taskfile.RepoContext,
	) error {
		taskWritten = true
		if len(repos) != 3 {
			t.Errorf("repo contexts = %d, want 3", len(repos))
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !taskWritten {
		t.Error("WriteMultiRepoNewTicketTask was not called")
	}
}

func TestMultiRepoNewTicket_StripsAndRestoresAuthPerRepo(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	var stripped, restored []string
	d.git.StripRemoteAuthFunc = func(dir string) error {
		stripped = append(stripped, filepath.Base(dir))
		return nil
	}
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		restored = append(restored, filepath.Base(dir))
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"svc-a", "svc-b", "svc-c"}
	if !equalSlice(stripped, want) {
		t.Errorf("stripped = %v, want %v", stripped, want)
	}

	// Restored should have all 3 repos (explicit restore after AI).
	if len(restored) < 3 {
		t.Errorf("restored = %v, want at least 3 entries", restored)
	}
}

func TestMultiRepoNewTicket_CommitPerRepoWithChanges(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	// svc-a and svc-c have changes; svc-b does not.
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		base := filepath.Base(dir)
		return base == "svc-a" || base == "svc-c", nil
	}

	var commitRepos []string
	d.git.CommitChangesFunc = func(upstreamOwner, owner, repo, branch, msg, dir, baseBranch string, author *models.Author, excludes []string, _ bool) (string, error) {
		commitRepos = append(commitRepos, repo)
		return "abc123", nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"svc-a", "svc-c"}
	if !equalSlice(commitRepos, want) {
		t.Errorf("committed repos = %v, want %v", commitRepos, want)
	}
}

func TestMultiRepoNewTicket_ValidationFailed_AppliesLabels(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	writeSessionOutput(t, d.wsDir, executor.SessionOutput{
		ValidationPassed: boolPtr(false),
	})

	var prCount int
	d.git.CreatePRFunc = func(params models.PRParams) (*models.PR, error) {
		prCount++
		if params.Draft {
			t.Errorf("PR[%d].Draft = true, want false", prCount-1)
		}
		return &models.PR{
			Number: prCount,
			URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", params.Owner, params.Repo, prCount),
		}, nil
	}

	var addedLabels []string
	d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error {
		addedLabels = append(addedLabels, label)
		return nil
	}

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.ValidationPassed {
		t.Error("result.ValidationPassed should be false")
	}

	// Each of the 3 PRs should get the ai-validation-failed label.
	wantLabels := 3
	if len(addedLabels) != wantLabels {
		t.Fatalf("addedLabels = %v, want %d entries", addedLabels, wantLabels)
	}
	for i, l := range addedLabels {
		if l != "ai-validation-failed" {
			t.Errorf("addedLabels[%d] = %q, want ai-validation-failed", i, l)
		}
	}

	// Should still transition to In Review.
	found := false
	for _, s := range transitions {
		if s == "In Review" {
			found = true
		}
	}
	if !found {
		t.Errorf("transitions = %v, want In Review", transitions)
	}
}

func TestMultiRepoNewTicket_ForwardsRootRepoURL(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	const wantRootURL = "https://github.com/org/workspace-root.git"

	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{
				{Name: "svc-a", Owner: "org", Repo: "svc-a", CloneURL: "https://github.com/org/svc-a.git", BaseBranch: "main"},
				{Name: "svc-b", Owner: "org", Repo: "svc-b", CloneURL: "https://github.com/org/svc-b.git", BaseBranch: "main"},
			},
			RootRepoURL:      wantRootURL,
			Container:        models.ContainerSettings{Image: "fat-container:latest"},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	var gotRootURL string
	d.workspaces.FindOrCreateMultiRepoFunc = func(ticketKey string, repos []workspace.RepoEntry, rootRepoURL string) (string, bool, error) {
		gotRootURL = rootRepoURL
		return d.wsDir, false, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotRootURL != wantRootURL {
		t.Errorf("rootRepoURL = %q, want %q", gotRootURL, wantRootURL)
	}
}

func TestMultiRepoNewTicket_SkipsMissingRepoDirs(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	// svc-a, svc-b, svc-c dirs exist from newMultiRepoTestDeps.
	// Add svc-d to config but don't create its directory.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{
				{Name: "svc-a", Owner: "org", Repo: "svc-a", CloneURL: "https://github.com/org/svc-a.git", BaseBranch: "main"},
				{Name: "svc-b", Owner: "org", Repo: "svc-b", CloneURL: "https://github.com/org/svc-b.git", BaseBranch: "main"},
				{Name: "svc-c", Owner: "org", Repo: "svc-c", CloneURL: "https://github.com/org/svc-c.git", BaseBranch: "main"},
				{Name: "svc-d", Owner: "org", Repo: "svc-d", CloneURL: "https://github.com/org/svc-d.git", BaseBranch: "main"},
			},
			Container:        models.ContainerSettings{Image: "fat-container:latest"},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	var createdBranches []string
	d.git.CreateBranchFunc = func(dir, branch, base string) error {
		createdBranches = append(createdBranches, filepath.Base(dir))
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// svc-d should be skipped — only svc-a, svc-b, svc-c get branches.
	for _, name := range createdBranches {
		if name == "svc-d" {
			t.Error("created branch for svc-d, but its directory does not exist")
		}
	}
	if len(createdBranches) != 3 {
		t.Errorf("expected branches for 3 repos, got %d: %v", len(createdBranches), createdBranches)
	}
}

func TestMultiRepoNewTicket_AllMissing_ReturnsError(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	// Configure repos that have no directories at all.
	d.projects.ResolveProjectFunc = func(workItem models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{
				{Name: "missing-x", Owner: "org", Repo: "missing-x", CloneURL: "https://github.com/org/missing-x.git", BaseBranch: "main"},
				{Name: "missing-y", Owner: "org", Repo: "missing-y", CloneURL: "https://github.com/org/missing-y.git", BaseBranch: "main"},
			},
			Container:        models.ContainerSettings{Image: "fat-container:latest"},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error when all repo directories are missing")
	}
	if !strings.Contains(err.Error(), "no repo directories found") {
		t.Errorf("unexpected error: %v", err)
	}
}

// --- Clean retry tests ---

func TestExecuteNewTicket_CleanRetry_DeletesBranchAndWorkspace(t *testing.T) {
	d := newTestDeps(t)

	var deletedBranches []string
	d.git.DeleteRemoteBranchFunc = func(owner, repo, branch string) error {
		deletedBranches = append(deletedBranches, owner+"/"+repo+":"+branch)
		return nil
	}

	var cleanedTicket string
	d.workspaces.CleanupFunc = func(ticketKey string) error {
		cleanedTicket = ticketKey
		return nil
	}

	p := d.pipeline(t)
	job := newTicketJob("PROJ-1")
	job.CleanRetry = true
	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deletedBranches) != 1 {
		t.Fatalf("deletedBranches = %v, want 1 entry", deletedBranches)
	}
	if want := "org/repo:ai-bot/PROJ-1"; deletedBranches[0] != want {
		t.Errorf("deleted branch = %q, want %q", deletedBranches[0], want)
	}
	if cleanedTicket != "PROJ-1" {
		t.Errorf("cleaned ticket = %q, want PROJ-1", cleanedTicket)
	}
}

func TestExecuteNewTicket_CleanRetry_CleanupErrorsNonFatal(t *testing.T) {
	d := newTestDeps(t)

	branchDeleteCalled := false
	d.git.DeleteRemoteBranchFunc = func(owner, repo, branch string) error {
		branchDeleteCalled = true
		return errors.New("branch delete failed")
	}

	cleanupCalled := false
	d.workspaces.CleanupFunc = func(ticketKey string) error {
		cleanupCalled = true
		return errors.New("cleanup failed")
	}

	p := d.pipeline(t)
	job := newTicketJob("PROJ-1")
	job.CleanRetry = true
	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("pipeline should succeed despite cleanup errors: %v", err)
	}

	if !branchDeleteCalled {
		t.Error("DeleteRemoteBranch should have been attempted despite error")
	}
	if !cleanupCalled {
		t.Error("Cleanup should have been attempted despite branch delete error")
	}
}

func TestExecuteNewTicket_CleanRetry_MultiRepo_DeletesAllBranches(t *testing.T) {
	d := newMultiRepoTestDeps(t)

	var deletedBranches []string
	d.git.DeleteRemoteBranchFunc = func(owner, repo, branch string) error {
		deletedBranches = append(deletedBranches, owner+"/"+repo+":"+branch)
		return nil
	}

	var cleanedTicket string
	d.workspaces.CleanupFunc = func(ticketKey string) error {
		cleanedTicket = ticketKey
		return nil
	}

	p := d.pipeline(t)
	job := newTicketJob("PROJ-1")
	job.CleanRetry = true
	_, err := p.Execute(context.Background(), job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(deletedBranches) != 3 {
		t.Fatalf("deletedBranches = %v, want 3 entries (one per repo)", deletedBranches)
	}
	wantBranches := []string{
		"org/svc-a:ai-bot/PROJ-1",
		"org/svc-b:ai-bot/PROJ-1",
		"org/svc-c:ai-bot/PROJ-1",
	}
	for i, want := range wantBranches {
		if deletedBranches[i] != want {
			t.Errorf("deletedBranches[%d] = %q, want %q", i, deletedBranches[i], want)
		}
	}
	if cleanedTicket != "PROJ-1" {
		t.Errorf("cleaned ticket = %q, want PROJ-1", cleanedTicket)
	}
}

func TestExecuteNewTicket_NoCleanRetry_SkipsCleanup(t *testing.T) {
	d := newTestDeps(t)

	branchDeleteCalled := false
	d.git.DeleteRemoteBranchFunc = func(owner, repo, branch string) error {
		branchDeleteCalled = true
		return nil
	}

	cleanupCalled := false
	d.workspaces.CleanupFunc = func(ticketKey string) error {
		cleanupCalled = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newTicketJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if branchDeleteCalled {
		t.Error("DeleteRemoteBranch should not be called for normal job")
	}
	if cleanupCalled {
		t.Error("Cleanup should not be called for normal job")
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// testDeps holds the stub dependencies used by test cases. Individual
// tests override specific Func fields to control behavior.
type testDeps struct {
	tracker    *trackertest.Stub
	git        *executortest.StubGitService
	containers *containertest.StubManager
	workspaces *workspacetest.Stub
	taskWriter *taskfiletest.Stub
	projects   *executortest.StubProjectResolver
	wsDir      string
}

func newTestDeps(t *testing.T) *testDeps {
	t.Helper()
	wsDir := t.TempDir()

	return &testDeps{
		tracker: &trackertest.Stub{
			GetWorkItemFunc: func(key string) (*models.WorkItem, error) {
				return &models.WorkItem{
					Key:        key,
					Summary:    "Fix a bug",
					Type:       "Bug",
					Components: []string{},
					Labels:     []string{},
				}, nil
			},
		},
		git: &executortest.StubGitService{
			HasChangesFunc: func(dir, baseBranch string) (bool, error) {
				return true, nil
			},
			CommitChangesFunc: func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
				return "abc123", nil
			},
			CreatePRFunc: func(params models.PRParams) (*models.PR, error) {
				return &models.PR{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
			},
		},
		containers: &containertest.StubManager{
			ResolveConfigFunc: func(repoDir string, projectOverride *container.SettingsOverride) (*container.Config, error) {
				return &container.Config{Image: "test:latest"}, nil
			},
			StartFunc: func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
				return &container.Container{ID: "c1", Name: "test-c1"}, nil
			},
		},
		workspaces: &workspacetest.Stub{
			FindOrCreateFunc: func(ticketKey, repoURL string) (string, bool, error) {
				return wsDir, false, nil
			},
		},
		taskWriter: &taskfiletest.Stub{},
		projects: &executortest.StubProjectResolver{
			ResolveProjectFunc: func(workItem models.WorkItem) (*models.ProjectSettings, error) {
				return &models.ProjectSettings{
					Repos:            []models.RepoSettings{{Owner: "org", Repo: "repo", CloneURL: "https://github.com/org/repo.git", BaseBranch: "main"}},
					InProgressStatus: "In Progress",
					InReviewStatus:   "In Review",
					TodoStatus:       "To Do",
					PRValidationLabels: models.PRValidationLabels{
						ValidationFailed: "ai-validation-failed",
						NonzeroExit:      "ai-nonzero-exit",
					},
				}, nil
			},
		},
		wsDir: wsDir,
	}
}

func (d *testDeps) pipeline(t *testing.T) *executor.Pipeline {
	t.Helper()
	return d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "test-key"},
		MaxRetries:      3,
	})
}

func (d *testDeps) pipelineWithConfig(t *testing.T, cfg executor.Config) *executor.Pipeline {
	t.Helper()
	p, err := executor.NewPipeline(cfg,
		d.tracker, d.git, d.containers, d.workspaces,
		d.taskWriter, d.projects, zap.NewNop())
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

func newTicketJob(ticketKey string) *jobmanager.Job {
	return &jobmanager.Job{
		ID:         "job-1",
		TicketKey:  ticketKey,
		Type:       jobmanager.JobTypeNewTicket,
		AttemptNum: 1,
	}
}

func writeSessionOutput(t *testing.T, wsDir string, output executor.SessionOutput) {
	t.Helper()
	dir := filepath.Join(wsDir, ".ai-session")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-output.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func createTempCredsFile(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "gcp-sa-key-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(f.Name()) })
	return f.Name()
}

// --- FilterTicketComments ---

func TestFilterTicketComments_RemovesBotComments(t *testing.T) {
	comments := []models.Comment{
		{ID: "1", Author: "Human", AuthorEmail: "human@example.com", Body: "This is a real human comment with details."},
		{ID: "2", Author: "Bot", AuthorEmail: "bot@example.com", Body: "AI processing started for ticket PROJ-123."},
		{ID: "3", Author: "Other Human", AuthorEmail: "other@example.com", Body: "I can confirm this is happening."},
	}

	got := executor.FilterTicketComments(comments, "bot@example.com", 20)

	if len(got) != 2 {
		t.Fatalf("got %d comments, want 2", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "3" {
		t.Errorf("unexpected comments: %+v", got)
	}
}

func TestFilterTicketComments_CaseInsensitiveEmail(t *testing.T) {
	comments := []models.Comment{
		{ID: "1", Author: "Bot", AuthorEmail: "Bot@Example.COM", Body: "This should be filtered out completely."},
		{ID: "2", Author: "Human", AuthorEmail: "human@example.com", Body: "This should remain in results."},
	}

	got := executor.FilterTicketComments(comments, "bot@example.com", 20)

	if len(got) != 1 {
		t.Fatalf("got %d comments, want 1", len(got))
	}
	if got[0].ID != "2" {
		t.Errorf("unexpected comment: %+v", got[0])
	}
}

func TestFilterTicketComments_RemovesShortComments(t *testing.T) {
	comments := []models.Comment{
		{ID: "1", Author: "Human", AuthorEmail: "a@b.com", Body: "ok"},
		{ID: "2", Author: "Human", AuthorEmail: "a@b.com", Body: "+1"},
		{ID: "3", Author: "Human", AuthorEmail: "a@b.com", Body: "   thanks   "},
		{ID: "4", Author: "Human", AuthorEmail: "a@b.com", Body: "This is a substantive comment with details."},
	}

	got := executor.FilterTicketComments(comments, "bot@example.com", 20)

	if len(got) != 1 {
		t.Fatalf("got %d comments, want 1", len(got))
	}
	if got[0].ID != "4" {
		t.Errorf("unexpected comment: %+v", got[0])
	}
}

func TestFilterTicketComments_EmptyInput(t *testing.T) {
	got := executor.FilterTicketComments([]models.Comment{}, "bot@example.com", 20)

	if got == nil {
		t.Fatal("expected non-nil slice")
	}
	if len(got) != 0 {
		t.Errorf("got %d comments, want 0", len(got))
	}
}

func TestFilterTicketComments_EmptyJiraUsername(t *testing.T) {
	comments := []models.Comment{
		{ID: "1", Author: "Anyone", AuthorEmail: "any@example.com", Body: "This should not be filtered by email."},
	}

	got := executor.FilterTicketComments(comments, "", 20)

	if len(got) != 1 {
		t.Fatalf("got %d comments, want 1", len(got))
	}
}

func TestFilterTicketComments_BoundaryLength(t *testing.T) {
	comments := []models.Comment{
		{ID: "1", Author: "Human", AuthorEmail: "a@b.com", Body: "exactly 19 chars..."},
		{ID: "2", Author: "Human", AuthorEmail: "a@b.com", Body: "exactly 20 chars...."},
		{ID: "3", Author: "Human", AuthorEmail: "a@b.com", Body: "exactly 21 chars....."},
	}

	got := executor.FilterTicketComments(comments, "bot@example.com", 20)

	if len(got) != 2 {
		t.Fatalf("got %d comments, want 2 (20 and 21 chars)", len(got))
	}
	if got[0].ID != "2" || got[1].ID != "3" {
		t.Errorf("unexpected comments: %+v", got)
	}
}
