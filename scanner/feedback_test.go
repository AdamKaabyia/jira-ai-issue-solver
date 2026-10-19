package scanner_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/scanner"
	"jira-ai-issue-solver/scanner/scannertest"
)

// --- Construction validation ---

func TestNewFeedbackScanner_Validation(t *testing.T) {
	validCfg := scanner.FeedbackScannerConfig{
		PollInterval: time.Minute,
		BotUsername:  "ai-bot",
	}

	tests := []struct {
		name      string
		searcher  scanner.IssueSearcher
		submitter scanner.JobSubmitter
		prs       scanner.PRFetcher
		repos     scanner.RepoLocator
		cfg       scanner.FeedbackScannerConfig
		logger    *zap.Logger
		wantErr   string
	}{
		{
			name: "nil searcher", searcher: nil,
			submitter: &scannertest.StubJobSubmitter{},
			prs:       &scannertest.StubPRFetcher{}, repos: &scannertest.StubRepoLocator{},
			cfg: validCfg, logger: zap.NewNop(), wantErr: "issue searcher",
		},
		{
			name: "nil submitter", searcher: &scannertest.StubIssueSearcher{},
			submitter: nil,
			prs:       &scannertest.StubPRFetcher{}, repos: &scannertest.StubRepoLocator{},
			cfg: validCfg, logger: zap.NewNop(), wantErr: "job submitter",
		},
		{
			name: "nil prs", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{},
			prs:       nil, repos: &scannertest.StubRepoLocator{},
			cfg: validCfg, logger: zap.NewNop(), wantErr: "PR fetcher",
		},
		{
			name: "nil repos", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{},
			prs:       &scannertest.StubPRFetcher{}, repos: nil,
			cfg: validCfg, logger: zap.NewNop(), wantErr: "repo locator",
		},
		{
			name: "zero poll", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{},
			prs:       &scannertest.StubPRFetcher{}, repos: &scannertest.StubRepoLocator{},
			cfg: scanner.FeedbackScannerConfig{BotUsername: "bot"}, logger: zap.NewNop(),
			wantErr: "poll interval",
		},
		{
			name: "empty bot", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{},
			prs:       &scannertest.StubPRFetcher{}, repos: &scannertest.StubRepoLocator{},
			cfg: scanner.FeedbackScannerConfig{PollInterval: time.Minute}, logger: zap.NewNop(),
			wantErr: "bot username",
		},
		{
			name: "nil logger", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{},
			prs:       &scannertest.StubPRFetcher{}, repos: &scannertest.StubRepoLocator{},
			cfg: validCfg, logger: nil, wantErr: "logger",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := scanner.NewFeedbackScanner(
				tt.searcher, tt.submitter, tt.prs, tt.repos, nil, tt.cfg, tt.logger)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- Emits event for actionable comments ---

func TestFeedbackScanner_EmitsEventForActionableComments(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	var mu sync.Mutex
	var submitted []jobmanager.Event
	d.submitter.SubmitFunc = func(event jobmanager.Event) (*jobmanager.Job, error) {
		mu.Lock()
		submitted = append(submitted, event)
		mu.Unlock()
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	mu.Lock()
	defer mu.Unlock()
	if len(submitted) != 1 {
		t.Fatalf("submitted %d events, want 1", len(submitted))
	}
	if submitted[0].TicketKey != "PROJ-1" {
		t.Errorf("ticket = %q, want PROJ-1", submitted[0].TicketKey)
	}
	if submitted[0].Type != jobmanager.JobTypeFeedback {
		t.Errorf("type = %q, want feedback", submitted[0].Type)
	}
}

// --- No event when all comments addressed ---

func TestFeedbackScanner_NoEventWhenAllAddressed(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
			{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
		}, nil
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when all comments are addressed")
	}
}

// --- No event when only ignored users ---

func TestFeedbackScanner_IgnoredUsersFiltered(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.IgnoredUsernames = []string{"packit"}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "packit[bot]"}, Body: "/build"},
		}, nil
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called for ignored users only")
	}
}

// --- Known bot loop filtered ---

func TestFeedbackScanner_KnownBotLoopFiltered(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.KnownBotUsernames = []string{"coderabbitai"}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
			{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
			// Only "new" comment is from a known bot replying to our bot.
			{ID: 3, Author: models.Author{Username: "coderabbitai[bot]"}, Body: "Also", InReplyTo: 2},
		}, nil
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when only actionable comment is a bot loop")
	}
}

// --- Thread depth filtered ---

func TestFeedbackScanner_ThreadDepthFiltered(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxThreadDepth = 1
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
			{ID: 2, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 1},
			// Depth at this comment is 1 (bot at ID 2), which equals max.
			{ID: 3, Author: models.Author{Username: "reviewer"}, Body: "Again", InReplyTo: 2},
		}, nil
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when comments exceed thread depth")
	}
}

// --- PR not found is skipped ---

func TestFeedbackScanner_PRNotFound_Skipped(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when PR not found")
	}
}

// --- Repo locate failure skipped ---

func TestFeedbackScanner_RepoLocateFailure_Skipped(t *testing.T) {
	d := newFeedbackDeps()
	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return nil, errors.New("unknown component")
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when repo locate fails")
	}
}

