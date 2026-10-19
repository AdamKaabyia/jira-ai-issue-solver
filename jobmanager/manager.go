// Package jobmanager coordinates job lifecycle for AI-assisted ticket
// processing.
//
// The Manager receives events from scanners, creates jobs, and enforces
// constraints including deduplication (one job per ticket), concurrency
// limits, retry policies, and circuit breaker protection. Jobs are
// dispatched to an [ExecuteFunc] for processing.
//
// Job state is in-memory. Durable state lives in the issue tracker
// (ticket status) and GitHub (PR existence). If the bot crashes, the
// crash recovery process re-queues stuck tickets on startup.
//
// # Retry semantics
//
// The Manager does not retry jobs itself. Scanners re-discover failed
// tickets (whose status reverts to "todo") during subsequent poll
// cycles and resubmit them. The Manager tracks per-ticket failure
// counts across submissions and rejects new submissions once the retry
// limit is exhausted.
//
// Test doubles are provided in the [jobmanagertest] subpackage.
package jobmanager

import (
	"errors"
	"time"
)

// JobType identifies the kind of work a job performs.
type JobType string

const (
	// JobTypeNewTicket processes a newly discovered ticket.
	JobTypeNewTicket JobType = "new_ticket"

	// JobTypeFeedback processes PR review feedback.
	JobTypeFeedback JobType = "feedback"

	// JobTypeMerge merges the target branch into a PR branch to
	// resolve conflicts, optionally using AI for conflict resolution.
	JobTypeMerge JobType = "merge"
)

// JobStatus represents the lifecycle state of a job.
type JobStatus string

const (
	JobStatusPending   JobStatus = "pending"
	JobStatusRunning   JobStatus = "running"
	JobStatusCompleted JobStatus = "completed"
	JobStatusFailed    JobStatus = "failed"
)

// Event represents a work signal from a scanner. The Manager creates
// a [Job] from each accepted event.
type Event struct {
	// Type identifies the kind of work to perform.
	Type JobType

	// TicketKey identifies the ticket this event pertains to
	// (e.g., "PROJ-123"). Used for deduplication and retry tracking.
	TicketKey string

	// CleanRetry signals the pipeline to delete stale remote branches
	// and the local workspace before processing. Set when the retry
	// label triggers resubmission of an exhausted ticket.
	CleanRetry bool

	// CommandSource identifies the PR comment that triggered a bot
	// command. Nil for events not triggered by a bot command.
	CommandSource *CommandSource
}

// CommandSource identifies the GitHub PR comment that triggered a bot
// command, enabling the executor to post a reply and update it on
// completion.
type CommandSource struct {
	Owner     string
	Repo      string
	PRNumber  int
	CommentID int64
}

// JobResult holds the outcome of a completed job.
type JobResult struct {
	// PRURL is the URL of the created pull request.
	PRURL string

	// PRNumber is the number of the created pull request.
	PRNumber int

	// Draft indicates whether the PR was created as a GitHub draft.
	// Only true when the repo config has pr.draft: true; validation
	// failures no longer produce drafts.
	Draft bool

	// CostUSD is the AI session cost reported by the provider.
	CostUSD float64

	// ValidationPassed indicates whether the AI session completed
	// successfully: validation was not explicitly failed and the
	// container exited with code zero.
	ValidationPassed bool

	// MergeCommitSHA is the commit SHA from a merge operation.
	// Only set for merge jobs that produced a commit.
	MergeCommitSHA string
}

// Job represents a unit of work tracked by the Manager. Jobs progress
// through the states: Pending -> Running -> Completed | Failed.
type Job struct {
	// ID is a unique identifier assigned on creation.
	ID string

	// TicketKey identifies the ticket this job is processing.
	TicketKey string

	// Type is the kind of work being performed.
	Type JobType

	// Status is the current lifecycle state.
	Status JobStatus

	// AttemptNum is the attempt number for this ticket, starting at 1.
	// Increments with each submission after a prior failure.
	AttemptNum int

	// CreatedAt is when the job was submitted.
	CreatedAt time.Time

	// StartedAt is when the job transitioned to Running. Zero if not
	// yet started.
	StartedAt time.Time

	// CompletedAt is when the job reached a terminal state. Zero if
	// still active.
	CompletedAt time.Time

	// Result is set when the job completes successfully.
	Result *JobResult

	// Err is set when the job fails.
	Err error

	// CleanRetry signals the pipeline to delete stale remote branches
	// and the local workspace before processing.
	CleanRetry bool

	// CommandSource identifies the PR comment that triggered a bot
	// command. Nil for jobs not triggered by a bot command.
	CommandSource *CommandSource
}

