package scanner_test

import (
	"context"
	"errors"
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

func TestNewMergeScanner_Validation(t *testing.T) {
	validCfg := scanner.MergeScannerConfig{
		PollInterval: time.Minute,
		BotUsername:  "ai-bot",
		IdleDays:     7,
		IdleLabel:    "ai-bot/idle",
	}

	tests := []struct {
		name       string
		searcher   scanner.IssueSearcher
		submitter  scanner.JobSubmitter
		prs        scanner.PRFetcher
		repos      scanner.RepoLocator
		mergeCheck scanner.MergeabilityChecker
		labeler    scanner.PRLabeler
		cfg        scanner.MergeScannerConfig
		logger     *zap.Logger
		wantErr    string
	}{
		{
			name: "nil searcher", searcher: nil,
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     validCfg, logger: zap.NewNop(), wantErr: "issue searcher",
		},
		{
			name: "nil submitter", searcher: &scannertest.StubIssueSearcher{},
			submitter: nil, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     validCfg, logger: zap.NewNop(), wantErr: "job submitter",
		},
		{
			name: "nil prs", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: nil,
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     validCfg, logger: zap.NewNop(), wantErr: "PR fetcher",
		},
		{
			name: "nil repos", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: nil, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     validCfg, logger: zap.NewNop(), wantErr: "repo locator",
		},
		{
			name: "nil mergeCheck", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: nil,
			labeler: &scannertest.StubPRLabeler{},
			cfg:     validCfg, logger: zap.NewNop(), wantErr: "mergeability checker",
		},
		{
			name: "nil labeler", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: nil,
			cfg:     validCfg, logger: zap.NewNop(), wantErr: "PR labeler",
		},
		{
			name: "zero poll", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     scanner.MergeScannerConfig{BotUsername: "bot"}, logger: zap.NewNop(),
			wantErr: "poll interval",
		},
		{
			name: "empty bot", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     scanner.MergeScannerConfig{PollInterval: time.Minute}, logger: zap.NewNop(),
			wantErr: "bot username",
		},
		{
			name: "nil logger", searcher: &scannertest.StubIssueSearcher{},
			submitter: &scannertest.StubJobSubmitter{}, prs: &scannertest.StubPRFetcher{},
			repos: &scannertest.StubRepoLocator{}, mergeCheck: &scannertest.StubMergeabilityChecker{},
			labeler: &scannertest.StubPRLabeler{},
			cfg:     validCfg, logger: nil, wantErr: "logger",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := scanner.NewMergeScanner(
				tt.searcher, tt.submitter, tt.prs, tt.repos,
				tt.mergeCheck, tt.labeler, tt.cfg, tt.logger)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- Submits merge event for unmergeable PR ---

func TestMergeScanner_SubmitsMergeEventForUnmergeablePR(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
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
	runOneMergeScan(t, s)

	mu.Lock()
	defer mu.Unlock()
	if len(submitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(submitted))
	}
	if submitted[0].Type != jobmanager.JobTypeMerge {
		t.Errorf("expected JobTypeMerge, got %s", submitted[0].Type)
	}
	if submitted[0].TicketKey != "PROJ-1" {
		t.Errorf("expected PROJ-1, got %s", submitted[0].TicketKey)
	}
}

// --- Skips mergeable PR ---

func TestMergeScanner_SkipsMergeablePR(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event for mergeable PR")
	}
}

// --- Skips unknown mergeability (nil) ---

func TestMergeScanner_SkipsUnknownMergeability(t *testing.T) {
	d := newMergeDeps()

	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: nil, BaseBranch: "main"}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event when mergeability is unknown")
	}
}

// --- Skips PR with no items ---

func TestMergeScanner_NoItems(t *testing.T) {
	d := newMergeDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event when no items found")
	}
}

// --- Handles search error ---

func TestMergeScanner_SearchError(t *testing.T) {
	d := newMergeDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return nil, errors.New("search failed")
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event on search error")
	}
}

