package jobmanager_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/jobmanager"
)

// --- NewCoordinator ---

func TestNewCoordinator_RejectsNilExecute(t *testing.T) {
	cfg := jobmanager.Config{MaxConcurrent: 1}
	_, err := jobmanager.NewCoordinator(cfg, nil, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for nil execute")
	}
}

func TestNewCoordinator_RejectsNilLogger(t *testing.T) {
	cfg := jobmanager.Config{MaxConcurrent: 1}
	_, err := jobmanager.NewCoordinator(cfg, noopExecute, nil)
	if err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestNewCoordinator_RejectsZeroMaxConcurrent(t *testing.T) {
	cfg := jobmanager.Config{MaxConcurrent: 0}
	_, err := jobmanager.NewCoordinator(cfg, noopExecute, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for zero max concurrent")
	}
}

func TestNewCoordinator_RejectsNegativeMaxConcurrent(t *testing.T) {
	cfg := jobmanager.Config{MaxConcurrent: -1}
	_, err := jobmanager.NewCoordinator(cfg, noopExecute, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for negative max concurrent")
	}
}

func TestNewCoordinator_ValidConfig(t *testing.T) {
	cfg := jobmanager.Config{MaxConcurrent: 5, MaxRetries: 3}
	coord, err := jobmanager.NewCoordinator(cfg, noopExecute, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if coord == nil {
		t.Fatal("expected non-nil coordinator")
	}
}

// --- Submit ---

func TestSubmit_CreatesJobWithCorrectState(t *testing.T) {
	clock := newTestClock()
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
		Clock:         clock.Now,
	}, blockForever)
	defer coord.Shutdown()

	event := jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	}

	job, err := coord.Submit(event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if job.ID == "" {
		t.Error("expected non-empty job ID")
	}
	if job.TicketKey != "PROJ-1" {
		t.Errorf("TicketKey = %q, want PROJ-1", job.TicketKey)
	}
	if job.Type != jobmanager.JobTypeNewTicket {
		t.Errorf("Type = %q, want %q", job.Type, jobmanager.JobTypeNewTicket)
	}
	if job.AttemptNum != 1 {
		t.Errorf("AttemptNum = %d, want 1", job.AttemptNum)
	}
	if job.CreatedAt != clock.now {
		t.Errorf("CreatedAt = %v, want %v", job.CreatedAt, clock.now)
	}
}

func TestSubmit_CleanRetryPropagated(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:       jobmanager.JobTypeNewTicket,
		TicketKey:  "PROJ-1",
		CleanRetry: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !job.CleanRetry {
		t.Error("expected CleanRetry to be true on job")
	}
}

func TestSubmit_CleanRetryDefaultFalse(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.CleanRetry {
		t.Error("expected CleanRetry to be false by default")
	}
}

func TestSubmit_CommandSourcePropagated(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	src := &jobmanager.CommandSource{
		Owner:     "org",
		Repo:      "repo",
		PRNumber:  42,
		CommentID: 99,
	}
	job, err := coord.Submit(jobmanager.Event{
		Type:          jobmanager.JobTypeMerge,
		TicketKey:     "PROJ-1",
		CommandSource: src,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.CommandSource == nil {
		t.Fatal("expected CommandSource to be propagated")
	}
	if job.CommandSource.CommentID != 99 {
		t.Errorf("got CommentID %d, want 99", job.CommandSource.CommentID)
	}
}

func TestSubmit_CommandSourceNilByDefault(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeMerge,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.CommandSource != nil {
		t.Error("expected CommandSource to be nil by default")
	}
}

func TestSubmit_RejectsEmptyTicketKey(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	_, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "",
	})
	if err == nil {
		t.Fatal("expected error for empty ticket key")
	}
}

func TestSubmit_FeedbackJobType(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeFeedback,
		TicketKey: "PROJ-2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if job.Type != jobmanager.JobTypeFeedback {
		t.Errorf("Type = %q, want %q", job.Type, jobmanager.JobTypeFeedback)
	}
}

// --- Deduplication ---

func TestDedup_RejectsSecondSubmitWhileActive(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 2,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	_, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	_, err = coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrDuplicateJob) {
		t.Fatalf("second submit: want ErrDuplicateJob, got %v", err)
	}
}

func TestDedup_AllowsResubmitAfterCompletion(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 2,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	waitForTerminal(t, coord, job.ID)

	_, err = coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("resubmit after completion: %v", err)
	}
}