// --- Comment fetch failure skipped ---

func TestFeedbackScanner_CommentFetchFailure_Skipped(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return nil, errors.New("API error")
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when comment fetch fails")
	}
}

// --- Multiple tickets with mixed actionable ---

func TestFeedbackScanner_MultipleTickets_MixedActionable(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{
			{Key: "PROJ-1"},
			{Key: "PROJ-2"},
		}, nil
	}

	d.prs.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, Branch: head, URL: "https://github.com/org/repo/pull/1"}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		}, nil
	}

	// Second ticket has no actionable comments.
	callCount := 0
	origComments := d.prs.GetPRCommentsFunc
	d.prs.GetPRCommentsFunc = func(owner, repo string, number int, _ time.Time) ([]models.PRComment, error) {
		callCount++
		if callCount == 2 {
			return []models.PRComment{
				{ID: 10, Author: models.Author{Username: "reviewer"}, Body: "Old"},
				{ID: 11, Author: models.Author{Username: "ai-bot"}, Body: "Done", InReplyTo: 10},
			}, nil
		}
		return origComments(owner, repo, number, time.Time{})
	}

	var mu sync.Mutex
	var submitted []string
	d.submitter.SubmitFunc = func(event jobmanager.Event) (*jobmanager.Job, error) {
		mu.Lock()
		submitted = append(submitted, event.TicketKey)
		mu.Unlock()
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	mu.Lock()
	defer mu.Unlock()
	// Only PROJ-1 should get an event (PROJ-2 is all addressed).
	if len(submitted) != 1 {
		t.Fatalf("submitted %d events, want 1", len(submitted))
	}
	if submitted[0] != "PROJ-1" {
		t.Errorf("submitted[0] = %q, want PROJ-1", submitted[0])
	}
}

// --- Circuit open stops scan cycle ---

func TestFeedbackScanner_CircuitOpenStopsCycle(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{Key: "PROJ-1"}, {Key: "PROJ-2"}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		}, nil
	}

	var mu sync.Mutex
	var callCount int
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		return nil, jobmanager.ErrCircuitOpen
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("Submit called %d times, want 1 (circuit open stops cycle)", callCount)
	}
}

// --- Budget exceeded stops scan cycle ---

func TestFeedbackScanner_BudgetExceededStopsCycle(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{Key: "PROJ-1"}, {Key: "PROJ-2"}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		}, nil
	}

	var mu sync.Mutex
	var callCount int
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		mu.Lock()
		callCount++
		mu.Unlock()
		return nil, jobmanager.ErrBudgetExceeded
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Errorf("Submit called %d times, want 1 (budget exceeded stops cycle)", callCount)
	}
}

// --- Start/Stop lifecycle ---

func TestFeedbackScanner_StartWhileRunning(t *testing.T) {
	d := newFeedbackDeps()
	s := d.scanner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}

	if err := s.Start(ctx); err == nil {
		t.Fatal("expected error on double start")
	}

	s.Stop()
}

func TestFeedbackScanner_StopWithoutStart(t *testing.T) {
	d := newFeedbackDeps()
	s := d.scanner(t)
	// Should not panic.
	s.Stop()
}

func TestFeedbackScanner_RestartAfterStop(t *testing.T) {
	d := newFeedbackDeps()

	var mu sync.Mutex
	var scanCount int
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		mu.Lock()
		scanCount++
		mu.Unlock()
		return []models.WorkItem{}, nil
	}

	s := d.scanner(t)

	// First Start/Stop cycle.
	runOneFeedbackScan(t, s)

	mu.Lock()
	firstCount := scanCount
	mu.Unlock()
	if firstCount == 0 {
		t.Fatal("expected at least one scan in first cycle")
	}

	// Second Start/Stop cycle -- restart should work.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("restart failed: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	s.Stop()

	mu.Lock()
	secondCount := scanCount
	mu.Unlock()
	if secondCount <= firstCount {
		t.Errorf("expected additional scans after restart, got %d total (was %d)", secondCount, firstCount)
	}
}

// --- Branch name convention ---

func TestFeedbackScanner_UsesBranchConvention(t *testing.T) {
	d := newFeedbackDeps()

	var receivedHead string
	d.prs.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		receivedHead = head
		return &models.PRDetails{Number: 1}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if receivedHead != "ai-bot/PROJ-1" {
		t.Errorf("branch = %q, want ai-bot/PROJ-1", receivedHead)
	}
}

// --- Fork mode ---

func TestFeedbackScanner_ForkMode_UsesOwnerPrefixedHead(t *testing.T) {
	d := newFeedbackDeps()
	d.repos.ForkOwnerHeadsFunc = func(workItem models.WorkItem, branchName string) []string {
		if workItem.Key == "PROJ-1" {
			return []string{"contributor-gh:" + branchName, branchName}
		}
		return []string{branchName}
	}

	var receivedHead string
	d.prs.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		receivedHead = head
		return &models.PRDetails{Number: 42, Branch: head}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix"},
		}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if receivedHead != "contributor-gh:ai-bot/PROJ-1" {
		t.Errorf("head = %q, want contributor-gh:ai-bot/PROJ-1", receivedHead)
	}
}