// --- Circuit breaker stops scan cycle ---

func TestMergeScanner_CircuitBreakerStopsScan(t *testing.T) {
	d := newMergeDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{Key: "PROJ-1"}, {Key: "PROJ-2"}}, nil
	}

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	callCount := 0
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		callCount++
		return nil, jobmanager.ErrCircuitOpen
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if callCount != 1 {
		t.Errorf("expected 1 submit call (circuit breaker should stop), got %d", callCount)
	}
}

// --- Idle detection: labels idle PR and skips ---

func TestMergeScanner_LabelsIdlePRAndSkips(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// Last human comment was 10 days ago (idle threshold is 7).
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "reviewer"},
				Body:      "Looks good",
				Timestamp: time.Now().AddDate(0, 0, -10),
			},
		}, nil
	}

	var labeledPR int
	var labeledWith string
	d.labeler.AddPRLabelFunc = func(_, _ string, number int, label string) error {
		labeledPR = number
		labeledWith = label
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event for idle PR")
	}
	if labeledPR != 42 {
		t.Errorf("expected PR 42 to be labeled, got %d", labeledPR)
	}
	if labeledWith != "ai-bot/idle" {
		t.Errorf("expected label ai-bot/idle, got %s", labeledWith)
	}
}

// --- Idle detection: skips PR already labeled and still idle ---

func TestMergeScanner_SkipsPRWithIdleLabelStillIdle(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// PR created 10 days ago, no human comments — still idle.
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Branch: head,
			URL:       "https://github.com/org/repo/pull/42",
			CreatedAt: time.Now().AddDate(0, 0, -10),
		}, nil
	}

	d.labeler.HasPRLabelFunc = func(_, _ string, _ int, _ string) (bool, error) {
		return true, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	// No human comments — PR is still idle.
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event for labeled idle PR")
	}
}

// --- Idle detection: reactivates PR when human comments after idle label ---

func TestMergeScanner_ReactivatesIdlePROnHumanComment(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.labeler.HasPRLabelFunc = func(_, _ string, _ int, _ string) (bool, error) {
		return true, nil
	}

	// Human commented 2 days ago (within idle threshold of 7).
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "reviewer"},
				Body:      "I still want this merged",
				Timestamp: time.Now().AddDate(0, 0, -2),
			},
		}, nil
	}

	var removedLabel string
	d.labeler.RemovePRLabelFunc = func(_, _ string, _ int, label string) error {
		removedLabel = label
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if !submitted {
		t.Error("should submit event after reactivating idle PR")
	}
	if removedLabel != "ai-bot/idle" {
		t.Errorf("expected idle label to be removed, got %q", removedLabel)
	}
}

// --- Idle detection: reactivates PR even when label removal fails ---

func TestMergeScanner_ReactivatesIdlePREvenWhenLabelRemovalFails(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.labeler.HasPRLabelFunc = func(_, _ string, _ int, _ string) (bool, error) {
		return true, nil
	}

	// Human commented 2 days ago (within idle threshold of 7).
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "reviewer"},
				Body:      "Please fix this",
				Timestamp: time.Now().AddDate(0, 0, -2),
			},
		}, nil
	}

	d.labeler.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error {
		return errors.New("API error")
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if !submitted {
		t.Error("should submit event even when label removal fails (fail-open)")
	}
}

// --- New PR with no human comments is not labeled idle ---

func TestMergeScanner_NewPRNotLabeledIdle(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// PR created 2 days ago (within idle threshold of 7).
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Branch: head,
			URL:       "https://github.com/org/repo/pull/42",
			CreatedAt: time.Now().AddDate(0, 0, -2),
		}, nil
	}

	// Only bot comments — no human activity.
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "ai-bot"},
				Body:      "Working on it.",
				Timestamp: time.Now(),
			},
		}, nil
	}

	labeled := false
	d.labeler.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labeled = true
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if labeled {
		t.Error("should not label a new PR as idle")
	}
	if !submitted {
		t.Error("should submit merge event for new unmergeable PR")
	}
}

