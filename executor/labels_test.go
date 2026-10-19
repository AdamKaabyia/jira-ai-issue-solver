package executor_test

import (
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"jira-ai-issue-solver/executor"
	"jira-ai-issue-solver/models"
)

func TestSetPipelineLabel(t *testing.T) {
	fl := models.FailureLabels{
		CIFailing:       "ci-fail",
		Rejected:        "rejected",
		Blocked:         "blocked",
		ForkUserMissing: "fork-missing",
	}
	ll := models.LifecycleLabels{
		Queued: "jira-autofix",
		Review: "jira-autofix-review",
		Merged: "jira-autofix-merged",
	}
	allLabels := models.AllPipelineLabels(fl, ll)

	t.Run("adds target and removes all others from both groups", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", allLabels, "blocked")

		if len(added) != 1 || added[0] != "blocked" {
			t.Errorf("added = %v, want [blocked]", added)
		}
		if len(removed) != 6 {
			t.Fatalf("removed = %v, want 6 entries (3 other failure + 3 lifecycle)", removed)
		}
		wantRemoved := map[string]bool{
			"ci-fail": true, "rejected": true, "fork-missing": true,
			"jira-autofix": true, "jira-autofix-review": true, "jira-autofix-merged": true,
		}
		for _, l := range removed {
			if !wantRemoved[l] {
				t.Errorf("unexpected removal of %q", l)
			}
		}
	})

	t.Run("lifecycle target removes all failure labels", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", allLabels, "jira-autofix-review")

		if len(added) != 1 || added[0] != "jira-autofix-review" {
			t.Errorf("added = %v, want [jira-autofix-review]", added)
		}
		removedSet := make(map[string]bool, len(removed))
		for _, l := range removed {
			removedSet[l] = true
		}
		for _, fl := range []string{"ci-fail", "rejected", "blocked", "fork-missing"} {
			if !removedSet[fl] {
				t.Errorf("expected failure label %q to be removed", fl)
			}
		}
		if !removedSet["jira-autofix"] {
			t.Error("expected queued label to be removed")
		}
		if !removedSet["jira-autofix-merged"] {
			t.Error("expected merged label to be removed")
		}
	})

	t.Run("merged removes review and queued and all failure", func(t *testing.T) {
		var removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, _ string) error { return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", allLabels, "jira-autofix-merged")

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
		if !removedSet["blocked"] {
			t.Error("expected blocked label to be removed")
		}
	})

	t.Run("empty target is a no-op", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", allLabels, "")

		if len(added) != 0 {
			t.Errorf("added = %v, want empty", added)
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want empty (empty target should no-op)", removed)
		}
	})

	t.Run("skips empty labels in config", func(t *testing.T) {
		partial := models.AllPipelineLabels(
			models.FailureLabels{Blocked: "blocked"},
			models.LifecycleLabels{},
		)
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", partial, "blocked")

		if len(added) != 1 || added[0] != "blocked" {
			t.Errorf("added = %v, want [blocked]", added)
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want empty (no other labels configured)", removed)
		}
	})

	t.Run("noop when all labels empty", func(t *testing.T) {
		empty := models.AllPipelineLabels(models.FailureLabels{}, models.LifecycleLabels{})
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", empty, "")

		if len(added) != 0 {
			t.Errorf("added = %v, want empty", added)
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want empty", removed)
		}
	})

}

func TestClearFailureLabels(t *testing.T) {
	t.Run("removes only executor-owned labels", func(t *testing.T) {
		fl := models.FailureLabels{
			CIFailing:       "ci-fail",
			Rejected:        "rejected",
			Blocked:         "blocked",
			ForkUserMissing: "fork-missing",
		}
		var removed []string
		d := newTestDeps(t)
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.ClearFailureLabels(p, zap.NewNop(), "TEST-1", fl)

		if len(removed) != 2 {
			t.Fatalf("removed = %v, want 2 entries (blocked + fork-missing)", removed)
		}
		counts := make(map[string]int, len(removed))
		for _, l := range removed {
			counts[l]++
		}
		for _, want := range []string{"blocked", "fork-missing"} {
			if counts[want] != 1 {
				t.Errorf("expected exactly 1 removal of %q, got %d", want, counts[want])
			}
		}
	})

	t.Run("skips empty labels", func(t *testing.T) {
		fl := models.FailureLabels{Blocked: "blocked"}
		var removed []string
		d := newTestDeps(t)
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.ClearFailureLabels(p, zap.NewNop(), "TEST-1", fl)

		if len(removed) != 1 || removed[0] != "blocked" {
			t.Errorf("removed = %v, want [blocked]", removed)
		}
	})
}

