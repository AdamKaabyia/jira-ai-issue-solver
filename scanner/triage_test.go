package scanner_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/scanner"
	"jira-ai-issue-solver/scanner/scannertest"
)

// --- Construction validation ---

func TestNewTriageLabelScanner_Validation(t *testing.T) {
	validCfg := scanner.TriageLabelScannerConfig{
		PollInterval: time.Minute,
		BotUsername:  "ai-bot",
	}

	tests := []struct {
		name     string
		searcher scanner.IssueSearcher
		labels   scanner.LabelManager
		resolver scanner.TriageLabelResolver
		cfg      scanner.TriageLabelScannerConfig
		logger   *zap.Logger
		wantErr  string
	}{
		{
			name: "nil searcher", searcher: nil,
			labels: &scannertest.StubLabelManager{}, resolver: &scannertest.StubTriageLabelResolver{},
			cfg: validCfg, logger: zap.NewNop(), wantErr: "issue searcher",
		},
		{
			name: "nil labels", searcher: &scannertest.StubIssueSearcher{},
			labels: nil, resolver: &scannertest.StubTriageLabelResolver{},
			cfg: validCfg, logger: zap.NewNop(), wantErr: "label manager",
		},
		{
			name: "nil resolver", searcher: &scannertest.StubIssueSearcher{},
			labels: &scannertest.StubLabelManager{}, resolver: nil,
			cfg: validCfg, logger: zap.NewNop(), wantErr: "triage label resolver",
		},
		{
			name: "zero poll", searcher: &scannertest.StubIssueSearcher{},
			labels: &scannertest.StubLabelManager{}, resolver: &scannertest.StubTriageLabelResolver{},
			cfg: scanner.TriageLabelScannerConfig{BotUsername: "bot"}, logger: zap.NewNop(),
			wantErr: "poll interval",
		},
		{
			name: "empty bot", searcher: &scannertest.StubIssueSearcher{},
			labels: &scannertest.StubLabelManager{}, resolver: &scannertest.StubTriageLabelResolver{},
			cfg: scanner.TriageLabelScannerConfig{PollInterval: time.Minute}, logger: zap.NewNop(),
			wantErr: "bot username",
		},
		{
			name: "nil logger", searcher: &scannertest.StubIssueSearcher{},
			labels: &scannertest.StubLabelManager{}, resolver: &scannertest.StubTriageLabelResolver{},
			cfg: validCfg, logger: nil, wantErr: "logger",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := scanner.NewTriageLabelScanner(
				tt.searcher, tt.labels, tt.resolver, tt.cfg, tt.logger)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// --- Happy path: cleans up triage labels ---

func TestTriageLabelScanner_CleansUpTriageLabels(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-missing-info", "other-label"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	var added, removed []string
	d.labels.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
	d.labels.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

	runOneTriageScan(t, d)

	if len(removed) != 1 || removed[0] != "jira-triage-missing-info" {
		t.Errorf("removed = %v, want [jira-triage-missing-info]", removed)
	}
	if len(added) != 1 || added[0] != "jira-triage-stale" {
		t.Errorf("added = %v, want [jira-triage-stale]", added)
	}
}

// --- Removes multiple active labels ---

func TestTriageLabelScanner_RemovesMultipleActiveLabels(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-missing-info", "jira-triage-not-fixable"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	var removed []string
	d.labels.AddLabelFunc = func(_, _ string) error { return nil }
	d.labels.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

	runOneTriageScan(t, d)

	if len(removed) != 2 {
		t.Fatalf("removed = %v, want 2 labels", removed)
	}
	removedSet := make(map[string]bool)
	for _, l := range removed {
		removedSet[l] = true
	}
	if !removedSet["jira-triage-missing-info"] || !removedSet["jira-triage-not-fixable"] {
		t.Errorf("removed = %v, want both active labels", removed)
	}
}

// --- Skip: still in New status ---

func TestTriageLabelScanner_SkipsNewStatus(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "New",
			Labels:   []string{"jira-triage-missing-info"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	labelCalled := false
	d.labels.AddLabelFunc = func(_, _ string) error { labelCalled = true; return nil }
	d.labels.RemoveLabelFunc = func(_, _ string) error { labelCalled = true; return nil }

	runOneTriageScan(t, d)

	if labelCalled {
		t.Error("expected no label operations for ticket still in New status")
	}
}

// --- Skip: assigned to bot ---

func TestTriageLabelScanner_SkipsBotAssignee(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-missing-info"},
			Assignee: &models.Author{Username: "ai-bot"},
		}}, nil
	}

	labelCalled := false
	d.labels.AddLabelFunc = func(_, _ string) error { labelCalled = true; return nil }
	d.labels.RemoveLabelFunc = func(_, _ string) error { labelCalled = true; return nil }

	runOneTriageScan(t, d)

	if labelCalled {
		t.Error("expected no label operations for bot-assigned ticket")
	}
}

// --- Skip: unassigned ---

func TestTriageLabelScanner_SkipsUnassigned(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:    "OSAC-100",
			Status: "In Progress",
			Labels: []string{"jira-triage-missing-info"},
		}}, nil
	}

	labelCalled := false
	d.labels.AddLabelFunc = func(_, _ string) error { labelCalled = true; return nil }
	d.labels.RemoveLabelFunc = func(_, _ string) error { labelCalled = true; return nil }

	runOneTriageScan(t, d)

	if labelCalled {
		t.Error("expected no label operations for unassigned ticket")
	}
}