func TestFeedbackScanner_ForkMode_MultiRepo_MixedHeads(t *testing.T) {
	d := newFeedbackDeps()

	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "svc-a"},
			{Owner: "org", Repo: "svc-b"},
		}, nil
	}
	d.repos.ForkOwnerHeadsFunc = func(_ models.WorkItem, branchName string) []string {
		return []string{"contributor-gh:" + branchName, branchName}
	}

	// svc-a has a fork-mode PR (matches fork head), svc-b has a
	// direct-mode PR (matches bare branch). Both should be found.
	var queriedRepoHeads []string
	d.prs.GetPRForBranchFunc = func(_, repo, head string) (*models.PRDetails, error) {
		queriedRepoHeads = append(queriedRepoHeads, repo+"@"+head)
		switch {
		case repo == "svc-a" && head == "contributor-gh:ai-bot/PROJ-1":
			return &models.PRDetails{Number: 10, Branch: head}, nil
		case repo == "svc-b" && head == "ai-bot/PROJ-1":
			return &models.PRDetails{Number: 20, Branch: head}, nil
		default:
			return nil, nil
		}
	}

	var commentRepos []string
	d.prs.GetPRCommentsFunc = func(_, repo string, _ int, _ time.Time) ([]models.PRComment, error) {
		commentRepos = append(commentRepos, repo)
		if repo == "svc-b" {
			return []models.PRComment{
				{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
			}, nil
		}
		return []models.PRComment{}, nil
	}

	var submitted bool
	d.submitter.SubmitFunc = func(e jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if !submitted {
		t.Error("expected feedback event to be submitted")
	}

	// Both repos should have had their comments fetched.
	if len(commentRepos) != 2 {
		t.Errorf("comments fetched for %v, want both svc-a and svc-b", commentRepos)
	}
}

// --- No event when no comments at all ---

func TestFeedbackScanner_NoCommentsNoEvent(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	submitCalled := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitCalled = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCalled {
		t.Error("Submit should not be called when no comments exist")
	}
}

// --- Multi-repo scanning ---

func TestFeedbackScanner_MultiRepo_CommentsOnSecondRepo(t *testing.T) {
	d := newFeedbackDeps()

	// Workspace has 3 repos; only svc-b has a PR with comments.
	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "svc-a"},
			{Owner: "org", Repo: "svc-b"},
			{Owner: "org", Repo: "svc-c"},
		}, nil
	}

	d.prs.GetPRForBranchFunc = func(owner, repo, head string) (*models.PRDetails, error) {
		if repo == "svc-b" {
			return &models.PRDetails{Number: 99, Branch: head}, nil
		}
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, repo string, _ int, _ time.Time) ([]models.PRComment, error) {
		if repo == "svc-b" {
			return []models.PRComment{
				{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
			}, nil
		}
		return []models.PRComment{}, nil
	}

	var submitCount int
	d.submitter.SubmitFunc = func(event jobmanager.Event) (*jobmanager.Job, error) {
		submitCount++
		if event.Type != jobmanager.JobTypeFeedback {
			t.Errorf("event type = %q, want feedback", event.Type)
		}
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitCount != 1 {
		t.Errorf("submitted %d events, want 1", submitCount)
	}
}

func TestFeedbackScanner_MultiRepo_NoCommentsOnAnyRepo(t *testing.T) {
	d := newFeedbackDeps()

	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "svc-a"},
			{Owner: "org", Repo: "svc-b"},
		}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneFeedbackScan(t, s)

	if submitted {
		t.Error("Submit should not be called when no repo has actionable comments")
	}
}

// --- CI failure detection ---

func TestFeedbackScanner_CIFailuresActionable(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 3

	// No review comments.
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}

	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{ID: 1, Name: "lint", Conclusion: "failure"}}, true, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected feedback event for CI failure")
	}
}

func TestFeedbackScanner_CIDisabled_MaxZero(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 0

	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	// No review comments.
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	// CI has failures, but MaxCIFixAttempts=0 means they are not
	// actionable. CI is still queried for label state.
	ciCalled := false
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			ciCalled = true
			return []models.CheckRunFailure{{Name: "lint"}}, true, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if submitted {
		t.Error("should not submit when CI fixing is disabled and no comments")
	}
	if !ciCalled {
		t.Error("CI should still be queried for label state even when fixing is disabled")
	}
}

func TestFeedbackScanner_CIPending_Skipped(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 3

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}

	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{ID: 1, Name: "lint", Conclusion: "failure"}}, false, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if submitted {
		t.Error("should not submit when CI checks are still running")
	}
}

func TestFeedbackScanner_CIIgnoredChecks(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 3
	d.cfg.IgnoredCheckNames = []string{"license/cla"}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}

	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{ID: 1, Name: "license/cla", Conclusion: "failure"}}, true, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if submitted {
		t.Error("should not submit when all CI failures are ignored")
	}
}