func TestDedup_AllowsResubmitAfterFailure(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 2,
		MaxRetries:    -1,
	}, failExecute)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	waitForTerminal(t, coord, job.ID)

	_, err = coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("resubmit after failure: %v", err)
	}
}

func TestDedup_RejectsPendingDuplicate(t *testing.T) {
	// Fill all slots so new jobs remain pending.
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, blockForever)
	defer coord.Shutdown()

	// Fill the slot.
	_, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "BLOCKER",
	})
	if err != nil {
		t.Fatalf("blocker submit: %v", err)
	}

	// This job will be pending (no slot available).
	_, err = coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("first pending submit: %v", err)
	}

	// Duplicate of the pending job.
	_, err = coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrDuplicateJob) {
		t.Fatalf("duplicate of pending: want ErrDuplicateJob, got %v", err)
	}
}

// --- Concurrency ---

func TestConcurrency_RespectsLimit(t *testing.T) {
	started := make(chan string, 10)
	block := make(chan struct{})

	execute := func(_ context.Context, job *jobmanager.Job) (jobmanager.JobResult, error) {
		started <- job.TicketKey
		<-block
		return jobmanager.JobResult{}, nil
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 2,
		MaxRetries:    -1,
	}, execute)
	defer coord.Shutdown()

	// Submit 3 jobs.
	for _, key := range []string{"A", "B", "C"} {
		if _, err := coord.Submit(jobmanager.Event{
			Type:      jobmanager.JobTypeNewTicket,
			TicketKey: key,
		}); err != nil {
			t.Fatalf("submit %s: %v", key, err)
		}
	}

	// Wait for 2 to start.
	<-started
	<-started

	// C should NOT have started.
	select {
	case key := <-started:
		t.Fatalf("expected only 2 concurrent jobs, but %s also started", key)
	case <-time.After(100 * time.Millisecond):
		// Expected: C is queued.
	}

	active := coord.ActiveJobs()
	if len(active) != 2 {
		t.Errorf("ActiveJobs() returned %d jobs, want 2", len(active))
	}

	// Release all. C should start.
	close(block)

	select {
	case <-started:
		// Good, C started.
	case <-time.After(time.Second):
		t.Fatal("expected C to start after slot freed")
	}
}

// --- Retry ---

func TestRetry_SucceedsWithinLimit(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    2,
	}, failExecute)
	defer coord.Shutdown()

	// Attempt 1: fails.
	job1, _ := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	waitForTerminal(t, coord, job1.ID)

	// Attempt 2 (retry 1): still within limit.
	job2, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("retry 1: %v", err)
	}
	if job2.AttemptNum != 2 {
		t.Errorf("AttemptNum = %d, want 2", job2.AttemptNum)
	}
	waitForTerminal(t, coord, job2.ID)

	// Attempt 3 (retry 2): still within limit.
	job3, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("retry 2: %v", err)
	}
	if job3.AttemptNum != 3 {
		t.Errorf("AttemptNum = %d, want 3", job3.AttemptNum)
	}
	waitForTerminal(t, coord, job3.ID)

	// Attempt 4: retries exhausted (3 failures > maxRetries=2).
	_, err = coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrRetriesExhausted) {
		t.Fatalf("after exhaustion: want ErrRetriesExhausted, got %v", err)
	}
}

func TestRetry_SuccessResetsCount(t *testing.T) {
	callCount := 0
	var mu sync.Mutex

	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()

		if n <= 2 {
			return jobmanager.JobResult{}, errors.New("fail")
		}
		return jobmanager.JobResult{}, nil
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    3,
	}, execute)
	defer coord.Shutdown()

	// Fail twice.
	for i := 0; i < 2; i++ {
		job, _ := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
		})
		waitForTerminal(t, coord, job.ID)
	}

	// Succeed on third attempt.
	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	waitForTerminal(t, coord, job.ID)

	got, _ := coord.GetJob(job.ID)
	if got.Status != jobmanager.JobStatusCompleted {
		t.Fatalf("Status = %q, want completed", got.Status)
	}

	// Retry count should be reset. AttemptNum starts fresh at 1.
	// (callCount is irrelevant here; we only verify AttemptNum reset.)

	job2, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("submit after success: %v", err)
	}
	if job2.AttemptNum != 1 {
		t.Errorf("AttemptNum after reset = %d, want 1", job2.AttemptNum)
	}
}

