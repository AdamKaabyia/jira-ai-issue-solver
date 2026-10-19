package executor_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/container/containertest"
	"jira-ai-issue-solver/executor"
	"jira-ai-issue-solver/executor/executortest"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/services"
	"jira-ai-issue-solver/taskfile"
	"jira-ai-issue-solver/taskfile/taskfiletest"
	"jira-ai-issue-solver/tracker/trackertest"
	"jira-ai-issue-solver/workspace"
	"jira-ai-issue-solver/workspace/workspacetest"
)

// --- Happy path ---

func TestExecuteFeedback_HappyPath(t *testing.T) {
	d := newFeedbackDeps(t)

	var commitBranch string
	d.git.CommitChangesFunc = func(_, _, _, branch, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		commitBranch = branch
		return "abc1234567890", nil
	}

	var repliedIDs []int64
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, commentID int64, body string) error {
		repliedIDs = append(repliedIDs, commentID)
		if !strings.Contains(body, "abc1234") {
			t.Errorf("reply body = %q, should contain short SHA", body)
		}
		return nil
	}

	var syncCalls int
	d.git.SyncWithRemoteFunc = func(dir, branch string, _ []string) error {
		syncCalls++
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.PRURL != "https://github.com/org/repo/pull/42" {
		t.Errorf("PRURL = %q, want github URL", result.PRURL)
	}
	if result.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", result.PRNumber)
	}
	if !result.ValidationPassed {
		t.Error("expected ValidationPassed = true")
	}
	if commitBranch != "ai-bot/PROJ-1" {
		t.Errorf("commit branch = %q, want ai-bot/PROJ-1", commitBranch)
	}
	if len(repliedIDs) != 1 || repliedIDs[0] != 1 {
		t.Errorf("repliedIDs = %v, want [1]", repliedIDs)
	}
	// Sync called twice: once before AI, once after commit.
	if syncCalls != 2 {
		t.Errorf("sync calls = %d, want 2", syncCalls)
	}
}

// --- AI-generated comment responses ---

func TestExecuteFeedback_AIGeneratedReplies(t *testing.T) {
	d := newFeedbackDeps(t)

	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc1234567890", nil
	}

	// AI writes comment-responses.json during the session.
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		writeCommentResponses(t, d.wsDir, `[
			{"comment_id": 1, "response": "Switched to Optional pattern as suggested."}
		]`)
		return "", 0, nil
	}

	var replyBodies []string
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, _ int64, body string) error {
		replyBodies = append(replyBodies, body)
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(replyBodies) != 1 {
		t.Fatalf("reply count = %d, want 1", len(replyBodies))
	}
	if !strings.Contains(replyBodies[0], "Switched to Optional pattern") {
		t.Errorf("reply should contain AI summary, got %q", replyBodies[0])
	}
	if !strings.Contains(replyBodies[0], "abc1234") {
		t.Errorf("reply should still contain commit SHA, got %q", replyBodies[0])
	}
}

func TestExecuteFeedback_FallbackWhenNoResponsesFile(t *testing.T) {
	d := newFeedbackDeps(t)

	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "def5678901234", nil
	}

	var replyBodies []string
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, _ int64, body string) error {
		replyBodies = append(replyBodies, body)
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(replyBodies) != 1 {
		t.Fatalf("reply count = %d, want 1", len(replyBodies))
	}
	// Without AI responses, should fall back to generic message.
	if !strings.Contains(replyBodies[0], "Addressed in def5678") {
		t.Errorf("reply should be generic fallback, got %q", replyBodies[0])
	}
	if strings.Contains(replyBodies[0], "\n") {
		t.Errorf("generic reply should be single line, got %q", replyBodies[0])
	}
}

// --- Workspace recreated (self-healing) ---

func TestExecuteFeedback_WorkspaceRecreated(t *testing.T) {
	d := newFeedbackDeps(t)
	d.workspaces.FindOrCreateFunc = func(ticketKey, repoURL string) (string, bool, error) {
		return d.wsDir, false, nil // newly created
	}

	var branchSwitched bool
	d.git.SwitchBranchFunc = func(dir, name string) error {
		branchSwitched = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !branchSwitched {
		t.Error("expected SwitchBranch to be called for recreated workspace")
	}
}

// --- Sync picks up changes ---

func TestExecuteFeedback_SyncCalledBeforeAI(t *testing.T) {
	d := newFeedbackDeps(t)

	var syncBeforeExec bool
	execCalled := false

	d.git.SyncWithRemoteFunc = func(dir, branch string, _ []string) error {
		if !execCalled {
			syncBeforeExec = true
		}
		return nil
	}
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		execCalled = true
		return "", 0, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !syncBeforeExec {
		t.Error("expected SyncWithRemote to be called before AI execution")
	}
}

// --- No changes from AI ---

func TestExecuteFeedback_NoChanges(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("expected no-changes error, got %v", err)
	}
}

func TestExecuteFeedback_NoChanges_WithCommentResponses(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	// AI writes comment-responses.json during the session (inside the container).
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		writeCommentResponses(t, d.wsDir, `[
			{"comment_id": 1, "response": "No code changes needed — this is already handled."}
		]`)
		return "", 0, nil
	}

	var repliedTo []int64
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, commentID int64, _ string) error {
		repliedTo = append(repliedTo, commentID)
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("expected no error when comment responses exist, got %v", err)
	}
	if len(repliedTo) != 1 || repliedTo[0] != 1 {
		t.Errorf("expected reply to comment 1, got %v", repliedTo)
	}
}