func TestFeedbackScanner_CIPreExistingFiltered(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 3

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123", BaseBranch: "main"}, nil
	}

	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, ref string) ([]models.CheckRunFailure, bool, error) {
			// Both PR and base branch have the same failing check.
			return []models.CheckRunFailure{{ID: 1, Name: "lint", Conclusion: "failure"}}, true, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if submitted {
		t.Error("should not submit when all CI failures are pre-existing on base branch")
	}
}

func TestFeedbackScanner_CIFixAttemptsExhausted(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 2

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{Author: models.Author{Username: "ai-bot"}, Body: "CI failures addressed in abc.\n<!-- ci-fix-attempt: 1 -->"},
			{Author: models.Author{Username: "ai-bot"}, Body: "CI failures addressed in def.\n<!-- ci-fix-attempt: 2 -->"},
		}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}

	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{ID: 3, Name: "test", Conclusion: "failure"}}, true, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if submitted {
		t.Error("should not submit when CI fix attempts exhausted")
	}
}

func TestFeedbackScanner_CIAndComments_BothActionable(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.MaxCIFixAttempts = 3

	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}

	// CI also has failures — checked for label state even when
	// review comments are actionable.
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{ID: 1, Name: "lint", Conclusion: "failure"}}, true, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected feedback event")
	}
}

// --- helpers ---

type feedbackDeps struct {
	searcher               *scannertest.StubIssueSearcher
	submitter              *scannertest.StubJobSubmitter
	prs                    *scannertest.StubPRFetcher
	repos                  *scannertest.StubRepoLocator
	ci                     *scannertest.StubCIChecker
	cfg                    scanner.FeedbackScannerConfig
	labels                 *scannertest.StubLabelManager
	prLabeler              *scannertest.StubPRLabeler
	labelResolver          *scannertest.StubFailureLabelResolver
	lifecycleLabelResolver *scannertest.StubLifecycleLabelResolver
	mergedStatusResolver   *scannertest.StubMergedStatusResolver
	statusTransitioner     *scannertest.StubStatusTransitioner
}

func newFeedbackDeps() *feedbackDeps {
	return &feedbackDeps{
		searcher: &scannertest.StubIssueSearcher{
			SearchWorkItemsFunc: func(_ models.SearchCriteria) ([]models.WorkItem, error) {
				return []models.WorkItem{{Key: "PROJ-1"}}, nil
			},
		},
		submitter: &scannertest.StubJobSubmitter{},
		prs: &scannertest.StubPRFetcher{
			GetPRForBranchFunc: func(owner, repo, head string) (*models.PRDetails, error) {
				return &models.PRDetails{
					Number: 42, Branch: head,
					URL: "https://github.com/org/repo/pull/42",
				}, nil
			},
			GetPRCommentsFunc: func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
				return []models.PRComment{
					{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
				}, nil
			},
		},
		repos: &scannertest.StubRepoLocator{
			LocateReposFunc: func(_ models.WorkItem) ([]models.RepoCoord, error) {
				return []models.RepoCoord{{Owner: "org", Repo: "repo"}}, nil
			},
		},
		cfg: scanner.FeedbackScannerConfig{
			PollInterval: time.Hour,
			BotUsername:  "ai-bot",
		},
	}
}

func (d *feedbackDeps) scanner(t *testing.T) *scanner.FeedbackScanner {
	t.Helper()
	var opts []scanner.FeedbackScannerOption
	if d.labels != nil && d.labelResolver != nil {
		opts = append(opts, scanner.WithLabelManager(d.labels, d.labelResolver))
	}
	if d.prLabeler != nil {
		opts = append(opts, scanner.WithPRLabeler(d.prLabeler))
	}
	if d.lifecycleLabelResolver != nil {
		var mr scanner.MergedStatusResolver
		if d.mergedStatusResolver != nil {
			mr = d.mergedStatusResolver
		}
		var st scanner.StatusTransitioner
		if d.statusTransitioner != nil {
			st = d.statusTransitioner
		}
		opts = append(opts, scanner.WithLifecycleLabelManager(d.lifecycleLabelResolver, mr, st))
	}
	s, err := scanner.NewFeedbackScanner(
		d.searcher, d.submitter, d.prs, d.repos, d.ci, d.cfg, zap.NewNop(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runOneFeedbackScan starts the scanner, waits for the immediate
// first scan to complete, then stops. Uses the long poll interval
// (1 hour) set by newFeedbackDeps to ensure only one scan cycle runs.
//
// The 50ms sleep is sufficient because all stubs complete
// synchronously (no real I/O). Tests that need tighter
// synchronization use channel-based patterns directly.
func runOneFeedbackScan(t *testing.T, s *scanner.FeedbackScanner) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	s.Stop()
}

func TestFeedbackScanner_FailureLabels_CIFailing(t *testing.T) {
	d := newFeedbackDeps()
	// No actionable review comments.
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	// CI is failing but not actionable (attempts exhausted).
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{Name: "build"}}, true, nil
		},
	}
	d.cfg.MaxCIFixAttempts = 0 // CI fixing disabled, but CI is still red

	var added []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, label string) error { added = append(added, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail", Blocked: "blocked"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "ci-fail" {
		t.Errorf("added = %v, want [ci-fail]", added)
	}
}