func TestRetry_ZeroMaxRetriesMeansNoRetries(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    0,
	}, failExecute)
	defer coord.Shutdown()

	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	waitForTerminal(t, coord, job.ID)

	// Even one failure exhausts retries when maxRetries=0.
	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrRetriesExhausted) {
		t.Fatalf("want ErrRetriesExhausted, got %v", err)
	}
}

func TestRetry_NegativeMaxRetriesDisablesLimit(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, failExecute)
	defer coord.Shutdown()

	// Fail many times; should never be rejected.
	for i := 0; i < 10; i++ {
		job, err := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		waitForTerminal(t, coord, job.ID)
	}
}

// --- ResetRetries ---

func TestResetRetries_AllowsResubmission(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    0,
	}, failExecute)
	defer coord.Shutdown()

	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	waitForTerminal(t, coord, job.ID)

	// Retries exhausted.
	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrRetriesExhausted) {
		t.Fatalf("want ErrRetriesExhausted, got %v", err)
	}

	// Reset allows resubmission.
	if err := coord.ResetRetries("PROJ-1"); err != nil {
		t.Fatalf("ResetRetries: %v", err)
	}

	job, err = coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatalf("submit after reset: %v", err)
	}
	if job.AttemptNum != 1 {
		t.Errorf("attempt = %d, want 1 after reset", job.AttemptNum)
	}
}

func TestResetRetries_RejectsEmptyKey(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    3,
	}, failExecute)
	defer coord.Shutdown()

	if err := coord.ResetRetries(""); err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestResetRetries_RejectsAfterShutdown(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    3,
	}, failExecute)
	coord.Shutdown()

	if err := coord.ResetRetries("PROJ-1"); !errors.Is(err, jobmanager.ErrShutdown) {
		t.Fatalf("want ErrShutdown, got %v", err)
	}
}

func TestResetRetries_NoopForUnknownTicket(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    3,
	}, failExecute)
	defer coord.Shutdown()

	if err := coord.ResetRetries("UNKNOWN-1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Complete / Fail ---

func TestComplete_UpdatesJobState(t *testing.T) {
	clock := newTestClock()
	block := make(chan struct{})
	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		<-block
		return jobmanager.JobResult{}, nil
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
		Clock:         clock.Now,
	}, execute)
	defer coord.Shutdown()

	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})

	clock.Advance(5 * time.Second)
	close(block)

	waitForTerminal(t, coord, job.ID)
	got, _ := coord.GetJob(job.ID)

	if got.Status != jobmanager.JobStatusCompleted {
		t.Errorf("Status = %q, want completed", got.Status)
	}
	if got.Result == nil {
		t.Error("expected non-nil Result")
	}
	if got.Err != nil {
		t.Errorf("expected nil Err, got %v", got.Err)
	}
	if got.CompletedAt.IsZero() {
		t.Error("expected non-zero CompletedAt")
	}
}

func TestFail_UpdatesJobState(t *testing.T) {
	clock := newTestClock()
	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		return jobmanager.JobResult{}, errors.New("something broke")
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
		Clock:         clock.Now,
	}, execute)
	defer coord.Shutdown()

	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})

	waitForTerminal(t, coord, job.ID)
	got, _ := coord.GetJob(job.ID)

	if got.Status != jobmanager.JobStatusFailed {
		t.Errorf("Status = %q, want failed", got.Status)
	}
	if got.Err == nil || got.Err.Error() != "something broke" {
		t.Errorf("Err = %v, want 'something broke'", got.Err)
	}
	if got.CompletedAt.IsZero() {
		t.Error("expected non-zero CompletedAt")
	}
}