// --- Manual label removal prevents re-application ---

func TestMergeScanner_ManualLabelRemovalRespected(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// PR created 10 days ago, no recent human comments — would be
	// idle, but someone removed the idle label 1 day ago.
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Branch: head,
			URL:       "https://github.com/org/repo/pull/42",
			CreatedAt: time.Now().AddDate(0, 0, -10),
		}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	d.labeler.LastLabelRemovalFunc = func(_, _ string, _ int, _ string) (time.Time, error) {
		return time.Now().AddDate(0, 0, -1), nil
	}

	labeled := false
	d.labeler.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labeled = true
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if labeled {
		t.Error("should not re-apply idle label after manual removal")
	}
	if !submitted {
		t.Error("should submit merge event after manual label removal")
	}
}

// --- Manual label removal expires after IdleDays ---

func TestMergeScanner_ManualLabelRemovalExpires(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// PR created 30 days ago, no recent human comments.
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Branch: head,
			URL:       "https://github.com/org/repo/pull/42",
			CreatedAt: time.Now().AddDate(0, 0, -30),
		}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{}, nil
	}

	// Label was removed 10 days ago — older than idle threshold of 7.
	d.labeler.LastLabelRemovalFunc = func(_, _ string, _ int, _ string) (time.Time, error) {
		return time.Now().AddDate(0, 0, -10), nil
	}

	labeled := false
	d.labeler.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labeled = true
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if !labeled {
		t.Error("should re-apply idle label after removal override expires")
	}
	if submitted {
		t.Error("should not submit event for idle PR with expired removal override")
	}
}

// --- Active PR (recent comments) is not labeled ---

func TestMergeScanner_ActivePRNotLabeled(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// Recent human comment (1 day ago, threshold is 7).
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "reviewer"},
				Body:      "Please fix this",
				Timestamp: time.Now().AddDate(0, 0, -1),
			},
		}, nil
	}

	labeled := false
	d.labeler.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labeled = true
		return nil
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
	runOneMergeScan(t, s)

	if labeled {
		t.Error("should not label active PR")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(submitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(submitted))
	}
}

// --- Idle detection disabled when IdleDays is zero ---

func TestMergeScanner_IdleDetectionDisabledWhenZero(t *testing.T) {
	d := newMergeDeps()
	d.cfg.IdleDays = 0

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
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
	runOneMergeScan(t, s)

	mu.Lock()
	defer mu.Unlock()
	if len(submitted) != 1 {
		t.Fatalf("expected 1 event (idle detection disabled), got %d", len(submitted))
	}
}

// --- Bot comments are excluded from activity detection ---

func TestMergeScanner_BotCommentsExcludedFromActivity(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// PR created 10 days ago with only bot comments — no human activity.
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Branch: head,
			URL:       "https://github.com/org/repo/pull/42",
			CreatedAt: time.Now().AddDate(0, 0, -10),
		}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "ai-bot"},
				Body:      "Addressed in abc123.",
				Timestamp: time.Now(),
			},
		}, nil
	}

	labeled := false
	d.labeler.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labeled = true
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	// No human comments at all — lastHumanCommentTime returns zero time,
	// which is before the idle threshold, so the PR is labeled idle.
	if !labeled {
		t.Error("expected PR to be labeled idle when only bot comments exist")
	}
	if submitted {
		t.Error("should not submit event for idle PR")
	}
}

// --- Known bots excluded from idle activity ---