func TestFeedbackScanner_FailureLabels_CIFailing_ClearsLifecycleLabels(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:    "PROJ-1",
			Labels: []string{"blocked", "jira-autofix", "jira-autofix-review"},
		}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{Name: "build"}}, true, nil
		},
	}
	d.cfg.MaxCIFixAttempts = 0

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail", Blocked: "blocked"}
		},
	}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Queued: "jira-autofix", Review: "jira-autofix-review"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "ci-fail" {
		t.Errorf("added = %v, want [ci-fail]", added)
	}
	removedSet := make(map[string]bool, len(removed))
	for _, l := range removed {
		removedSet[l] = true
	}
	if !removedSet["blocked"] {
		t.Error("expected sibling failure label 'blocked' to be removed")
	}
	if !removedSet["jira-autofix"] {
		t.Error("expected lifecycle label 'jira-autofix' to be removed (cross-group)")
	}
	if !removedSet["jira-autofix-review"] {
		t.Error("expected lifecycle label 'jira-autofix-review' to be removed (cross-group)")
	}
}

func TestFeedbackScanner_FailureLabels_CIPassing(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{Key: "PROJ-1", Labels: []string{"ci-fail"}}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	// CI is passing.
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{}, true, nil
		},
	}

	var removed []string
	d.labels = &scannertest.StubLabelManager{
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(removed) != 1 || removed[0] != "ci-fail" {
		t.Errorf("removed = %v, want [ci-fail]", removed)
	}
}

func TestFeedbackScanner_FailureLabels_CIFailing_EmptyLabel(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{Name: "build"}}, true, nil
		},
	}
	d.cfg.MaxCIFixAttempts = 0

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{Blocked: "blocked"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 0 {
		t.Errorf("added = %v, want empty (CIFailing not configured)", added)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty (CIFailing not configured)", removed)
	}
}

func TestFeedbackScanner_FailureLabels_CIRecovery_RestoresReview(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:    "PROJ-1",
			Labels: []string{"ci-fail", "blocked", "jira-autofix"},
		}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{}, true, nil
		},
	}

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail", Blocked: "blocked"}
		},
	}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Queued: "jira-autofix", Review: "jira-autofix-review"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "jira-autofix-review" {
		t.Errorf("added = %v, want [jira-autofix-review]", added)
	}
	removedSet := make(map[string]bool, len(removed))
	for _, l := range removed {
		removedSet[l] = true
	}
	if !removedSet["ci-fail"] {
		t.Error("expected ci-fail to be removed during recovery")
	}
	if !removedSet["blocked"] {
		t.Error("expected sibling failure label 'blocked' to be removed")
	}
	if !removedSet["jira-autofix"] {
		t.Error("expected lifecycle label 'jira-autofix' to be removed (cross-group)")
	}
}

func TestFeedbackScanner_FailureLabels_Rejected(t *testing.T) {
	d := newFeedbackDeps()
	// No open PR — simulate PR not found.
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	// Closed (rejected) PR exists.
	d.prs.GetClosedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var added []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, label string) error { added = append(added, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{Rejected: "rejected", CIFailing: "ci-fail"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "rejected" {
		t.Errorf("added = %v, want [rejected]", added)
	}
}

func TestFeedbackScanner_FailureLabels_Rejected_EmptyLabel(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetClosedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 0 {
		t.Errorf("added = %v, want empty (Rejected not configured)", added)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty (Rejected not configured)", removed)
	}
}

func TestFeedbackScanner_FailureLabels_NoOpWhenNotConfigured(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// No labels or labelResolver set — should be a no-op.
	runOneFeedbackScan(t, d.scanner(t))
	// If we got here without panic, the test passes.
}

func TestFeedbackScanner_FailureLabels_EmptyLabelsSkipped(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{}, true, nil
		},
	}

	labelsCalled := false
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, _ string) error { labelsCalled = true; return nil },
		RemoveLabelFunc: func(_, _ string) error { labelsCalled = true; return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{} // all empty
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if labelsCalled {
		t.Error("expected no label operations when all labels are empty")
	}
}