func TestValidateForkMode(t *testing.T) {
	t.Run("passes when fork mode disabled", func(t *testing.T) {
		d := newTestDeps(t)
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: false, GitHubUsername: ""},
		)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("passes when fork mode disabled even with username", func(t *testing.T) {
		d := newTestDeps(t)
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: false, GitHubUsername: "alice"},
		)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("passes when fork mode enabled with username", func(t *testing.T) {
		d := newTestDeps(t)
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: true, GitHubUsername: "alice"},
		)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("fails when fork mode enabled without username and unassigned", func(t *testing.T) {
		var added []string
		var commentBody string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, _ string) error { return nil }
		d.tracker.AddCommentFunc = func(_, body string) error { commentBody = body; return nil }

		fl := models.FailureLabels{ForkUserMissing: "fork-missing"}
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: true, GitHubUsername: "", FailureLabels: fl},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "fork mode requires assignee GitHub mapping") {
			t.Errorf("error = %q, want it to mention fork mode requirement", err.Error())
		}
		if strings.Contains(err.Error(), "unassigned") {
			t.Error("error should not contain assignee details (PII redaction)")
		}
		if len(added) != 1 || added[0] != "fork-missing" {
			t.Errorf("added = %v, want [fork-missing]", added)
		}
		if !strings.Contains(commentBody, "[AI-BOT-STATUS]") {
			t.Errorf("comment body = %q, want it to contain [AI-BOT-STATUS]", commentBody)
		}
		if !strings.Contains(commentBody, "unassigned") {
			t.Errorf("comment body = %q, want it to mention 'unassigned'", commentBody)
		}
	})

	t.Run("fails when fork mode enabled with assignee not in mapping", func(t *testing.T) {
		var added []string
		var commentBody string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, _ string) error { return nil }
		d.tracker.AddCommentFunc = func(_, body string) error { commentBody = body; return nil }

		fl := models.FailureLabels{ForkUserMissing: "fork-missing"}
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{
				Key:      "TEST-1",
				Assignee: &models.Author{Email: "bob@example.com"},
			},
			&models.ProjectSettings{ForkMode: true, GitHubUsername: "", FailureLabels: fl},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !strings.Contains(err.Error(), "fork mode requires assignee GitHub mapping") {
			t.Errorf("error = %q, want it to mention fork mode requirement", err.Error())
		}
		if strings.Contains(err.Error(), "bob@example.com") {
			t.Error("error should not contain assignee email (PII redaction)")
		}
		if len(added) != 1 || added[0] != "fork-missing" {
			t.Errorf("added = %v, want [fork-missing]", added)
		}
		if !strings.Contains(commentBody, "[AI-BOT-STATUS]") {
			t.Errorf("comment body = %q, want it to contain [AI-BOT-STATUS]", commentBody)
		}
	})

	t.Run("no label applied when fork_user_missing label not configured", func(t *testing.T) {
		var added []string
		var commentPosted bool
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, _ string) error { return nil }
		d.tracker.AddCommentFunc = func(_, _ string) error { commentPosted = true; return nil }

		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: true, GitHubUsername: "", FailureLabels: models.FailureLabels{}},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if len(added) != 0 {
			t.Errorf("added = %v, want empty (label not configured)", added)
		}
		if !commentPosted {
			t.Error("expected status comment to be posted even when label not configured")
		}
	})

	t.Run("does not clear other labels when fork_user_missing not configured", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }
		d.tracker.AddCommentFunc = func(_, _ string) error { return nil }

		fl := models.FailureLabels{
			CIFailing: "ci-fail",
			Rejected:  "rejected",
			Blocked:   "blocked",
		}
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: true, GitHubUsername: "", FailureLabels: fl},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if len(added) != 0 {
			t.Errorf("added = %v, want empty", added)
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want empty (other labels should not be cleared)", removed)
		}
	})

	t.Run("clears lifecycle labels when setting fork_user_missing", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, label string) error { added = append(added, label); return nil }
		d.tracker.RemoveLabelFunc = func(_, label string) error { removed = append(removed, label); return nil }
		d.tracker.AddCommentFunc = func(_, _ string) error { return nil }

		fl := models.FailureLabels{ForkUserMissing: "fork-missing"}
		ll := models.LifecycleLabels{
			Queued: "jira-autofix",
			Review: "jira-autofix-review",
		}
		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{ForkMode: true, GitHubUsername: "", FailureLabels: fl, LifecycleLabels: ll},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if len(added) != 1 || added[0] != "fork-missing" {
			t.Errorf("added = %v, want [fork-missing]", added)
		}
		removedSet := make(map[string]bool, len(removed))
		for _, l := range removed {
			removedSet[l] = true
		}
		if !removedSet["jira-autofix"] {
			t.Error("expected queued lifecycle label to be removed")
		}
		if !removedSet["jira-autofix-review"] {
			t.Error("expected review lifecycle label to be removed")
		}
	})

	t.Run("skips comment when error comments disabled", func(t *testing.T) {
		var commentPosted bool
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, _ string) error { return nil }
		d.tracker.RemoveLabelFunc = func(_, _ string) error { return nil }
		d.tracker.AddCommentFunc = func(_, _ string) error { commentPosted = true; return nil }

		p := d.pipeline(t)

		err := executor.ValidateForkMode(p, zap.NewNop(), "TEST-1",
			&models.WorkItem{Key: "TEST-1"},
			&models.ProjectSettings{
				ForkMode:             true,
				GitHubUsername:       "",
				DisableErrorComments: true,
			},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if commentPosted {
			t.Error("comment should not be posted when DisableErrorComments is true")
		}
	})
}