func TestMergeScanner_KnownBotExcludedFromActivity(t *testing.T) {
	d := newMergeDeps()
	d.cfg.KnownBotUsernames = []string{"coderabbitai"}
	d.cfg.IgnoredUsernames = []string{"packit-as-a-service[bot]"}

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	// PR created 10 days ago. Only known bot and ignored user have
	// recent comments — no human activity.
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		return &models.PRDetails{
			Number: 42, Branch: head,
			URL:       "https://github.com/org/repo/pull/42",
			CreatedAt: time.Now().AddDate(0, 0, -10),
		}, nil
	}
	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{
				ID:        1,
				Author:    models.Author{Username: "coderabbitai"},
				Body:      "LGTM",
				Timestamp: time.Now(),
			},
			{
				ID:        2,
				Author:    models.Author{Username: "packit-as-a-service[bot]"},
				Body:      "Build succeeded",
				Timestamp: time.Now(),
			},
		}, nil
	}

	labeled := false
	d.labeler.AddPRLabelFunc = func(_, _ string, _ int, _ string) error {
		labeled = true
		return nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if !labeled {
		t.Error("expected PR to be labeled idle when only known bots commented")
	}
	if submitted {
		t.Error("should not submit event for idle PR")
	}
}

// --- Fork workflow uses correct head format ---

func TestMergeScanner_ForkWorkflowHead(t *testing.T) {
	d := newMergeDeps()
	d.repos.ForkOwnerHeadsFunc = func(_ models.WorkItem, branchName string) []string {
		return []string{"fork-user:" + branchName, branchName}
	}

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	var headUsed string
	d.prs.GetPRForBranchFunc = func(_, _, head string) (*models.PRDetails, error) {
		headUsed = head
		return &models.PRDetails{Number: 42, Branch: "ai-bot/PROJ-1"}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	expected := "fork-user:ai-bot/PROJ-1"
	if headUsed != expected {
		t.Errorf("expected head %q, got %q", expected, headUsed)
	}
}

// --- Lifecycle: double start returns error ---

func TestMergeScanner_DoubleStartReturnsError(t *testing.T) {
	d := newMergeDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{}, nil
	}

	s := d.scanner(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.Start(ctx); err == nil {
		t.Error("expected error on double start")
	}
}

// --- Duplicate job is handled gracefully ---

func TestMergeScanner_DuplicateJobHandled(t *testing.T) {
	d := newMergeDeps()

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		return nil, jobmanager.ErrDuplicateJob
	}

	s := d.scanner(t)
	runOneMergeScan(t, s) // Should not panic or error.
}

// --- No PR found for repo is handled gracefully ---

func TestMergeScanner_NoPRFound(t *testing.T) {
	d := newMergeDeps()
	d.prs.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
		return nil, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	s := d.scanner(t)
	runOneMergeScan(t, s)

	if submitted {
		t.Error("should not submit event when no PR exists")
	}
}

// --- Test helpers ---

type mergeDeps struct {
	searcher   *scannertest.StubIssueSearcher
	submitter  *scannertest.StubJobSubmitter
	prs        *scannertest.StubPRFetcher
	repos      *scannertest.StubRepoLocator
	mergeCheck *scannertest.StubMergeabilityChecker
	labeler    *scannertest.StubPRLabeler
	cfg        scanner.MergeScannerConfig
}

func newMergeDeps() *mergeDeps {
	return &mergeDeps{
		searcher: &scannertest.StubIssueSearcher{
			SearchWorkItemsFunc: func(_ models.SearchCriteria) ([]models.WorkItem, error) {
				return []models.WorkItem{{Key: "PROJ-1"}}, nil
			},
		},
		submitter: &scannertest.StubJobSubmitter{},
		prs: &scannertest.StubPRFetcher{
			GetPRForBranchFunc: func(_, _, head string) (*models.PRDetails, error) {
				return &models.PRDetails{
					Number:    42,
					Branch:    head,
					URL:       "https://github.com/org/repo/pull/42",
					CreatedAt: time.Now().AddDate(0, 0, -1),
				}, nil
			},
			GetPRCommentsFunc: func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
				return []models.PRComment{
					{
						ID:        1,
						Author:    models.Author{Username: "reviewer"},
						Body:      "Review comment",
						Timestamp: time.Now(),
					},
				}, nil
			},
		},
		repos: &scannertest.StubRepoLocator{
			LocateReposFunc: func(_ models.WorkItem) ([]models.RepoCoord, error) {
				return []models.RepoCoord{{Owner: "org", Repo: "repo"}}, nil
			},
		},
		mergeCheck: &scannertest.StubMergeabilityChecker{},
		labeler:    &scannertest.StubPRLabeler{},
		cfg: scanner.MergeScannerConfig{
			PollInterval: time.Hour,
			BotUsername:  "ai-bot",
			IdleDays:     7,
			IdleLabel:    "ai-bot/idle",
		},
	}
}