func TestFeedbackScanner_FailureLabels_Rejected_MultiRepo_ErrorOnOneRepo(t *testing.T) {
	d := newFeedbackDeps()
	// No open PR on any repo.
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	// Multi-repo: two repos, first errors on closed PR check, second has rejected PR.
	d.repos = &scannertest.StubRepoLocator{
		LocateReposFunc: func(_ models.WorkItem) ([]models.RepoCoord, error) {
			return []models.RepoCoord{
				{Owner: "org", Repo: "repo-a"},
				{Owner: "org", Repo: "repo-b"},
			}, nil
		},
	}
	callCount := 0
	d.prs.GetClosedPRForBranchFunc = func(_, repo, _ string) (*models.PRDetails, error) {
		callCount++
		if repo == "repo-a" {
			return nil, fmt.Errorf("API error")
		}
		return &models.PRDetails{Number: 1, URL: "https://github.com/org/repo-b/pull/1"}, nil
	}

	var added []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, label string) error { added = append(added, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{Rejected: "rejected"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if callCount != 2 {
		t.Errorf("expected 2 GetClosedPRForBranch calls, got %d", callCount)
	}
	if len(added) != 1 || added[0] != "rejected" {
		t.Errorf("added = %v, want [rejected]", added)
	}
}

func TestFeedbackScanner_FailureLabels_CIErrorPreservesLabel(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{Key: "PROJ-1", Labels: []string{"ci-fail"}}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	// CI query fails — label should NOT be removed.
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return nil, false, fmt.Errorf("GitHub API error")
		},
	}

	labelRemoved := false
	d.labels = &scannertest.StubLabelManager{
		RemoveLabelFunc: func(_, _ string) error { labelRemoved = true; return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if labelRemoved {
		t.Error("ci-failing label should be preserved when CI query errors")
	}
}

func TestFeedbackScanner_FailureLabels_Idempotent_NoAPICallsWhenCorrect(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:    "PROJ-1",
			Labels: []string{"ci-fail"},
		}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{{Name: "build"}}, true, nil
		},
	}
	d.cfg.MaxCIFixAttempts = 0

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail", Blocked: "blocked"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 0 {
		t.Errorf("added = %v, want empty (ci-fail already present)", added)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty (no stale labels to remove)", removed)
	}
}

func TestFeedbackScanner_FailureLabels_CIPassing_NoOpWhenLabelAbsent(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{Key: "PROJ-1", Labels: []string{}}}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, _, _ string) ([]models.CheckRunFailure, bool, error) {
			return []models.CheckRunFailure{}, true, nil
		},
	}

	var removed []string
	d.labels = &scannertest.StubLabelManager{
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty (ci-fail not present, no remove needed)", removed)
	}
}

func TestFeedbackScanner_FailureLabels_MultiRepo_ActionableAndCIFailing(t *testing.T) {
	d := newFeedbackDeps()
	// Two repos: repo-a has actionable comments, repo-b has failing CI.
	d.repos = &scannertest.StubRepoLocator{
		LocateReposFunc: func(_ models.WorkItem) ([]models.RepoCoord, error) {
			return []models.RepoCoord{
				{Owner: "org", Repo: "repo-a"},
				{Owner: "org", Repo: "repo-b"},
			}, nil
		},
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, HeadSHA: "abc123"}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, repo string, _ int, _ time.Time) ([]models.PRComment, error) {
		if repo == "repo-a" {
			return []models.PRComment{
				{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
			}, nil
		}
		return []models.PRComment{}, nil
	}
	d.ci = &scannertest.StubCIChecker{
		ListCheckRunsForRefFunc: func(_, repo, _ string) ([]models.CheckRunFailure, bool, error) {
			if repo == "repo-b" {
				return []models.CheckRunFailure{{Name: "lint"}}, true, nil
			}
			return []models.CheckRunFailure{}, true, nil
		},
	}

	var added []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, label string) error { added = append(added, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail"}
		},
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected feedback submission for actionable comments on repo-a")
	}
	if len(added) != 1 || added[0] != "ci-fail" {
		t.Errorf("added = %v, want [ci-fail] (CI on repo-b should be observed even though repo-a has actionable comments)", added)
	}
}

// --- Lifecycle label tests ---

func TestFeedbackScanner_LifecycleLabels_Merged(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:    "PROJ-1",
			Labels: []string{"jira-autofix", "jira-autofix-review"},
		}}, nil
	}
	// No open PR.
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// Merged PR exists.
	d.prs.GetMergedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{
				Queued: "jira-autofix",
				Review: "jira-autofix-review",
				Merged: "jira-autofix-merged",
			}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "jira-autofix-merged" {
		t.Errorf("added = %v, want [jira-autofix-merged]", added)
	}
	removedSet := make(map[string]bool, len(removed))
	for _, l := range removed {
		removedSet[l] = true
	}
	if !removedSet["jira-autofix"] {
		t.Error("expected queued label to be removed")
	}
	if !removedSet["jira-autofix-review"] {
		t.Error("expected review label to be removed")
	}
}

func TestFeedbackScanner_LifecycleLabels_Merged_ClearsFailureLabels(t *testing.T) {
	d := newFeedbackDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:    "PROJ-1",
			Labels: []string{"ci-fail", "blocked", "jira-autofix", "jira-autofix-review"},
		}}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetMergedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1, URL: "https://github.com/org/repo/pull/1"}, nil
	}

	var added, removed []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, label string) error { removed = append(removed, label); return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{
		ResolveFailureLabelsFunc: func(_ models.WorkItem) models.FailureLabels {
			return models.FailureLabels{CIFailing: "ci-fail", Blocked: "blocked"}
		},
	}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{
				Queued: "jira-autofix",
				Review: "jira-autofix-review",
				Merged: "jira-autofix-merged",
			}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "jira-autofix-merged" {
		t.Errorf("added = %v, want [jira-autofix-merged]", added)
	}
	removedSet := make(map[string]bool, len(removed))
	for _, l := range removed {
		removedSet[l] = true
	}
	if !removedSet["ci-fail"] {
		t.Error("expected failure label 'ci-fail' to be removed (cross-group)")
	}
	if !removedSet["blocked"] {
		t.Error("expected failure label 'blocked' to be removed (cross-group)")
	}
	if !removedSet["jira-autofix"] {
		t.Error("expected lifecycle label 'jira-autofix' to be removed")
	}
	if !removedSet["jira-autofix-review"] {
		t.Error("expected lifecycle label 'jira-autofix-review' to be removed")
	}
}