func TestComplete_RejectsUnknownJob(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	err := coord.Complete("nonexistent", jobmanager.JobResult{})
	if !errors.Is(err, jobmanager.ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestFail_RejectsUnknownJob(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	err := coord.Fail("nonexistent", errors.New("x"))
	if !errors.Is(err, jobmanager.ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestComplete_RejectsNonRunningJob(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	waitForTerminal(t, coord, job.ID)

	err := coord.Complete(job.ID, jobmanager.JobResult{})
	if !errors.Is(err, jobmanager.ErrJobNotRunning) {
		t.Fatalf("want ErrJobNotRunning, got %v", err)
	}
}

// --- ActiveJobs ---

func TestActiveJobs_ReturnsOnlyRunningJobs(t *testing.T) {
	started := make(chan struct{}, 2)
	block := make(chan struct{})
	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		started <- struct{}{}
		<-block
		return jobmanager.JobResult{}, nil
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 5,
		MaxRetries:    -1,
	}, execute)
	defer coord.Shutdown()

	jobA, _ := coord.Submit(jobmanager.Event{Type: jobmanager.JobTypeNewTicket, TicketKey: "A"})
	jobB, _ := coord.Submit(jobmanager.Event{Type: jobmanager.JobTypeNewTicket, TicketKey: "B"})

	if jobA == nil || jobB == nil {
		t.Fatal("expected non-nil jobs from Submit")
	}

	// Wait for both goroutines to be running.
	<-started
	<-started

	active := coord.ActiveJobs()
	if len(active) != 2 {
		t.Errorf("ActiveJobs() = %d jobs, want 2", len(active))
	}

	close(block)

	// After completion, no active jobs.
	waitForTerminal(t, coord, active[0].ID)
	waitForTerminal(t, coord, active[1].ID)

	active = coord.ActiveJobs()
	if len(active) != 0 {
		t.Errorf("ActiveJobs() after completion = %d jobs, want 0", len(active))
	}
}

// --- GetJob ---

func TestGetJob_ReturnsSnapshot(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})

	got, err := coord.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.TicketKey != "PROJ-1" {
		t.Errorf("TicketKey = %q, want PROJ-1", got.TicketKey)
	}
}

func TestGetJob_RejectsUnknown(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)
	defer coord.Shutdown()

	_, err := coord.GetJob("nonexistent")
	if !errors.Is(err, jobmanager.ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

// --- Shutdown ---

func TestShutdown_RejectsNewSubmissions(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, noopExecute)

	coord.Shutdown()

	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrShutdown) {
		t.Fatalf("want ErrShutdown, got %v", err)
	}
}

func TestShutdown_CancelsRunningJobs(t *testing.T) {
	ctxCancelled := make(chan struct{})
	execute := func(ctx context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		<-ctx.Done()
		close(ctxCancelled)
		return jobmanager.JobResult{}, ctx.Err()
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, execute)

	if _, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "PROJ-1",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The goroutine is already launched by tryDispatch within Submit.
	// Shutdown should cancel the context and wait.
	done := make(chan struct{})
	go func() {
		coord.Shutdown()
		close(done)
	}()

	select {
	case <-done:
		// Good.
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown did not return within timeout")
	}

	select {
	case <-ctxCancelled:
		// Good, context was cancelled.
	default:
		t.Fatal("expected context to be cancelled")
	}
}

// --- Circuit breaker ---

func TestCircuitBreaker_TripsAfterConsecutiveFailures(t *testing.T) {
	clock := newTestClock()
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent:           1,
		MaxRetries:              -1,
		CircuitBreakerThreshold: 3,
		CircuitBreakerWindow:    10 * time.Minute,
		CircuitBreakerCooldown:  5 * time.Minute,
		Clock:                   clock.Now,
	}, failExecute)
	defer coord.Shutdown()

	// Fail 3 times (using different tickets to avoid dedup).
	for i := 0; i < 3; i++ {
		key := ticketKey(i)
		job, err := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: key,
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		waitForTerminal(t, coord, job.ID)
		clock.Advance(time.Second)
	}

	// Fourth submission should be rejected.
	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "NEW",
	})
	if !errors.Is(err, jobmanager.ErrCircuitOpen) {
		t.Fatalf("want ErrCircuitOpen, got %v", err)
	}
}

func TestCircuitBreaker_ResetsAfterCooldown(t *testing.T) {
	clock := newTestClock()
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent:           1,
		MaxRetries:              -1,
		CircuitBreakerThreshold: 2,
		CircuitBreakerWindow:    10 * time.Minute,
		CircuitBreakerCooldown:  5 * time.Minute,
		Clock:                   clock.Now,
	}, failExecute)
	defer coord.Shutdown()

	// Trip the breaker.
	for i := 0; i < 2; i++ {
		job, _ := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: ticketKey(i),
		})
		waitForTerminal(t, coord, job.ID)
		clock.Advance(time.Second)
	}

	// Breaker is open.
	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "BLOCKED",
	})
	if !errors.Is(err, jobmanager.ErrCircuitOpen) {
		t.Fatalf("want ErrCircuitOpen, got %v", err)
	}

	// Advance past cooldown.
	clock.Advance(5 * time.Minute)

	// Should be accepted now.
	_, err = coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "UNBLOCKED",
	})
	if err != nil {
		t.Fatalf("submit after cooldown: %v", err)
	}
}