func (d *mergeDeps) scanner(t *testing.T, opts ...scanner.MergeScannerOption) *scanner.MergeScanner {
	t.Helper()
	s, err := scanner.NewMergeScanner(
		d.searcher, d.submitter, d.prs, d.repos,
		d.mergeCheck, d.labeler, d.cfg, zap.NewNop(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runOneMergeScan(t *testing.T, s *scanner.MergeScanner) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}

	time.Sleep(50 * time.Millisecond)
	s.Stop()
}

// --- Skip PR label ---

func TestMergeScanner_SkipPRLabel_SkipsPR(t *testing.T) {
	d := newMergeDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"
	// Returns true only for the skip label. The idle label check
	// (isIdlePR) is never reached because hasSkipLabel runs first
	// and continues past the PR.
	d.labeler.HasPRLabelFunc = func(_, _ string, _ int, label string) (bool, error) {
		return label == "ai-bot-skip", nil
	}

	mergeChecked := false
	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		mergeChecked = true
		return &models.PRMergeState{Mergeable: &mergeable}, nil
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t))

	if mergeChecked {
		t.Error("expected mergeability check to be skipped when PR has skip label")
	}
	if submitted {
		t.Error("expected no merge event when PR has skip label")
	}
}

func TestMergeScanner_SkipPRLabel_ErrorFailsOpen(t *testing.T) {
	d := newMergeDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"
	d.labeler.HasPRLabelFunc = func(_, _ string, _ int, label string) (bool, error) {
		if label == "ai-bot-skip" {
			return false, errors.New("API error")
		}
		return false, nil
	}
	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable}, nil
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t))

	if !submitted {
		t.Error("expected merge event when skip label check fails (fail-open)")
	}
}

func TestMergeScanner_SkipPRLabel_MultiRepo_SkipsOnlyLabeledPR(t *testing.T) {
	d := newMergeDeps()
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

	d.labeler.HasPRLabelFunc = func(_, repo string, _ int, label string) (bool, error) {
		return repo == "skipped-repo" && label == "ai-bot-skip", nil
	}

	mergeCheckedRepos := map[string]bool{}
	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, repo string, _ int) (*models.PRMergeState, error) {
		mergeCheckedRepos[repo] = true
		return &models.PRMergeState{Mergeable: &mergeable}, nil
	}

	var submitted bool
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t))

	if mergeCheckedRepos["skipped-repo"] {
		t.Error("expected mergeability NOT to be checked for skipped-repo")
	}
	if !mergeCheckedRepos["active-repo"] {
		t.Error("expected mergeability to be checked for active-repo")
	}
	if !submitted {
		t.Error("expected merge event from active-repo's unmergeable PR")
	}
}

// --- Merge command detection ---

func TestMergeScanner_SyncCommand_DetectedAndSubmitted(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot sync", Timestamp: time.Now()},
		}, nil
	}

	var submitted []jobmanager.Event
	d.submitter.SubmitFunc = func(e jobmanager.Event) (*jobmanager.Job, error) {
		submitted = append(submitted, e)
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if len(submitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(submitted))
	}
	if submitted[0].Type != jobmanager.JobTypeMerge {
		t.Errorf("expected JobTypeMerge, got %s", submitted[0].Type)
	}
	if submitted[0].CommandSource == nil {
		t.Fatal("expected CommandSource to be set")
	}
	if submitted[0].CommandSource.CommentID != 100 {
		t.Errorf("expected CommentID 100, got %d", submitted[0].CommandSource.CommentID)
	}
	if submitted[0].CommandSource.PRNumber != 42 {
		t.Errorf("expected PRNumber 42, got %d", submitted[0].CommandSource.PRNumber)
	}
}