// --- Skip: only stale label, no active labels ---

func TestTriageLabelScanner_SkipsOnlyStale(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-stale"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	labelCalled := false
	d.labels.AddLabelFunc = func(_, _ string) error { labelCalled = true; return nil }
	d.labels.RemoveLabelFunc = func(_, _ string) error { labelCalled = true; return nil }

	runOneTriageScan(t, d)

	if labelCalled {
		t.Error("expected no label operations when ticket has only stale label")
	}
}

// --- Skip: triage labels not configured ---

func TestTriageLabelScanner_SkipsNotConfigured(t *testing.T) {
	d := newTriageDeps()
	d.resolver.ResolveTriageLabelsFunc = func(_ models.WorkItem) models.TriageLabels {
		return models.TriageLabels{}
	}
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-missing-info"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	labelCalled := false
	d.labels.AddLabelFunc = func(_, _ string) error { labelCalled = true; return nil }
	d.labels.RemoveLabelFunc = func(_, _ string) error { labelCalled = true; return nil }

	runOneTriageScan(t, d)

	if labelCalled {
		t.Error("expected no label operations when triage labels not configured")
	}
}

// --- Multiple tickets: only qualifying ones cleaned up ---

func TestTriageLabelScanner_MultipleTickets_MixedEligibility(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{
			{
				Key:      "OSAC-100",
				Status:   "In Progress",
				Labels:   []string{"jira-triage-missing-info"},
				Assignee: &models.Author{Username: "human-dev"},
			},
			{
				Key:      "OSAC-101",
				Status:   "New",
				Labels:   []string{"jira-triage-not-fixable"},
				Assignee: &models.Author{Username: "human-dev"},
			},
			{
				Key:      "OSAC-102",
				Status:   "In Review",
				Labels:   []string{"jira-triage-not-fixable"},
				Assignee: &models.Author{Username: "another-human"},
			},
		}, nil
	}

	var addedKeys, removedKeys []string
	d.labels.AddLabelFunc = func(key, _ string) error { addedKeys = append(addedKeys, key); return nil }
	d.labels.RemoveLabelFunc = func(key, _ string) error { removedKeys = append(removedKeys, key); return nil }

	runOneTriageScan(t, d)

	if len(addedKeys) != 2 {
		t.Fatalf("added stale to %d tickets, want 2", len(addedKeys))
	}
	if addedKeys[0] != "OSAC-100" || addedKeys[1] != "OSAC-102" {
		t.Errorf("added to = %v, want [OSAC-100, OSAC-102]", addedKeys)
	}
	if len(removedKeys) != 2 {
		t.Fatalf("removed from %d tickets, want 2", len(removedKeys))
	}
}

// --- Only removes labels the ticket actually has ---