func TestFeedbackScanner_LifecycleLabels_MergedWithStatusTransition(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetMergedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1}, nil
	}

	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, _ string) error { return nil },
		RemoveLabelFunc: func(_, _ string) error { return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}
	d.mergedStatusResolver = &scannertest.StubMergedStatusResolver{
		ResolveMergedStatusFunc: func(_ models.WorkItem) string { return "MODIFIED" },
	}

	var transitioned string
	d.statusTransitioner = &scannertest.StubStatusTransitioner{
		TransitionStatusFunc: func(_, status string) error {
			transitioned = status
			return nil
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if transitioned != "MODIFIED" {
		t.Errorf("transitioned = %q, want %q", transitioned, "MODIFIED")
	}
}

func TestFeedbackScanner_LifecycleLabels_NoTransitionWhenStatusEmpty(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	d.prs.GetMergedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return &models.PRDetails{Number: 1}, nil
	}

	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, _ string) error { return nil },
		RemoveLabelFunc: func(_, _ string) error { return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}
	d.mergedStatusResolver = &scannertest.StubMergedStatusResolver{
		ResolveMergedStatusFunc: func(_ models.WorkItem) string { return "" },
	}

	transitionCalled := false
	d.statusTransitioner = &scannertest.StubStatusTransitioner{
		TransitionStatusFunc: func(_, _ string) error {
			transitionCalled = true
			return nil
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if transitionCalled {
		t.Error("expected no status transition when merged status is empty")
	}
}

func TestFeedbackScanner_LifecycleLabels_NoOpWhenNotConfigured(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// No lifecycle label resolver set — should be a no-op.
	runOneFeedbackScan(t, d.scanner(t))
}

func TestFeedbackScanner_LifecycleLabels_NotMergedWhenOpenPRExists(t *testing.T) {
	d := newFeedbackDeps()
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// Open PR exists — not merged.
	d.prs.GetMergedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	addCalled := false
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, _ string) error { addCalled = true; return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if addCalled {
		t.Error("merged label should not be applied when PR is not merged")
	}
}

func TestFeedbackScanner_LifecycleLabels_MultiRepo_AllMergedRequired(t *testing.T) {
	d := newFeedbackDeps()
	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "repo-a"},
			{Owner: "org", Repo: "repo-b"},
		}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// repo-a is merged; repo-b has a closed (unmerged) PR — still outstanding.
	d.prs.GetMergedPRForBranchFunc = func(_, repo, _ string) (*models.PRDetails, error) {
		if repo == "repo-a" {
			return &models.PRDetails{Number: 1}, nil
		}
		return nil, nil
	}
	d.prs.GetClosedPRForBranchFunc = func(_, repo, _ string) (*models.PRDetails, error) {
		if repo == "repo-b" {
			return &models.PRDetails{Number: 2}, nil
		}
		return nil, nil
	}

	addCalled := false
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, _ string) error { addCalled = true; return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if addCalled {
		t.Error("merged label should not be applied when a repo has an unmerged PR")
	}
}

func TestFeedbackScanner_LifecycleLabels_MergedNotMaskedByStaleClosedPR(t *testing.T) {
	d := newFeedbackDeps()

	// Fork-mode: fork head is checked first, bare branch second.
	d.repos.ForkOwnerHeadsFunc = func(_ models.WorkItem, branchName string) []string {
		return []string{"contributor-gh:" + branchName, branchName}
	}

	// No open PR on any head.
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	// Fork head has a stale closed PR; bare branch has the merged PR.
	d.prs.GetMergedPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		if head == "ai-bot/PROJ-1" {
			return &models.PRDetails{Number: 1}, nil
		}
		return nil, nil
	}
	d.prs.GetClosedPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		if head == "contributor-gh:ai-bot/PROJ-1" {
			return &models.PRDetails{Number: 2}, nil
		}
		return nil, nil
	}

	var mergedLabelApplied bool
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { mergedLabelApplied = (label == "merged"); return nil },
		RemoveLabelFunc: func(_, _ string) error { return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !mergedLabelApplied {
		t.Error("merged label should be applied — merged PR under bare branch must not be masked by stale closed PR under fork head")
	}
}