func TestMergeScanner_SyncCommand_AlreadyAddressed(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot sync"},
			{ID: 200, Author: models.Author{Username: "ai-bot[bot]"}, Body: "Merging from main -> ai-bot/PROJ-1\n<!-- addressed: 100 -->"},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event when sync command already addressed by bot reply")
	}
}

func TestMergeScanner_SyncCommand_AlreadyAddressed_BotSuffix(t *testing.T) {
	d := newMergeDeps()
	d.cfg.BotUsername = "ai-bot[bot]"

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot sync"},
			{ID: 200, Author: models.Author{Username: "ai-bot[bot]"}, Body: "Merging from main -> ai-bot/PROJ-1\n<!-- addressed: 100 -->"},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event when bot username has [bot] suffix and command is already addressed")
	}
}

func TestMergeScanner_SyncCommand_SkippedWithoutOption(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot sync"},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t))

	if submitted {
		t.Error("expected no merge event when sync commands not configured")
	}
}

func TestMergeScanner_SyncCommand_IgnoresBotOwnComment(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "ai-bot[bot]"}, Body: "@ai-bot sync"},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event for bot's own sync command")
	}
}

func TestMergeScanner_SyncCommand_SkippedWhenPRHasSkipLabel(t *testing.T) {
	d := newMergeDeps()
	d.cfg.SkipPRLabel = "ai-bot-skip"

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.labeler.HasPRLabelFunc = func(_, _ string, _ int, label string) (bool, error) {
		return label == "ai-bot-skip", nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot sync"},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event when PR has skip label")
	}
}

func TestMergeScanner_SyncCommand_NoCommandPresent(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "Looks good to me!"},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event when no sync command present")
	}
}

func TestMergeScanner_SyncCommand_CommentFetchError(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return nil, errors.New("API error")
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event when comment fetch fails")
	}
}

func TestMergeScanner_SyncCommand_IgnoresReviewComments(t *testing.T) {
	d := newMergeDeps()

	mergeable := true
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	d.prs.GetPRCommentsFunc = func(_, _ string, _ int, _ time.Time) ([]models.PRComment, error) {
		return []models.PRComment{
			{ID: 100, Author: models.Author{Username: "reviewer"}, Body: "@ai-bot sync", IsReviewComment: true},
		}, nil
	}

	submitted := false
	d.submitter.SubmitFunc = func(_ jobmanager.Event) (*jobmanager.Job, error) {
		submitted = true
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if submitted {
		t.Error("expected no merge event for review comment sync commands")
	}
}

func TestMergeScanner_SyncCommand_ConflictsTakePriority(t *testing.T) {
	d := newMergeDeps()
	d.cfg.IdleDays = 0

	mergeable := false
	d.mergeCheck.GetPRMergeabilityFunc = func(_, _ string, _ int) (*models.PRMergeState, error) {
		return &models.PRMergeState{Mergeable: &mergeable, BaseBranch: "main"}, nil
	}

	var submitted []jobmanager.Event
	d.submitter.SubmitFunc = func(e jobmanager.Event) (*jobmanager.Job, error) {
		submitted = append(submitted, e)
		return &jobmanager.Job{}, nil
	}

	runOneMergeScan(t, d.scanner(t, scanner.WithMergeCommands()))

	if len(submitted) != 1 {
		t.Fatalf("expected 1 event, got %d", len(submitted))
	}
	if submitted[0].CommandSource != nil {
		t.Error("expected no CommandSource when merge triggered by conflicts")
	}
}