func TestExecuteFeedback_StaleCommentResponsesCleaned(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil
	}

	// Stale file from a prior session — written BEFORE Execute, not
	// by the container. The cleanup should remove it before the AI runs.
	writeCommentResponses(t, d.wsDir, `[
		{"comment_id": 1, "response": "Stale response from prior session"}
	]`)

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("expected no-changes error (stale file should be cleaned), got %v", err)
	}
}

func TestExecuteFeedback_CleanupPreservesSessionContext(t *testing.T) {
	d := newFeedbackDeps(t)

	// Write session-context.md (should survive cleanup).
	ctxPath := filepath.Join(d.wsDir, taskfile.SessionContextPath)
	if err := os.MkdirAll(filepath.Dir(ctxPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ctxPath, []byte("design rationale from initial session"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := d.pipeline(t)
	// Error is expected (no changes produced); we only care that
	// cleanup ran and preserved session-context.md.
	if _, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1")); err != nil {
		t.Logf("Execute returned expected error: %v", err)
	}

	if _, err := os.Stat(ctxPath); os.IsNotExist(err) {
		t.Error("session-context.md should be preserved across feedback sessions")
	}
}

// --- AI timeout ---

func TestExecuteFeedback_SessionTimeout(t *testing.T) {
	d := newFeedbackDeps(t)
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		<-ctx.Done() // wait for session timeout
		return "", 0, ctx.Err()
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		SessionTimeout:  100 * time.Millisecond,
		AIAPIKeys:       map[string]string{"claude": "test-key"},
	})

	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error on timeout")
	}
	if !strings.Contains(err.Error(), "session timeout exceeded") {
		t.Errorf("expected session timeout error, got %v", err)
	}
}

func TestExecuteFeedback_ParentContextCancelled(t *testing.T) {
	d := newFeedbackDeps(t)

	ctx, cancel := context.WithCancel(context.Background())
	d.containers.ExecFunc = func(execCtx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		cancel() // simulate shutdown
		return "", 0, context.Canceled
	}

	p := d.pipeline(t)
	_, err := p.Execute(ctx, newFeedbackJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "job cancelled") {
		t.Fatalf("expected job cancelled error, got %v", err)
	}
}

// --- PR not found ---

func TestExecuteFeedback_PRNotFound(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return nil, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "no open PR found") {
		t.Fatalf("expected PR not found error, got %v", err)
	}
}

// --- No new comments ---

func TestExecuteFeedback_NoNewComments(t *testing.T) {
	d := newFeedbackDeps(t)
	// All comments are from the bot (no actionable feedback).
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "ai-bot"}, Body: "Addressed"},
		}, nil
	}

	containerStarted := false
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		containerStarted = true
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("expected success when no new comments, got %v", err)
	}
	if containerStarted {
		t.Error("container should not start when there are no new comments")
	}
	if result.PRURL != "" {
		t.Errorf("expected empty PRURL, got %q", result.PRURL)
	}
}

// --- Commit failure ---

func TestExecuteFeedback_CommitFails(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "", errors.New("API rate limit")
	}

	var errorComment string
	d.tracker.AddCommentFunc = func(key, body string) error {
		errorComment = body
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "commit changes") {
		t.Fatalf("expected commit error, got %v", err)
	}
	if !strings.Contains(errorComment, "feedback processing failed") {
		t.Errorf("error comment = %q, should mention feedback failure", errorComment)
	}
}

// --- Status NOT reverted on failure ---

func TestExecuteFeedback_StatusNotRevertedOnFailure(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil // trigger failure
	}

	var transitions []string
	d.tracker.TransitionStatusFunc = func(key, status string) error {
		transitions = append(transitions, status)
		return nil
	}

	p := d.pipeline(t)
	_, _ = p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if len(transitions) > 0 {
		t.Errorf("expected no status transitions for feedback failure, got %v", transitions)
	}
}

// --- Error comments disabled ---

func TestExecuteFeedback_ErrorCommentsDisabled(t *testing.T) {
	d := newFeedbackDeps(t)
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
	_, _ = p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if commentPosted {
		t.Error("expected no error comment when disabled")
	}
}

// --- Container stopped on all paths ---