func TestCircuitBreaker_DoesNotTripOnIsolatedFailures(t *testing.T) {
	clock := newTestClock()
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent:           1,
		MaxRetries:              -1,
		CircuitBreakerThreshold: 3,
		CircuitBreakerWindow:    10 * time.Minute,
		CircuitBreakerCooldown:  5 * time.Minute,
		Clock:                   clock.Now,
	}, failExecute)
	defer coord.Shutdown()

	// Failures spread over time (each outside the window of the first).
	for i := 0; i < 5; i++ {
		job, err := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: ticketKey(i),
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		waitForTerminal(t, coord, job.ID)
		clock.Advance(11 * time.Minute) // Outside the 10-minute window.
	}

	// Should still be able to submit (breaker never tripped).
	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "STILL-OK",
	})
	if err != nil {
		t.Fatalf("submit after isolated failures: %v", err)
	}
}

func TestCircuitBreaker_SuccessResetsBreakerState(t *testing.T) {
	clock := newTestClock()
	callCount := 0
	var mu sync.Mutex

	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()

		// Fail first 2, succeed on 3rd, then fail again.
		if n == 3 {
			return jobmanager.JobResult{}, nil
		}
		return jobmanager.JobResult{}, errors.New("fail")
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent:           1,
		MaxRetries:              -1,
		CircuitBreakerThreshold: 3,
		CircuitBreakerWindow:    10 * time.Minute,
		CircuitBreakerCooldown:  5 * time.Minute,
		Clock:                   clock.Now,
	}, execute)
	defer coord.Shutdown()

	// Fail twice.
	for i := 0; i < 2; i++ {
		job, _ := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: ticketKey(i),
		})
		waitForTerminal(t, coord, job.ID)
		clock.Advance(time.Second)
	}

	// Succeed once (resets breaker).
	job, _ := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "SUCCESS",
	})
	waitForTerminal(t, coord, job.ID)
	clock.Advance(time.Second)

	// Fail twice more. Should NOT trip (counter was reset by success).
	for i := 10; i < 12; i++ {
		job, _ := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: ticketKey(i),
		})
		waitForTerminal(t, coord, job.ID)
		clock.Advance(time.Second)
	}

	// Breaker should not be open (only 2 consecutive failures, threshold=3).
	_, err := coord.Submit(jobmanager.Event{
		Type: jobmanager.JobTypeNewTicket, TicketKey: "SHOULD-WORK",
	})
	if err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

func TestCircuitBreaker_DisabledWhenThresholdZero(t *testing.T) {
	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent:           1,
		MaxRetries:              -1,
		CircuitBreakerThreshold: 0,
	}, failExecute)
	defer coord.Shutdown()

	// Many failures should never trip the breaker.
	for i := 0; i < 20; i++ {
		job, err := coord.Submit(jobmanager.Event{
			Type: jobmanager.JobTypeNewTicket, TicketKey: ticketKey(i),
		})
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		waitForTerminal(t, coord, job.ID)
	}
}

// --- Dispatch: queued jobs start when slots free ---

func TestDispatch_QueuedJobStartsWhenSlotFrees(t *testing.T) {
	started := make(chan string, 10)
	blocks := make(map[string]chan struct{})
	var blocksMu sync.Mutex

	getBlock := func(key string) chan struct{} {
		blocksMu.Lock()
		defer blocksMu.Unlock()
		ch, ok := blocks[key]
		if !ok {
			ch = make(chan struct{})
			blocks[key] = ch
		}
		return ch
	}

	execute := func(_ context.Context, job *jobmanager.Job) (jobmanager.JobResult, error) {
		started <- job.TicketKey
		<-getBlock(job.TicketKey)
		return jobmanager.JobResult{}, nil
	}

	// Pre-create block channels.
	for _, key := range []string{"A", "B", "C"} {
		getBlock(key)
	}

	coord := mustCoordinator(t, jobmanager.Config{
		MaxConcurrent: 1,
		MaxRetries:    -1,
	}, execute)
	defer coord.Shutdown()

	if _, err := coord.Submit(jobmanager.Event{Type: jobmanager.JobTypeNewTicket, TicketKey: "A"}); err != nil {
		t.Fatalf("submit A: %v", err)
	}
	if _, err := coord.Submit(jobmanager.Event{Type: jobmanager.JobTypeNewTicket, TicketKey: "B"}); err != nil {
		t.Fatalf("submit B: %v", err)
	}

	// A starts.
	select {
	case key := <-started:
		if key != "A" {
			t.Fatalf("expected A to start first, got %s", key)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for A")
	}

	// B should not have started.
	select {
	case key := <-started:
		t.Fatalf("B should not start while A is running, got %s", key)
	case <-time.After(50 * time.Millisecond):
	}

	// Complete A.
	close(getBlock("A"))

	// B should start.
	select {
	case key := <-started:
		if key != "B" {
			t.Fatalf("expected B to start, got %s", key)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for B")
	}

	close(getBlock("B"))
}

// --- Cost recording ---

type costStub struct {
	mu       sync.Mutex
	total    float64
	exceeded bool
}

func (c *costStub) Record(amount float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += amount
}

func (c *costStub) BudgetExceeded() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exceeded
}

func (c *costStub) getTotal() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func TestCoordinator_SuccessfulJobRecordsCostOnce(t *testing.T) {
	costs := &costStub{}
	cfg := jobmanager.Config{
		MaxConcurrent: 1,
		CostRecorder:  costs,
	}
	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		return jobmanager.JobResult{CostUSD: 1.50}, nil
	}
	coord := mustCoordinator(t, cfg, execute)

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, coord, job.ID)

	if got := costs.getTotal(); got != 1.50 {
		t.Errorf("total cost = %f, want 1.50 (cost should be recorded exactly once)", got)
	}
}