// CostRecorder tracks AI session costs for budget enforcement. The
// Coordinator checks [CostRecorder.BudgetExceeded] on each Submit and
// records costs via [CostRecorder.Record] when a job completes with a
// non-zero CostUSD in its [JobResult].
type CostRecorder interface {
	// Record adds the given amount to the daily cost total.
	Record(amount float64)

	// BudgetExceeded reports whether the daily cost budget has been
	// reached or exceeded.
	BudgetExceeded() bool
}

// Sentinel errors returned by [Manager] methods.
var (
	// ErrDuplicateJob indicates a pending or running job already
	// exists for the ticket.
	ErrDuplicateJob = errors.New("job already active for this ticket")

	// ErrRetriesExhausted indicates the ticket has failed the maximum
	// number of times and will not be retried.
	ErrRetriesExhausted = errors.New("max retries exhausted for this ticket")

	// ErrCircuitOpen indicates the circuit breaker has tripped due to
	// consecutive failures, temporarily blocking new job creation.
	ErrCircuitOpen = errors.New("circuit breaker is open")

	// ErrBudgetExceeded indicates the daily cost budget has been
	// reached and new job creation is temporarily paused.
	ErrBudgetExceeded = errors.New("daily cost budget exceeded")

	// ErrJobNotFound indicates no job exists with the given ID.
	ErrJobNotFound = errors.New("job not found")

	// ErrJobNotRunning indicates the job is not in Running state and
	// cannot be completed or failed.
	ErrJobNotRunning = errors.New("job is not running")

	// ErrShutdown indicates the manager has been shut down and is no
	// longer accepting new jobs.
	ErrShutdown = errors.New("job manager is shut down")
)

// Manager coordinates job lifecycle, enforcing deduplication,
// concurrency limits, retry policies, and circuit breaker protection.
type Manager interface {
	// Submit creates a job from the given event and enqueues it for
	// execution. Returns a snapshot of the created job.
	//
	// Returns [ErrDuplicateJob] if a pending or running job already
	// exists for the ticket. Returns [ErrRetriesExhausted] if the
	// ticket has exceeded its retry limit. Returns [ErrCircuitOpen]
	// if the circuit breaker has tripped. Returns [ErrShutdown] if
	// the manager has been shut down.
	Submit(event Event) (*Job, error)

	// Complete marks a running job as successfully completed.
	// Returns [ErrJobNotFound] or [ErrJobNotRunning] on invalid state.
	//
	// When using [Coordinator], jobs are auto-transitioned based on
	// the [ExecuteFunc] return value. Complete is available for
	// external override scenarios (e.g., crash recovery marking a
	// job as done). If the ExecuteFunc has already transitioned the
	// job, Complete returns [ErrJobNotRunning].
	Complete(jobID string, result JobResult) error

	// Fail marks a running job as failed.
	// Returns [ErrJobNotFound] or [ErrJobNotRunning] on invalid state.
	//
	// When using [Coordinator], jobs are auto-transitioned based on
	// the [ExecuteFunc] return value. Fail is available for external
	// override scenarios (e.g., admin cancellation). If the
	// ExecuteFunc has already transitioned the job, Fail returns
	// [ErrJobNotRunning].
	Fail(jobID string, err error) error

	// GetJob returns a snapshot of the job with the given ID.
	// Returns [ErrJobNotFound] if the ID is unknown.
	GetJob(jobID string) (*Job, error)

	// ActiveJobs returns snapshots of all jobs in Running state.
	ActiveJobs() []*Job

	// ResetRetries clears the failure count for a ticket, allowing
	// it to be resubmitted even after exhausting the retry limit.
	// Returns [ErrShutdown] if the manager has been shut down.
	ResetRetries(ticketKey string) error
}