func TestExecuteFeedback_ContainerStoppedOnSuccess(t *testing.T) {
	d := newFeedbackDeps(t)

	stopped := false
	d.containers.StopFunc = func(ctx context.Context, ctr *container.Container) error {
		stopped = true
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stopped {
		t.Error("expected container to be stopped")
	}
}

func TestExecuteFeedback_ContainerStoppedOnFailure(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.HasChangesFunc = func(dir, baseBranch string) (bool, error) {
		return false, nil // trigger failure
	}

	stopped := false
	d.containers.StopFunc = func(ctx context.Context, ctr *container.Container) error {
		stopped = true
		return nil
	}

	p := d.pipeline(t)
	_, _ = p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if !stopped {
		t.Error("expected container to be stopped on failure")
	}
}

// --- Session cost ---

func TestExecuteFeedback_SessionCostCaptured(t *testing.T) {
	d := newFeedbackDeps(t)
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		writeSessionOutput(t, d.wsDir, executor.SessionOutput{
			ExitCode: 0,
			CostUSD:  2.50,
		})
		return "", 0, nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.CostUSD != 2.50 {
		t.Errorf("CostUSD = %f, want 2.50", result.CostUSD)
	}
}

// --- Co-author attribution ---

func TestExecuteFeedback_CoAuthorAttribution(t *testing.T) {
	d := newFeedbackDeps(t)
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
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedCoAuthor == nil || receivedCoAuthor.Name != "Jane Doe" {
		t.Errorf("expected co-author Jane Doe, got %+v", receivedCoAuthor)
	}
}

// --- Comment reply failure is non-fatal ---

func TestExecuteFeedback_ReplyFailureNonFatal(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, _ int64, _ string) error {
		return errors.New("rate limited")
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("expected success despite reply failure, got %v", err)
	}
	if result.PRURL == "" {
		t.Error("expected PR URL in result")
	}
}

// --- Multiple comments categorized correctly ---

func TestExecuteFeedback_MultipleComments(t *testing.T) {
	d := newFeedbackDeps(t)

	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer1"}, Body: "Fix this", IsReviewComment: true},
			{ID: 2, Author: models.Author{Username: "reviewer2"}, Body: "Also fix that", IsReviewComment: true},
			{ID: 3, Author: models.Author{Username: "ai-bot"}, Body: "Addressed", InReplyTo: 1, IsReviewComment: true},
		}, nil
	}

	var taskNewComments []models.PRComment
	d.taskWriter.WriteFeedbackTaskFunc = func(pr models.PRDetails, newC, addrC []models.PRComment, _ []models.CheckRunFailure, dir, _, _ string) error {
		taskNewComments = newC
		return nil
	}

	var repliedIDs []int64
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, commentID int64, _ string) error {
		repliedIDs = append(repliedIDs, commentID)
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only comment 2 is new (comment 1 was addressed by bot reply 3).
	if len(taskNewComments) != 1 || taskNewComments[0].ID != 2 {
		t.Errorf("taskNewComments = %v, want [comment 2]", taskNewComments)
	}
	// Only replied to comment 2 (the new one).
	if len(repliedIDs) != 1 || repliedIDs[0] != 2 {
		t.Errorf("repliedIDs = %v, want [2]", repliedIDs)
	}
}

// --- Conversation comment reply routing ---

func TestExecuteFeedback_ConversationCommentUsesPostIssueComment(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Update docs", IsReviewComment: false},
		}, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc1234567890", nil
	}

	reviewReplyCalled := false
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, _ int64, _ string) error {
		reviewReplyCalled = true
		return nil
	}

	var issueCommentBody string
	d.git.PostIssueCommentFunc = func(_, _ string, _ int, body string) (int64, error) {
		issueCommentBody = body
		return 0, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if reviewReplyCalled {
		t.Error("ReplyToComment should NOT be called for conversation comments")
	}
	if issueCommentBody == "" {
		t.Fatal("PostIssueComment should be called for conversation comments")
	}
	if !strings.Contains(issueCommentBody, "abc1234") {
		t.Errorf("issue comment should contain short SHA, got %q", issueCommentBody)
	}
	if !strings.Contains(issueCommentBody, "<!-- addressed: 100 -->") {
		t.Errorf("issue comment should contain addressed marker, got %q", issueCommentBody)
	}
}

func TestExecuteFeedback_ReviewCommentUsesReplyToComment(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this line", IsReviewComment: true},
		}, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "def5678901234", nil
	}

	var repliedID int64
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, commentID int64, _ string) error {
		repliedID = commentID
		return nil
	}

	issueCommentCalled := false
	d.git.PostIssueCommentFunc = func(_, _ string, _ int, _ string) (int64, error) {
		issueCommentCalled = true
		return 0, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repliedID != 1 {
		t.Errorf("ReplyToComment called with ID %d, want 1", repliedID)
	}
	if issueCommentCalled {
		t.Error("PostIssueComment should NOT be called for review comments")
	}
}

func TestExecuteFeedback_MixedCommentTypes(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix line", IsReviewComment: true},
			{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Update readme", IsReviewComment: false},
		}, nil
	}
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc1234567890", nil
	}

	var reviewRepliedIDs []int64
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, commentID int64, _ string) error {
		reviewRepliedIDs = append(reviewRepliedIDs, commentID)
		return nil
	}

	var issueCommentBodies []string
	d.git.PostIssueCommentFunc = func(_, _ string, _ int, body string) (int64, error) {
		issueCommentBodies = append(issueCommentBodies, body)
		return 0, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reviewRepliedIDs) != 1 || reviewRepliedIDs[0] != 1 {
		t.Errorf("review replies = %v, want [1]", reviewRepliedIDs)
	}
	if len(issueCommentBodies) != 1 {
		t.Fatalf("issue comments count = %d, want 1", len(issueCommentBodies))
	}
	if !strings.Contains(issueCommentBodies[0], "<!-- addressed: 2 -->") {
		t.Errorf("issue comment should contain addressed marker for ID 2, got %q", issueCommentBodies[0])
	}
}

// --- CategorizeComments unit tests ---