func TestCoordinator_ExternalCompleteRecordsCost(t *testing.T) {
	costs := &costStub{}
	cfg := jobmanager.Config{
		MaxConcurrent: 1,
		CostRecorder:  costs,
	}
	coord := mustCoordinator(t, cfg, blockForever)
	defer coord.Shutdown()

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for job to be running.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, _ := coord.GetJob(job.ID)
		if j.Status == jobmanager.JobStatusRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	err = coord.Complete(job.ID, jobmanager.JobResult{CostUSD: 2.00})
	if err != nil {
		t.Fatal(err)
	}

	if got := costs.getTotal(); got != 2.00 {
		t.Errorf("total cost = %f, want 2.00", got)
	}
}

func TestCoordinator_BudgetExceededRejectsSubmit(t *testing.T) {
	costs := &costStub{exceeded: true}
	cfg := jobmanager.Config{
		MaxConcurrent: 1,
		CostRecorder:  costs,
	}
	coord := mustCoordinator(t, cfg, noopExecute)

	_, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if !errors.Is(err, jobmanager.ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got %v", err)
	}
}

func TestCoordinator_ZeroCostNotRecorded(t *testing.T) {
	costs := &costStub{}
	cfg := jobmanager.Config{
		MaxConcurrent: 1,
		CostRecorder:  costs,
	}
	execute := func(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
		return jobmanager.JobResult{CostUSD: 0}, nil
	}
	coord := mustCoordinator(t, cfg, execute)

	job, err := coord.Submit(jobmanager.Event{
		Type:      jobmanager.JobTypeNewTicket,
		TicketKey: "PROJ-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, coord, job.ID)

	if got := costs.getTotal(); got != 0 {
		t.Errorf("total cost = %f, want 0 (zero cost should not be recorded)", got)
	}
}

// --- helpers ---

func mustCoordinator(t *testing.T, cfg jobmanager.Config, execute jobmanager.ExecuteFunc) *jobmanager.Coordinator {
	t.Helper()
	coord, err := jobmanager.NewCoordinator(cfg, execute, zap.NewNop())
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	return coord
}

// waitForTerminal polls until the job reaches a terminal state.
func waitForTerminal(t *testing.T, coord *jobmanager.Coordinator, jobID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job, err := coord.GetJob(jobID)
		if err != nil {
			t.Fatalf("GetJob(%s): %v", jobID, err)
		}
		if job.Status == jobmanager.JobStatusCompleted || job.Status == jobmanager.JobStatusFailed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach terminal state within timeout", jobID)
}

func ticketKey(i int) string {
	return fmt.Sprintf("TICKET-%d", i)
}

// noopExecute completes immediately with success.
func noopExecute(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
	return jobmanager.JobResult{}, nil
}

// failExecute fails immediately.
func failExecute(_ context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
	return jobmanager.JobResult{}, errors.New("execution failed")
}

// blockForever blocks until the context is cancelled.
func blockForever(ctx context.Context, _ *jobmanager.Job) (jobmanager.JobResult, error) {
	<-ctx.Done()
	return jobmanager.JobResult{}, ctx.Err()
}

// testClock provides a controllable clock for testing.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