func TestTriageLabelScanner_OnlyRemovesPresent(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-not-fixable"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	var removed []string
	d.labels.AddLabelFunc = func(_, _ string) error { return nil }
	d.labels.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

	runOneTriageScan(t, d)

	if len(removed) != 1 || removed[0] != "jira-triage-not-fixable" {
		t.Errorf("removed = %v, want [jira-triage-not-fixable] (should not remove missing-info)", removed)
	}
}

// --- No active labels on ticket: no-op (cross-project label match) ---

func TestTriageLabelScanner_NoActiveLabels_Skipped(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"other-label"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	labelCalled := false
	d.labels.AddLabelFunc = func(_, _ string) error { labelCalled = true; return nil }
	d.labels.RemoveLabelFunc = func(_, _ string) error { labelCalled = true; return nil }

	runOneTriageScan(t, d)

	if labelCalled {
		t.Error("expected no label operations when ticket has no active triage labels")
	}
}

// --- Already stale but still has active label: removes active, skips adding stale ---

func TestTriageLabelScanner_AlreadyStale_StillRemovesActive(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-stale", "jira-triage-missing-info"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	var added, removed []string
	d.labels.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
	d.labels.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

	runOneTriageScan(t, d)

	if len(removed) != 1 || removed[0] != "jira-triage-missing-info" {
		t.Errorf("removed = %v, want [jira-triage-missing-info]", removed)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want empty (stale already present)", added)
	}
}

// --- AddLabel failure preserves active labels for retry ---

func TestTriageLabelScanner_AddStaleFailure_PreservesActiveLabels(t *testing.T) {
	d := newTriageDeps()
	d.searcher.SearchWorkItemsFunc = func(_ models.SearchCriteria) ([]models.WorkItem, error) {
		return []models.WorkItem{{
			Key:      "OSAC-100",
			Status:   "In Progress",
			Labels:   []string{"jira-triage-missing-info"},
			Assignee: &models.Author{Username: "human-dev"},
		}}, nil
	}

	var removed []string
	d.labels.AddLabelFunc = func(_, _ string) error { return errors.New("Jira API error") }
	d.labels.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

	runOneTriageScan(t, d)

	if len(removed) != 0 {
		t.Errorf("removed = %v, want empty (active labels should be preserved when stale add fails)", removed)
	}
}

// --- Start/Stop lifecycle ---

func TestTriageLabelScanner_StartWhileRunning(t *testing.T) {
	d := newTriageDeps()
	s := buildTriageScanner(t, d)

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

func TestTriageLabelScanner_StopWithoutStart(t *testing.T) {
	d := newTriageDeps()
	s := buildTriageScanner(t, d)
	s.Stop()
}

// --- helpers ---

type triageDeps struct {
	searcher *scannertest.StubIssueSearcher
	labels   *scannertest.StubLabelManager
	resolver *scannertest.StubTriageLabelResolver
	cfg      scanner.TriageLabelScannerConfig
}

func newTriageDeps() *triageDeps {
	return &triageDeps{
		searcher: &scannertest.StubIssueSearcher{
			SearchWorkItemsFunc: func(_ models.SearchCriteria) ([]models.WorkItem, error) {
				return []models.WorkItem{}, nil
			},
		},
		labels: &scannertest.StubLabelManager{},
		resolver: &scannertest.StubTriageLabelResolver{
			ResolveTriageLabelsFunc: func(_ models.WorkItem) models.TriageLabels {
				return models.TriageLabels{
					Active:    []string{"jira-triage-missing-info", "jira-triage-not-fixable"},
					Stale:     "jira-triage-stale",
					NewStatus: "New",
				}
			},
		},
		cfg: scanner.TriageLabelScannerConfig{
			PollInterval: time.Hour,
			BotUsername:  "ai-bot",
		},
	}
}

func buildTriageScanner(t *testing.T, d *triageDeps) *scanner.TriageLabelScanner {
	t.Helper()
	s, err := scanner.NewTriageLabelScanner(
		d.searcher, d.labels, d.resolver, d.cfg, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runOneTriageScan runs a single scan cycle synchronously via the
// exported ScanOnce method. No goroutine, no timing, deterministic.
func runOneTriageScan(t *testing.T, d *triageDeps) {
	t.Helper()
	s := buildTriageScanner(t, d)
	s.ScanOnce(context.Background())
}