func TestCategorizeComments_SeparatesNewAndAddressed(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Old feedback"},
		{ID: 2, Author: models.Author{Username: "bot"}, Body: "Addressed", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "New feedback"},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if len(newC) != 1 || newC[0].ID != 3 {
		t.Errorf("new = %v, want [comment 3]", newC)
	}
	if len(addrC) != 1 || addrC[0].ID != 1 {
		t.Errorf("addressed = %v, want [comment 1]", addrC)
	}
}

func TestCategorizeComments_ExcludesBotComments(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "bot"}, Body: "I did something"},
		{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Looks good"},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if len(newC) != 1 || newC[0].ID != 2 {
		t.Errorf("new = %v, want [comment 2]", newC)
	}
	if len(addrC) != 0 {
		t.Errorf("addressed = %v, want empty", addrC)
	}
}

func TestCategorizeComments_AllNew(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer1"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "reviewer2"}, Body: "Fix that"},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if len(newC) != 2 {
		t.Errorf("new count = %d, want 2", len(newC))
	}
	if len(addrC) != 0 {
		t.Errorf("addressed count = %d, want 0", len(addrC))
	}
}

func TestCategorizeComments_AllAddressed(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		{ID: 2, Author: models.Author{Username: "bot"}, Body: "Done", InReplyTo: 1},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if len(newC) != 0 {
		t.Errorf("new count = %d, want 0", len(newC))
	}
	if len(addrC) != 1 {
		t.Errorf("addressed count = %d, want 1", len(addrC))
	}
}

func TestCategorizeComments_EmptyInput(t *testing.T) {
	newC, addrC := executor.CategorizeComments(nil, "bot")

	if newC == nil {
		t.Error("new should be non-nil")
	}
	if addrC == nil {
		t.Error("addressed should be non-nil")
	}
	if len(newC) != 0 || len(addrC) != 0 {
		t.Error("both slices should be empty")
	}
}

func TestCategorizeComments_NilSliceNormalization(t *testing.T) {
	// All comments from bot -- both outputs would be nil without
	// normalization.
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "bot"}, Body: "Hello"},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if newC == nil {
		t.Error("new should be non-nil (empty slice, not nil)")
	}
	if addrC == nil {
		t.Error("addressed should be non-nil (empty slice, not nil)")
	}
}

func TestCategorizeComments_BotReplyToOwnComment(t *testing.T) {
	// Bot replying to its own comment should not mark anything as
	// addressed.
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "bot"}, Body: "Initial"},
		{ID: 2, Author: models.Author{Username: "bot"}, Body: "Follow-up", InReplyTo: 1},
		{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Please fix"},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if len(newC) != 1 || newC[0].ID != 3 {
		t.Errorf("new = %v, want [comment 3]", newC)
	}
	if len(addrC) != 0 {
		t.Errorf("addressed = %v, want empty", addrC)
	}
}

func TestCategorizeComments_ConversationCommentAddressedViaMarker(t *testing.T) {
	comments := []models.PRComment{
		{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Update docs"},
		{ID: 200, Author: models.Author{Username: "bot"}, Body: "Done.\n<!-- addressed: 100 -->"},
	}

	newC, addrC := executor.CategorizeComments(comments, "bot")

	if len(addrC) != 1 || addrC[0].ID != 100 {
		t.Errorf("addressed = %v, want [comment 100]", addrC)
	}
	if len(newC) != 0 {
		t.Errorf("new = %v, want empty", newC)
	}
}

func TestCategorizeComments_MarkerFromNonBotIgnored(t *testing.T) {
	// A reviewer quoting the marker text should not count.
	comments := []models.PRComment{
		{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Update docs"},
		{ID: 200, Author: models.Author{Username: "reviewer2"}, Body: "<!-- addressed: 100 -->"},
	}

	newC, _ := executor.CategorizeComments(comments, "bot")

	if len(newC) != 2 {
		t.Errorf("new count = %d, want 2 (marker from non-bot ignored)", len(newC))
	}
}

func TestCategorizeComments_NormalizesUsername(t *testing.T) {
	comments := []models.PRComment{
		{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		// Bot username has different casing and [bot] suffix.
		{ID: 2, Author: models.Author{Username: "MyBot[bot]"}, Body: "Done", InReplyTo: 1},
	}

	newC, addrC := executor.CategorizeComments(comments, "mybot")

	if len(addrC) != 1 || addrC[0].ID != 1 {
		t.Errorf("addressed = %v, want [comment 1]", addrC)
	}
	if len(newC) != 0 {
		t.Errorf("new = %v, want empty", newC)
	}
}

// --- Comment filter integration ---

func TestExecuteFeedback_IgnoredUsersFiltered(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			// Only comment is from an ignored user.
			{ID: 1, Author: models.Author{Username: "packit-as-a-service[bot]"}, Body: "/build"},
		}, nil
	}

	containerStarted := false
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		containerStarted = true
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:      "ai-bot",
		DefaultProvider:  "claude",
		AIAPIKeys:        map[string]string{"claude": "test-key"},
		IgnoredUsernames: []string{"packit-as-a-service"},
	})
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("expected success when only ignored users, got %v", err)
	}
	if containerStarted {
		t.Error("container should not start when all comments are from ignored users")
	}
	if result.PRURL != "" {
		t.Errorf("expected empty PRURL, got %q", result.PRURL)
	}
}