func TestFeedbackScanner_LifecycleLabels_MultiRepo_SkipsReposWithoutPRs(t *testing.T) {
	d := newFeedbackDeps()
	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "repo-a"},
			{Owner: "org", Repo: "repo-b"},
			{Owner: "org", Repo: "repo-c"},
		}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// Only repo-a had a PR, and it's merged. repo-b and repo-c never
	// had PRs (no open, closed, or merged) — they should be skipped.
	d.prs.GetMergedPRForBranchFunc = func(_, repo, _ string) (*models.PRDetails, error) {
		if repo == "repo-a" {
			return &models.PRDetails{Number: 1}, nil
		}
		return nil, nil
	}
	d.prs.GetClosedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	var added []string
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc:    func(_, label string) error { added = append(added, label); return nil },
		RemoveLabelFunc: func(_, _ string) error { return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}

	var transitioned string
	d.mergedStatusResolver = &scannertest.StubMergedStatusResolver{
		ResolveMergedStatusFunc: func(_ models.WorkItem) string { return "MODIFIED" },
	}
	d.statusTransitioner = &scannertest.StubStatusTransitioner{
		TransitionStatusFunc: func(_, status string) error {
			transitioned = status
			return nil
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if len(added) != 1 || added[0] != "merged" {
		t.Errorf("added = %v, want [merged]", added)
	}
	if transitioned != "MODIFIED" {
		t.Errorf("transitioned = %q, want %q", transitioned, "MODIFIED")
	}
}

func TestFeedbackScanner_LifecycleLabels_MultiRepo_NoPRsAnywhere(t *testing.T) {
	d := newFeedbackDeps()
	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "repo-a"},
			{Owner: "org", Repo: "repo-b"},
		}, nil
	}
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}
	// No merged, closed, or open PRs on any repo.
	d.prs.GetMergedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}
	d.prs.GetClosedPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	addCalled := false
	d.labels = &scannertest.StubLabelManager{
		AddLabelFunc: func(_, _ string) error { addCalled = true; return nil },
	}
	d.labelResolver = &scannertest.StubFailureLabelResolver{}
	d.lifecycleLabelResolver = &scannertest.StubLifecycleLabelResolver{
		ResolveLifecycleLabelsFunc: func(_ models.WorkItem) models.LifecycleLabels {
			return models.LifecycleLabels{Merged: "merged"}
		},
	}

	runOneFeedbackScan(t, d.scanner(t))

	if addCalled {
		t.Error("merged label should not be applied when no repo had any PR")
	}
}

// --- Skip PR label ---

func TestFeedbackScanner_SkipPRLabel_SkipsPR(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"
	d.prLabeler = &scannertest.StubPRLabeler{
		HasPRLabelFunc: func(_, _ string, _ int, label string) (bool, error) {
			return label == "ai-bot-skip", nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	var commentsFetched bool
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		commentsFetched = true
		return []models.PRComment{
			{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
		}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if submitted {
		t.Error("expected no event to be submitted when PR has skip label")
	}
	if commentsFetched {
		t.Error("expected PR comments not to be fetched when PR has skip label")
	}
}

func TestFeedbackScanner_SkipPRLabel_ErrorFailsOpen(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"
	d.prLabeler = &scannertest.StubPRLabeler{
		HasPRLabelFunc: func(_, _ string, _ int, _ string) (bool, error) {
			return false, errors.New("API error")
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected event to be submitted when skip label check fails (fail-open)")
	}
}

func TestFeedbackScanner_SkipPRLabel_EmptyLabelSkipsCheck(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.SkipPRLabel = ""
	d.prLabeler = &scannertest.StubPRLabeler{
		HasPRLabelFunc: func(_, _ string, _ int, _ string) (bool, error) {
			t.Error("HasPRLabel should not be called when skip label is empty")
			return false, nil
		},
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected event to be submitted when skip label is not configured")
	}
}

func TestFeedbackScanner_SkipPRLabel_NoPRLabeler(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"
	// prLabeler intentionally left nil

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected event to be submitted when PR labeler is not configured")
	}
}

func TestFeedbackScanner_SkipPRLabel_MultiRepo_SkipsOnlyLabeledPR(t *testing.T) {
	d := newFeedbackDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"

	d.repos.LocateReposFunc = func(_ models.WorkItem) ([]models.RepoCoord, error) {
		return []models.RepoCoord{
			{Owner: "org", Repo: "skipped-repo"},
			{Owner: "org", Repo: "active-repo"},
		}, nil
	}

	d.prs.GetPRForBranchFunc = func(_, repo, head string) (*models.PRDetails, error) {
		switch repo {
		case "skipped-repo":
			return &models.PRDetails{Number: 10, Branch: head}, nil
		case "active-repo":
			return &models.PRDetails{Number: 20, Branch: head}, nil
		}
		return nil, nil
	}

	d.prLabeler = &scannertest.StubPRLabeler{
		HasPRLabelFunc: func(_, repo string, _ int, label string) (bool, error) {
			return repo == "skipped-repo" && label == "ai-bot-skip", nil
		},
	}

	commentRepos := map[string]bool{}
	d.prs.GetPRCommentsFunc = func(_, repo string, _ int, _ time.Time) ([]models.PRComment, error) {
		commentRepos[repo] = true
		if repo == "active-repo" {
			return []models.PRComment{
				{ID: 1, Author: models.Author{Username: "reviewer"}, Body: "Fix this"},
			}, nil
		}
		return []models.PRComment{}, nil
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneFeedbackScan(t, d.scanner(t))

	if commentRepos["skipped-repo"] {
		t.Error("expected comments NOT to be fetched for skipped-repo")
	}
	if !commentRepos["active-repo"] {
		t.Error("expected comments to be fetched for active-repo")
	}
	if !submitted {
		t.Error("expected feedback event from active-repo's actionable comments")
	}
}