func TestValidationLabel(t *testing.T) {
	vl := models.PRValidationLabels{
		ValidationFailed: "ai-validation-failed",
		NonzeroExit:      "ai-nonzero-exit",
	}

	tests := []struct {
		name             string
		validationPassed *bool
		exitCode         int
		want             string
	}{
		{"all OK", nil, 0, ""},
		{"validation explicitly passed", boolPtr(true), 0, ""},
		{"validation failed", boolPtr(false), 0, "ai-validation-failed"},
		{"nonzero exit", nil, 1, "ai-nonzero-exit"},
		{"validation passed but nonzero exit", boolPtr(true), 1, "ai-nonzero-exit"},
		{"validation failed takes precedence over nonzero exit", boolPtr(false), 1, "ai-validation-failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := executor.SessionOutput{ValidationPassed: tt.validationPassed}
			got := executor.ValidationLabel(session, tt.exitCode, vl)
			if got != tt.want {
				t.Errorf("validationLabel() = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("falls through to nonzero_exit when validation_failed is disabled", func(t *testing.T) {
		partial := models.PRValidationLabels{NonzeroExit: "ai-nonzero-exit"}
		session := executor.SessionOutput{ValidationPassed: boolPtr(false)}
		got := executor.ValidationLabel(session, 1, partial)
		if got != "ai-nonzero-exit" {
			t.Errorf("validationLabel() = %q, want ai-nonzero-exit", got)
		}
	})
}

func TestValidationPassed(t *testing.T) {
	tests := []struct {
		name             string
		validationPassed *bool
		exitCode         int
		want             bool
	}{
		{"all OK", nil, 0, true},
		{"validation explicitly passed", boolPtr(true), 0, true},
		{"validation failed", boolPtr(false), 0, false},
		{"nonzero exit", nil, 1, false},
		{"validation passed but nonzero exit", boolPtr(true), 1, false},
		{"validation failed and nonzero exit", boolPtr(false), 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := executor.SessionOutput{ValidationPassed: tt.validationPassed}
			got := executor.ValidationPassed(session, tt.exitCode)
			if got != tt.want {
				t.Errorf("validationPassed() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSetPRValidationLabel(t *testing.T) {
	vl := models.PRValidationLabels{
		ValidationFailed: "ai-validation-failed",
		NonzeroExit:      "ai-nonzero-exit",
	}

	t.Run("adds target and removes others", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error { added = append(added, label); return nil }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPRValidationLabel(p, zap.NewNop(), "org", "repo", 42, vl, "ai-validation-failed")

		if len(added) != 1 || added[0] != "ai-validation-failed" {
			t.Errorf("added = %v, want [ai-validation-failed]", added)
		}
		if len(removed) != 1 || removed[0] != "ai-nonzero-exit" {
			t.Errorf("removed = %v, want [ai-nonzero-exit]", removed)
		}
	})

	t.Run("empty target only removes others", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error { added = append(added, label); return nil }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPRValidationLabel(p, zap.NewNop(), "org", "repo", 42, vl, "")

		if len(added) != 0 {
			t.Errorf("added = %v, want empty", added)
		}
		if len(removed) != 2 {
			t.Errorf("removed = %v, want 2 entries", removed)
		}
	})

	t.Run("skips empty labels in config", func(t *testing.T) {
		partial := models.PRValidationLabels{ValidationFailed: "ai-validation-failed"}
		var added, removed []string
		d := newTestDeps(t)
		d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error { added = append(added, label); return nil }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPRValidationLabel(p, zap.NewNop(), "org", "repo", 42, partial, "ai-validation-failed")

		if len(added) != 1 || added[0] != "ai-validation-failed" {
			t.Errorf("added = %v, want [ai-validation-failed]", added)
		}
		if len(removed) != 0 {
			t.Errorf("removed = %v, want empty (no other labels configured)", removed)
		}
	})

	t.Run("errors are swallowed", func(t *testing.T) {
		d := newTestDeps(t)
		d.git.AddPRLabelFunc = func(_, _ string, _ int, _ string) error { return fmt.Errorf("add failed") }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error { return fmt.Errorf("remove failed") }

		p := d.pipeline(t)
		executor.SetPRValidationLabel(p, zap.NewNop(), "org", "repo", 42, vl, "ai-validation-failed")
	})

	t.Run("removes cost cap label when setting validation label", func(t *testing.T) {
		costCap := models.DefaultCostCapLabel
		vlAll := models.PRValidationLabels{
			ValidationFailed: "ai-validation-failed",
			NonzeroExit:      "ai-nonzero-exit",
			CostCapExceeded:  &costCap,
		}
		var added, removed []string
		d := newTestDeps(t)
		d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error { added = append(added, label); return nil }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.SetPRValidationLabel(p, zap.NewNop(), "org", "repo", 42, vlAll, "ai-validation-failed")

		if len(added) != 1 || added[0] != "ai-validation-failed" {
			t.Errorf("added = %v, want [ai-validation-failed]", added)
		}
		removedSet := make(map[string]bool, len(removed))
		for _, l := range removed {
			removedSet[l] = true
		}
		if !removedSet["ai-nonzero-exit"] {
			t.Error("expected ai-nonzero-exit to be removed")
		}
		if !removedSet[models.DefaultCostCapLabel] {
			t.Errorf("expected %s to be removed", models.DefaultCostCapLabel)
		}
		if len(removed) != 2 {
			t.Errorf("removed = %v, want exactly 2 entries", removed)
		}
	})
}

func TestClearPRValidationLabels(t *testing.T) {
	t.Run("removes all configured labels", func(t *testing.T) {
		vl := models.PRValidationLabels{
			ValidationFailed: "ai-validation-failed",
			NonzeroExit:      "ai-nonzero-exit",
		}
		var removed []string
		d := newTestDeps(t)
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.ClearPRValidationLabels(p, zap.NewNop(), "org", "repo", 42, vl)

		if len(removed) != 2 {
			t.Fatalf("removed = %v, want 2 entries", removed)
		}
		want := map[string]bool{"ai-validation-failed": true, "ai-nonzero-exit": true}
		for _, l := range removed {
			if !want[l] {
				t.Errorf("unexpected removal of %q", l)
			}
		}
	})

	t.Run("skips empty labels", func(t *testing.T) {
		vl := models.PRValidationLabels{ValidationFailed: "ai-validation-failed"}
		var removed []string
		d := newTestDeps(t)
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		p := d.pipeline(t)
		executor.ClearPRValidationLabels(p, zap.NewNop(), "org", "repo", 42, vl)

		if len(removed) != 1 || removed[0] != "ai-validation-failed" {
			t.Errorf("removed = %v, want [ai-validation-failed]", removed)
		}
	})

	t.Run("errors are swallowed", func(t *testing.T) {
		vl := models.PRValidationLabels{
			ValidationFailed: "ai-validation-failed",
			NonzeroExit:      "ai-nonzero-exit",
		}
		d := newTestDeps(t)
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error { return fmt.Errorf("remove failed") }

		p := d.pipeline(t)
		executor.ClearPRValidationLabels(p, zap.NewNop(), "org", "repo", 42, vl)
	})
}

func TestSetPipelineLabel_ErrorsAreSwallowed(t *testing.T) {
	allLabels := models.AllPipelineLabels(
		models.FailureLabels{
			CIFailing:       "ci-fail",
			Rejected:        "rejected",
			Blocked:         "blocked",
			ForkUserMissing: "fork-missing",
		},
		models.LifecycleLabels{
			Queued: "jira-autofix",
			Review: "jira-autofix-review",
			Merged: "jira-autofix-merged",
		},
	)

	t.Run("AddLabel error does not propagate", func(t *testing.T) {
		var addCalled bool
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, _ string) error { addCalled = true; return fmt.Errorf("add failed") }
		d.tracker.RemoveLabelFunc = func(_, _ string) error { return nil }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", allLabels, "blocked")

		if !addCalled {
			t.Error("expected AddLabel to be called")
		}
	})

	t.Run("RemoveLabel error does not propagate", func(t *testing.T) {
		var removeCalled bool
		d := newTestDeps(t)
		d.tracker.AddLabelFunc = func(_, _ string) error { return nil }
		d.tracker.RemoveLabelFunc = func(_, _ string) error { removeCalled = true; return fmt.Errorf("remove failed") }

		p := d.pipeline(t)
		executor.SetPipelineLabel(p, zap.NewNop(), "TEST-1", allLabels, "blocked")

		if !removeCalled {
			t.Error("expected RemoveLabel to be called")
		}
	})

	t.Run("ClearFailureLabels swallows errors", func(t *testing.T) {
		var removeCalled bool
		fl := models.FailureLabels{
			CIFailing:       "ci-fail",
			Rejected:        "rejected",
			Blocked:         "blocked",
			ForkUserMissing: "fork-missing",
		}
		d := newTestDeps(t)
		d.tracker.RemoveLabelFunc = func(_, _ string) error { removeCalled = true; return fmt.Errorf("remove failed") }

		p := d.pipeline(t)
		executor.ClearFailureLabels(p, zap.NewNop(), "TEST-1", fl)

		if !removeCalled {
			t.Error("expected RemoveLabel to be called")
		}
	})
}

func TestApplyCostCapPRLabel(t *testing.T) {
	costCapLabel := models.DefaultCostCapLabel

	makeSettings := func(repos []models.RepoSettings) *models.ProjectSettings {
		return &models.ProjectSettings{
			Repos: repos,
			PRValidationLabels: models.PRValidationLabels{
				ValidationFailed: "ai-validation-failed",
				NonzeroExit:      "ai-nonzero-exit",
				CostCapExceeded:  &costCapLabel,
			},
		}
	}

	t.Run("applies label and removes others on exceeded", func(t *testing.T) {
		var added, removed []string
		d := newTestDeps(t)
		d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
			return &models.PRDetails{Number: 42}, nil
		}
		d.git.AddPRLabelFunc = func(_, _ string, _ int, label string) error { added = append(added, label); return nil }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		settings := makeSettings([]models.RepoSettings{{Owner: "org", Repo: "repo"}})
		p := d.pipeline(t)
		executor.ApplyCostCapPRLabel(p, zap.NewNop(), "TEST-1", settings, true)

		if len(added) != 1 || added[0] != models.DefaultCostCapLabel {
			t.Errorf("added = %v, want [%s]", added, models.DefaultCostCapLabel)
		}
		removedSet := make(map[string]bool, len(removed))
		for _, l := range removed {
			removedSet[l] = true
		}
		if !removedSet["ai-validation-failed"] {
			t.Error("expected ai-validation-failed to be removed")
		}
		if !removedSet["ai-nonzero-exit"] {
			t.Error("expected ai-nonzero-exit to be removed")
		}
	})

	t.Run("removes only cost cap label when not exceeded", func(t *testing.T) {
		var removed []string
		d := newTestDeps(t)
		d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
			return &models.PRDetails{Number: 42}, nil
		}
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, label string) error { removed = append(removed, label); return nil }

		settings := makeSettings([]models.RepoSettings{{Owner: "org", Repo: "repo"}})
		p := d.pipeline(t)
		executor.ApplyCostCapPRLabel(p, zap.NewNop(), "TEST-1", settings, false)

		if len(removed) != 1 || removed[0] != models.DefaultCostCapLabel {
			t.Errorf("removed = %v, want [%s]", removed, models.DefaultCostCapLabel)
		}
	})

	t.Run("no-op when label is disabled", func(t *testing.T) {
		var prLookups int
		empty := ""
		d := newTestDeps(t)
		d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
			prLookups++
			return &models.PRDetails{Number: 42}, nil
		}

		settings := &models.ProjectSettings{
			Repos: []models.RepoSettings{{Owner: "org", Repo: "repo"}},
			PRValidationLabels: models.PRValidationLabels{
				CostCapExceeded: &empty,
			},
		}
		p := d.pipeline(t)
		executor.ApplyCostCapPRLabel(p, zap.NewNop(), "TEST-1", settings, true)

		if prLookups != 0 {
			t.Errorf("expected no PR lookups when label disabled, got %d", prLookups)
		}
	})

	t.Run("skips repos without open PRs", func(t *testing.T) {
		var added []string
		d := newTestDeps(t)
		d.git.GetPRForBranchFunc = func(_, repo, _ string) (*models.PRDetails, error) {
			if repo == "repo-with-pr" {
				return &models.PRDetails{Number: 10}, nil
			}
			return nil, nil
		}
		d.git.AddPRLabelFunc = func(_, repo string, prNum int, label string) error {
			added = append(added, fmt.Sprintf("%s#%d:%s", repo, prNum, label))
			return nil
		}
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error { return nil }

		settings := makeSettings([]models.RepoSettings{
			{Owner: "org", Repo: "repo-no-pr"},
			{Owner: "org", Repo: "repo-with-pr"},
		})
		p := d.pipeline(t)
		executor.ApplyCostCapPRLabel(p, zap.NewNop(), "TEST-1", settings, true)

		if len(added) != 1 {
			t.Fatalf("added = %v, want 1 entry", added)
		}
		want := fmt.Sprintf("repo-with-pr#10:%s", models.DefaultCostCapLabel)
		if added[0] != want {
			t.Errorf("added[0] = %q, want %q", added[0], want)
		}
	})

	t.Run("multi-repo applies to all PRs", func(t *testing.T) {
		var addedRepos []string
		d := newTestDeps(t)
		d.git.GetPRForBranchFunc = func(_, repo, _ string) (*models.PRDetails, error) {
			if repo == "repo1" {
				return &models.PRDetails{Number: 10}, nil
			}
			return &models.PRDetails{Number: 20}, nil
		}
		d.git.AddPRLabelFunc = func(_, repo string, _ int, _ string) error {
			addedRepos = append(addedRepos, repo)
			return nil
		}
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error { return nil }

		settings := makeSettings([]models.RepoSettings{
			{Owner: "org", Repo: "repo1"},
			{Owner: "org", Repo: "repo2"},
		})
		p := d.pipeline(t)
		executor.ApplyCostCapPRLabel(p, zap.NewNop(), "TEST-1", settings, true)

		if len(addedRepos) != 2 {
			t.Fatalf("expected label on 2 repos, got %d", len(addedRepos))
		}
	})

	t.Run("PR lookup errors are swallowed", func(t *testing.T) {
		var labelCalls int
		d := newTestDeps(t)
		d.git.GetPRForBranchFunc = func(_, _, _ string) (*models.PRDetails, error) {
			return nil, fmt.Errorf("API error")
		}
		d.git.AddPRLabelFunc = func(_, _ string, _ int, _ string) error { labelCalls++; return nil }
		d.git.RemovePRLabelFunc = func(_, _ string, _ int, _ string) error { labelCalls++; return nil }

		settings := makeSettings([]models.RepoSettings{{Owner: "org", Repo: "repo"}})
		p := d.pipeline(t)
		executor.ApplyCostCapPRLabel(p, zap.NewNop(), "TEST-1", settings, true)

		if labelCalls != 0 {
			t.Errorf("expected no label calls on lookup error, got %d", labelCalls)
		}
	})
}