func TestExecuteFeedback_KnownBotLoopFiltered(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
			{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
			// Known bot replying to our bot — should be filtered.
			{ID: 3, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Also...", InReplyTo: 2},
		}, nil
	}

	containerStarted := false
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		containerStarted = true
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:       "ai-bot",
		DefaultProvider:   "claude",
		AIAPIKeys:         map[string]string{"claude": "test-key"},
		KnownBotUsernames: []string{"coderabbitai"},
	})
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("expected success when bot loop filtered, got %v", err)
	}
	if containerStarted {
		t.Error("container should not start when only actionable comment is a bot loop")
	}
	if result.PRURL != "" {
		t.Errorf("expected empty PRURL, got %q", result.PRURL)
	}
}

func TestExecuteFeedback_ThreadDepthExceeded(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
			{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
			// Depth at this comment: bot appeared once (ID 2), equals max.
			{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Still wrong", InReplyTo: 2},
		}, nil
	}

	containerStarted := false
	d.containers.StartFunc = func(ctx context.Context, cfg *container.Config, wsDir, ticketKey string, env map[string]string) (*container.Container, error) {
		containerStarted = true
		return &container.Container{ID: "c1", Name: "test"}, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "test-key"},
		MaxThreadDepth:  1,
	})
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("expected success when thread depth exceeded, got %v", err)
	}
	if containerStarted {
		t.Error("container should not start when all comments exceed thread depth")
	}
	if result.PRURL != "" {
		t.Errorf("expected empty PRURL, got %q", result.PRURL)
	}
}

func TestExecuteFeedback_FilterKeepsActionableComments(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			// Ignored user — should be filtered.
			{ID: 1, Author: models.Author{Username: "packit[bot]"}, Body: "/build"},
			// Real reviewer comment — should pass through.
			{ID: 2, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	var taskNewComments []models.PRComment
	d.taskWriter.WriteFeedbackTaskFunc = func(pr models.PRDetails, newC, addrC []models.PRComment, _ []models.CheckRunFailure, dir, _, _ string) error {
		taskNewComments = newC
		return nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:      "ai-bot",
		DefaultProvider:  "claude",
		AIAPIKeys:        map[string]string{"claude": "test-key"},
		IgnoredUsernames: []string{"packit"},
	})
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only comment 2 (reviewer) should be in the task — comment 1 (ignored) filtered out.
	if len(taskNewComments) != 1 || taskNewComments[0].ID != 2 {
		t.Errorf("taskNewComments = %v, want [comment 2 only]", taskNewComments)
	}
}

// --- Auth strip/restore ---

func TestExecuteFeedback_AuthStrippedBeforeAI(t *testing.T) {
	d := newFeedbackDeps(t)

	var sequence []string
	d.git.StripRemoteAuthFunc = func(dir string) error {
		sequence = append(sequence, "strip")
		return nil
	}
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		sequence = append(sequence, "exec")
		return "", 0, nil
	}
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		sequence = append(sequence, "restore")
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// strip → exec → restore (explicit restore, then defer is a no-op because authStripped=false)
	if len(sequence) < 3 {
		t.Fatalf("sequence = %v, want at least [strip exec restore]", sequence)
	}
	if sequence[0] != "strip" {
		t.Errorf("sequence[0] = %q, want strip", sequence[0])
	}
	if sequence[1] != "exec" {
		t.Errorf("sequence[1] = %q, want exec", sequence[1])
	}
	if sequence[2] != "restore" {
		t.Errorf("sequence[2] = %q, want restore", sequence[2])
	}
}

func TestExecuteFeedback_AuthRestoredOnExecFailure(t *testing.T) {
	d := newFeedbackDeps(t)

	var restored bool
	d.git.RestoreRemoteAuthFunc = func(dir, owner, repo string) error {
		restored = true
		return nil
	}
	d.containers.ExecFunc = func(ctx context.Context, ctr *container.Container, cmd []string) (string, int, error) {
		return "", 0, errors.New("AI crashed")
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil {
		t.Fatal("expected error from exec failure")
	}
	if !restored {
		t.Error("expected RestoreRemoteAuth to be called on exec failure")
	}
}

// --- ErrNoChanges in feedback ---

func TestExecuteFeedback_ErrNoChanges_ReturnsError(t *testing.T) {
	d := newFeedbackDeps(t)
	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "", services.ErrNoChanges
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:     "ai-bot",
		DefaultProvider: "claude",
		AIAPIKeys:       map[string]string{"claude": "test-key"},
		MaxRetries:      3,
	})
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil {
		t.Fatal("expected error for ErrNoChanges")
	}
	if !strings.Contains(err.Error(), "no committable changes") {
		t.Errorf("error = %q, want to mention no committable changes", err.Error())
	}
}

// --- helpers ---

func newFeedbackDeps(t *testing.T) *testDeps {
	t.Helper()
	d := newTestDeps(t)

	// Override defaults for feedback-specific methods.
	d.workspaces.FindOrCreateFunc = func(ticketKey, repoURL string) (string, bool, error) {
		return d.wsDir, true, nil // reused workspace
	}
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42,
			Title:  "Fix a bug",
			Branch: head,
			URL:    "https://github.com/org/repo/pull/42",
		}, nil
	}
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Please fix this", IsReviewComment: true},
		}, nil
	}

	return d
}

func writeCommentResponses(t *testing.T, dir, content string) {
	t.Helper()
	path := filepath.Join(dir, taskfile.CommentResponsesPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFeedbackJob(ticketKey string) *jobmanager.Job {
	return &jobmanager.Job{
		ID:         "job-1",
		TicketKey:  ticketKey,
		Type:       jobmanager.JobTypeFeedback,
		AttemptNum: 1,
	}
}

// --- Multi-repo feedback ---

func newMultiRepoFeedbackDeps(t *testing.T) *testDeps {
	t.Helper()
	wsDir := t.TempDir()

	for _, name := range []string{"svc-a", "svc-b"} {
		if err := os.MkdirAll(filepath.Join(wsDir, name), 0o750); err != nil {
			t.Fatal(err)
		}
	}

	return &testDeps{
		tracker: &trackertest.Stub{
			GetWorkItemFunc: func(key string) (*models.WorkItem, error) {
				return &models.WorkItem{
					Key: key, Summary: "Multi-repo bug", Type: "Bug",
					Components: []string{}, Labels: []string{},
				}, nil
			},
		},
		git: &executortest.StubGitService{
			HasChangesFunc: func(dir, baseBranch string) (bool, error) {
				return true, nil
			},
			CommitChangesFunc: func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
				return "def456", nil
			},
			GetPRForBranchFunc: func(owner, repo, head string) (*models.PRDetails, error) {
				return &models.PRDetails{
					Number: 10, Title: "Fix", Branch: head,
					URL: fmt.Sprintf("https://github.com/%s/%s/pull/10", owner, repo),
				}, nil
			},
			GetPRCommentsFunc: func(_, repo string, _ int, _ time.Time) ([]models.PRComment, error) {
				if repo == "svc-a" {
					return []models.PRComment{
						{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Fix A", IsReviewComment: true},
					}, nil
				}
				return []models.PRComment{}, nil
			},
		},
		containers: &containertest.StubManager{
			ResolveConfigFunc: func(_ string, _ *container.SettingsOverride) (*container.Config, error) {
				return &container.Config{Image: "test:latest"}, nil
			},
			StartFunc: func(_ context.Context, _ *container.Config, _, _ string, _ map[string]string) (*container.Container, error) {
				return &container.Container{ID: "c1", Name: "test-c1"}, nil
			},
		},
		workspaces: &workspacetest.Stub{
			FindOrCreateMultiRepoFunc: func(ticketKey string, repos []workspace.RepoEntry, rootRepoURL string) (string, bool, error) {
				return wsDir, true, nil
			},
		},
		taskWriter: &taskfiletest.Stub{},
		projects: &executortest.StubProjectResolver{
			ResolveProjectFunc: func(_ models.WorkItem) (*models.ProjectSettings, error) {
				return &models.ProjectSettings{
					Repos: []models.RepoSettings{
						{Name: "svc-a", Owner: "org", Repo: "svc-a", CloneURL: "https://github.com/org/svc-a.git", BaseBranch: "main"},
						{Name: "svc-b", Owner: "org", Repo: "svc-b", CloneURL: "https://github.com/org/svc-b.git", BaseBranch: "main"},
					},
					Container:        models.ContainerSettings{Image: "fat:latest"},
					InProgressStatus: "In Progress",
					InReviewStatus:   "In Review",
					TodoStatus:       "To Do",
				}, nil
			},
		},
		wsDir: wsDir,
	}
}

func TestMultiRepoFeedback_HappyPath(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	var repliedTo []int64
	d.git.ReplyToCommentFunc = func(_, _ string, _ int, commentID int64, _ string) error {
		repliedTo = append(repliedTo, commentID)
		return nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Comment 100 on svc-a should be replied to.
	if len(repliedTo) != 1 || repliedTo[0] != 100 {
		t.Errorf("replied to comments %v, want [100]", repliedTo)
	}

	if result.PRURL == "" {
		t.Error("result.PRURL should not be empty")
	}
}

func TestMultiRepoFeedback_NoNewComments(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	// No new comments on any repo.
	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	p := d.pipeline(t)
	result, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.PRURL != "" {
		t.Errorf("expected empty PRURL when no comments, got %q", result.PRURL)
	}
}

func TestMultiRepoFeedback_NoPRsFound(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))

	if err == nil || !strings.Contains(err.Error(), "no PRs found") {
		t.Fatalf("expected no-PRs error, got %v", err)
	}
}

func TestMultiRepoFeedback_SkipsSyncForReposWithoutPRs(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	// Only svc-a has a PR; svc-b does not.
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		if repo == "svc-a" {
			return &models.PRDetails{
				Number: 10, Title: "Fix", Branch: head,
				URL: fmt.Sprintf("https://github.com/%s/%s/pull/10", owner, repo),
			}, nil
		}
		return nil, fmt.Errorf("not found")
	}

	// svc-b has no changes (AI only touched svc-a).
	d.git.HasChangesFunc = func(dir, _ string) (bool, error) {
		return filepath.Base(dir) == "svc-a", nil
	}

	// SyncWithRemote should only be called for svc-a. If called for
	// svc-b it would fail because the branch was never pushed there.
	d.git.SyncWithRemoteFunc = func(dir, _ string, _ []string) error {
		if filepath.Base(dir) == "svc-b" {
			t.Fatal("SyncWithRemote called for repo without a PR")
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMultiRepoFeedback_WritesMultiRepoFeedbackTask(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	var taskWritten bool
	d.taskWriter.WriteMultiRepoFeedbackTaskFunc = func(
		_ models.PRDetails, newComments, _ []models.PRComment,
		_ []models.CheckRunFailure,
		_ string, repos []taskfile.RepoContext,
	) error {
		taskWritten = true
		if len(repos) != 2 {
			t.Errorf("repo contexts = %d, want 2", len(repos))
		}
		if len(newComments) != 1 {
			t.Errorf("new comments = %d, want 1", len(newComments))
		}
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !taskWritten {
		t.Error("WriteMultiRepoFeedbackTask was not called")
	}
}

func TestMultiRepoFeedback_ForwardsRootRepoURL(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	const wantRootURL = "https://github.com/org/workspace-root.git"

	d.projects.ResolveProjectFunc = func(_ models.WorkItem) (*models.ProjectSettings, error) {
		return &models.ProjectSettings{
			Repos: []models.RepoSettings{
				{Name: "svc-a", Owner: "org", Repo: "svc-a", CloneURL: "https://github.com/org/svc-a.git", BaseBranch: "main"},
				{Name: "svc-b", Owner: "org", Repo: "svc-b", CloneURL: "https://github.com/org/svc-b.git", BaseBranch: "main"},
			},
			RootRepoURL:      wantRootURL,
			Container:        models.ContainerSettings{Image: "fat:latest"},
			InProgressStatus: "In Progress",
			InReviewStatus:   "In Review",
			TodoStatus:       "To Do",
		}, nil
	}

	var gotRootURL string
	d.workspaces.FindOrCreateMultiRepoFunc = func(ticketKey string, repos []workspace.RepoEntry, rootRepoURL string) (string, bool, error) {
		gotRootURL = rootRepoURL
		return d.wsDir, true, nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotRootURL != wantRootURL {
		t.Errorf("rootRepoURL = %q, want %q", gotRootURL, wantRootURL)
	}
}

// --- Emoji reactions ---

func TestExecuteFeedback_ReactsToNewComments(t *testing.T) {
	d := newFeedbackDeps(t)

	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc123", nil
	}

	var reacted []int64
	d.git.AddCommentReactionFunc = func(_, _ string, comment models.PRComment, reaction string) error {
		if reaction != "eyes" {
			t.Errorf("reaction = %q, want eyes", reaction)
		}
		reacted = append(reacted, comment.ID)
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reacted) != 1 || reacted[0] != 1 {
		t.Errorf("reacted comment IDs = %v, want [1]", reacted)
	}
}

func TestExecuteFeedback_ReactionFailureNonFatal(t *testing.T) {
	d := newFeedbackDeps(t)

	d.git.CommitChangesFunc = func(_, _, _, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc123", nil
	}

	d.git.AddCommentReactionFunc = func(_, _ string, _ models.PRComment, _ string) error {
		return errors.New("reaction API error")
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("reaction failure should not abort pipeline, got: %v", err)
	}
}

func TestExecuteFeedback_NoReactionsWhenNoNewComments(t *testing.T) {
	d := newFeedbackDeps(t)

	d.git.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	d.git.AddCommentReactionFunc = func(_, _ string, _ models.PRComment, _ string) error {
		t.Error("AddCommentReaction should not be called when there are no comments")
		return nil
	}

	p := d.pipeline(t)
	_, _ = p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
}

func TestMultiRepoFeedback_CIFixMarkerUsesActualSHA(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	// Return a per-repo SHA so we can verify the marker uses it.
	d.git.CommitChangesFunc = func(_, _, repo, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		return "abc123-" + repo, nil
	}

	// Set HeadSHA on PRs so CI analysis runs.
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 10, Title: "Fix", Branch: head, HeadSHA: "oldsha",
			BaseBranch: "main",
			URL:        fmt.Sprintf("https://github.com/%s/%s/pull/10", owner, repo),
		}, nil
	}

	// Only svc-a has a CI failure.
	d.git.ListCheckRunsForRefFunc = func(_, repo, ref string) ([]models.CheckRunFailure, bool, error) {
		if ref == "main" {
			return nil, true, nil
		}
		if repo == "svc-a" {
			return []models.CheckRunFailure{
				{ID: 42, Name: "ci/build", Conclusion: "failure"},
			}, true, nil
		}
		return nil, true, nil
	}

	type issueComment struct {
		repo string
		body string
	}
	var postedComments []issueComment
	d.git.PostIssueCommentFunc = func(_, repo string, _ int, body string) (int64, error) {
		postedComments = append(postedComments, issueComment{repo: repo, body: body})
		return 0, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:      "ai-bot",
		DefaultProvider:  "claude",
		AIAPIKeys:        map[string]string{"claude": "test-key"},
		MaxCIFixAttempts: 3,
	})
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the CI fix marker comment.
	var markers []issueComment
	for _, c := range postedComments {
		if strings.Contains(c.body, "ci-fix-attempt") {
			markers = append(markers, c)
		}
	}

	if len(markers) != 1 {
		t.Fatalf("expected 1 CI fix marker, got %d: %+v", len(markers), markers)
	}
	if markers[0].repo != "svc-a" {
		t.Errorf("marker repo = %q, want svc-a", markers[0].repo)
	}
	if strings.Contains(markers[0].body, "multi-repo") {
		t.Errorf("marker should use actual commit SHA, not 'multi-repo': %s", markers[0].body)
	}
	if !strings.Contains(markers[0].body, "abc123-svc-a") {
		t.Errorf("marker should contain commit SHA 'abc123-svc-a': %s", markers[0].body)
	}
}

func TestMultiRepoFeedback_CIFixMarkerPartialMapOnSyncError(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	// svc-a commits successfully; svc-b will fail during sync.
	var svcBCommitted bool
	d.git.CommitChangesFunc = func(_, _, repo, _, _, _, _ string, _ *models.Author, _ []string, _ bool) (string, error) {
		if repo == "svc-b" {
			svcBCommitted = true
		}
		return "sha-" + repo, nil
	}
	// Fail sync for svc-b only after its commit succeeds, so the test
	// expresses intent ("commit succeeded, then sync failed") rather
	// than depending on internal call ordering.
	d.git.SyncWithRemoteFunc = func(repoDir, _ string, _ []string) error {
		if strings.Contains(repoDir, "svc-b") && svcBCommitted {
			return fmt.Errorf("sync failed for svc-b")
		}
		return nil
	}

	// Both repos have CI failures so markers are attempted for both.
	d.git.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 10, Title: "Fix", Branch: head, HeadSHA: "oldsha",
			BaseBranch: "main",
			URL:        fmt.Sprintf("https://github.com/%s/%s/pull/10", owner, repo),
		}, nil
	}
	d.git.GetPRCommentsFunc = func(_, repo string, _ int, _ time.Time) ([]models.PRComment, error) {
		if repo == "svc-a" {
			return []models.PRComment{
				{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Fix A", IsReviewComment: true},
			}, nil
		}
		return []models.PRComment{
			{ID: 200, Author: models.Author{Username: "reviewer"}, Body: "Fix B", IsReviewComment: true},
		}, nil
	}
	d.git.ListCheckRunsForRefFunc = func(_, repo, ref string) ([]models.CheckRunFailure, bool, error) {
		if ref == "main" {
			return nil, true, nil
		}
		return []models.CheckRunFailure{
			{ID: 1, Name: "ci/build", Conclusion: "failure"},
		}, true, nil
	}

	type issueComment struct {
		repo string
		body string
	}
	var postedComments []issueComment
	d.git.PostIssueCommentFunc = func(_, repo string, _ int, body string) (int64, error) {
		postedComments = append(postedComments, issueComment{repo: repo, body: body})
		return 0, nil
	}

	p := d.pipelineWithConfig(t, executor.Config{
		BotUsername:      "ai-bot",
		DefaultProvider:  "claude",
		AIAPIKeys:        map[string]string{"claude": "test-key"},
		MaxCIFixAttempts: 3,
	})
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err == nil {
		t.Fatal("expected error from sync failure, got nil")
	}
	if !strings.Contains(err.Error(), "sync failed for svc-b") {
		t.Fatalf("expected sync error, got: %v", err)
	}

	// Despite the error, CI fix markers should still have been posted.
	// svc-a should use its real SHA; svc-b should use its real SHA too
	// because CommitChanges uses the Git Data API (commit exists on the
	// remote before SyncWithRemote runs).
	var markers []issueComment
	for _, c := range postedComments {
		if strings.Contains(c.body, "ci-fix-attempt") {
			markers = append(markers, c)
		}
	}

	if len(markers) != 2 {
		t.Fatalf("expected 2 CI fix markers, got %d: %+v", len(markers), markers)
	}

	for _, m := range markers {
		if strings.Contains(m.body, "multi-repo") {
			t.Errorf("marker should not contain 'multi-repo': %s", m.body)
		}
		expectedSHA := "sha-" + m.repo
		if !strings.Contains(m.body, expectedSHA) {
			t.Errorf("marker for %s should contain %q: %s", m.repo, expectedSHA, m.body)
		}
	}
}

func TestExecuteMultiRepoFeedback_ReactsPerRepo(t *testing.T) {
	d := newMultiRepoFeedbackDeps(t)

	type reactionCall struct {
		repo      string
		commentID int64
	}
	var reactions []reactionCall
	d.git.AddCommentReactionFunc = func(_, repo string, comment models.PRComment, reaction string) error {
		if reaction != "eyes" {
			t.Errorf("reaction = %q, want eyes", reaction)
		}
		reactions = append(reactions, reactionCall{repo: repo, commentID: comment.ID})
		return nil
	}

	p := d.pipeline(t)
	_, err := p.Execute(context.Background(), newFeedbackJob("PROJ-1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(reactions) != 1 {
		t.Fatalf("expected 1 reaction, got %d", len(reactions))
	}
	if reactions[0].repo != "svc-a" {
		t.Errorf("reaction repo = %q, want svc-a", reactions[0].repo)
	}
	if reactions[0].commentID != 100 {
		t.Errorf("reaction comment ID = %d, want 100", reactions[0].commentID)
	}
}
