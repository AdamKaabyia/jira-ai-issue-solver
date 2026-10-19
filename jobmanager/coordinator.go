package jobmanager

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Compile-time check that Coordinator implements Manager.
var _ Manager = (*Coordinator)(nil)

// ExecuteFunc runs a job. The [Coordinator] calls this in a new
// goroutine when a concurrency slot is available. The context is
// cancelled when the Coordinator shuts down. Return a result on
// success or an error on failure.
type ExecuteFunc func(ctx context.Context, job *Job) (JobResult, error)

// Config holds construction parameters for [Coordinator].
type Config struct {
	// MaxConcurrent is the maximum number of jobs that can run
	// simultaneously. Must be positive.
	MaxConcurrent int

	// MaxRetries is the maximum number of times a ticket can fail
	// before further submissions are rejected. Zero means no retries
	// (one attempt total). Negative disables the retry limit.
	MaxRetries int

	// CircuitBreakerThreshold is the number of consecutive failures
	// within CircuitBreakerWindow that trips the breaker. Zero
	// disables the circuit breaker.
	CircuitBreakerThreshold int

	// CircuitBreakerWindow is the time window for counting
	// consecutive failures. Failures outside this window are pruned.
	CircuitBreakerWindow time.Duration

	// CircuitBreakerCooldown is how long the circuit breaker stays
	// open before automatically resetting.
	CircuitBreakerCooldown time.Duration

	// CostRecorder optionally tracks AI session costs for budget
	// enforcement. When set, [Coordinator.Submit] returns
	// [ErrBudgetExceeded] if the daily budget has been reached, and
	// completed jobs' costs are recorded automatically. Nil disables
	// cost tracking.
	CostRecorder CostRecorder

	// Clock returns the current time. Defaults to [time.Now] when
	// nil. Exposed for testing.
	Clock func() time.Time
}

// Coordinator implements [Manager] by coordinating job lifecycle with
// deduplication, concurrency limits, retry tracking, and circuit
// breaker protection. Jobs are dispatched to the provided
// [ExecuteFunc] when concurrency slots are available.
type Coordinator struct {
	mu sync.Mutex

	// Job storage and indices.
	jobs       map[string]*Job
	ticketJobs map[string]string // ticket key -> job ID (pending/running)
	queue      []string          // ordered pending job IDs

	// Concurrency control.
	running    int
	maxRunning int

	// Retry tracking: ticket key -> cumulative failure count.
	failureCounts map[string]int
	maxRetries    int

	breaker circuitBreaker
	costs   CostRecorder // nil disables cost tracking

	execute ExecuteFunc
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	stopped bool
	clock   func() time.Time
	logger  *zap.Logger
}

// NewCoordinator creates a Coordinator that dispatches jobs to the
// given execute function. Returns an error if any required parameter
// is invalid.
func NewCoordinator(cfg Config, execute ExecuteFunc, logger *zap.Logger) (*Coordinator, error) {
	if execute == nil {
		return nil, errors.New("execute function must not be nil")
	}
	if logger == nil {
		return nil, errors.New("logger must not be nil")
	}
	if cfg.MaxConcurrent <= 0 {
		return nil, errors.New("max concurrent jobs must be positive")
	}

	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Coordinator{
		jobs:          make(map[string]*Job),
		ticketJobs:    make(map[string]string),
		failureCounts: make(map[string]int),
		maxRunning:    cfg.MaxConcurrent,
		maxRetries:    cfg.MaxRetries,
		breaker: circuitBreaker{
			threshold: cfg.CircuitBreakerThreshold,
			window:    cfg.CircuitBreakerWindow,
			cooldown:  cfg.CircuitBreakerCooldown,
		},
		costs:   cfg.CostRecorder,
		execute: execute,
		ctx:     ctx,
		cancel:  cancel,
		clock:   clock,
		logger:  logger,
	}, nil
}

func (c *Coordinator) Submit(event Event) (*Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return nil, ErrShutdown
	}

	if event.TicketKey == "" {
		return nil, errors.New("event ticket key must not be empty")
	}

	if _, exists := c.ticketJobs[event.TicketKey]; exists {
		return nil, ErrDuplicateJob
	}

	if c.maxRetries >= 0 && c.failureCounts[event.TicketKey] > c.maxRetries {
		return nil, ErrRetriesExhausted
	}

	now := c.clock()
	if c.breaker.isOpen(now) {
		return nil, ErrCircuitOpen
	}

	if c.costs != nil && c.costs.BudgetExceeded() {
		return nil, ErrBudgetExceeded
	}

	var cmdSrc *CommandSource
	if event.CommandSource != nil {
		cloned := *event.CommandSource
		cmdSrc = &cloned
	}

	job := &Job{
		ID:            generateJobID(),
		TicketKey:     event.TicketKey,
		Type:          event.Type,
		Status:        JobStatusPending,
		AttemptNum:    c.failureCounts[event.TicketKey] + 1,
		CreatedAt:     now,
		CleanRetry:    event.CleanRetry,
		CommandSource: cmdSrc,
	}

	c.jobs[job.ID] = job
	c.ticketJobs[event.TicketKey] = job.ID
	c.queue = append(c.queue, job.ID)

	c.logger.Info("Job submitted",
		zap.String("job_id", job.ID),
		zap.String("ticket", event.TicketKey),
		zap.String("type", string(event.Type)),
		zap.Int("attempt", job.AttemptNum))

	c.tryDispatch()

	return c.snapshot(job), nil
}

func (c *Coordinator) Complete(jobID string, result JobResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	job, ok := c.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if job.Status != JobStatusRunning {
		return ErrJobNotRunning
	}

	if c.costs != nil && result.CostUSD > 0 {
		c.costs.Record(result.CostUSD)
	}

	c.completeLocked(job, result)
	c.running--
	c.tryDispatch()
	return nil
}

func (c *Coordinator) Fail(jobID string, err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	job, ok := c.jobs[jobID]
	if !ok {
		return ErrJobNotFound
	}
	if job.Status != JobStatusRunning {
		return ErrJobNotRunning
	}

	c.failLocked(job, err)
	c.running--
	c.tryDispatch()
	return nil
}

func (c *Coordinator) GetJob(jobID string) (*Job, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	job, ok := c.jobs[jobID]
	if !ok {
		return nil, ErrJobNotFound
	}
	return c.snapshot(job), nil
}

func (c *Coordinator) ActiveJobs() []*Job {
	c.mu.Lock()
	defer c.mu.Unlock()

	var active []*Job
	for _, job := range c.jobs {
		if job.Status == JobStatusRunning {
			active = append(active, c.snapshot(job))
		}
	}
	return active
}

// Shutdown stops accepting new jobs, cancels running jobs via context
// cancellation, and waits for all dispatched goroutines to finish.
func (c *Coordinator) Shutdown() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()

	c.cancel()
	c.wg.Wait()
}

// PurgeCompleted removes all terminal (completed or failed) jobs from
// the in-memory store. This prevents unbounded memory growth when the
// bot runs for extended periods. Active (pending or running) jobs are
// not affected. Returns the number of jobs removed.
func (c *Coordinator) PurgeCompleted() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	removed := 0
	for id, job := range c.jobs {
		if job.Status == JobStatusCompleted || job.Status == JobStatusFailed {
			delete(c.jobs, id)
			removed++
		}
	}
	return removed
}

func (c *Coordinator) ResetRetries(ticketKey string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		return ErrShutdown
	}

	if ticketKey == "" {
		return errors.New("ticket key must not be empty")
	}

	delete(c.failureCounts, ticketKey)

	c.logger.Info("Retry count reset",
		zap.String("ticket", ticketKey))

	return nil
}

// --- internal ---

// tryDispatch starts goroutines for pending jobs when concurrency
// slots are available. Must be called with c.mu held.
func (c *Coordinator) tryDispatch() {
	if c.stopped {
		return
	}
	for c.running < c.maxRunning && len(c.queue) > 0 {
		jobID := c.queue[0]
		c.queue = c.queue[1:]

		job := c.jobs[jobID]
		job.Status = JobStatusRunning
		job.StartedAt = c.clock()
		c.running++

		snapshot := c.snapshot(job)

		c.logger.Info("Job dispatched",
			zap.String("job_id", jobID),
			zap.String("ticket", job.TicketKey))

		c.wg.Add(1)
		go c.runJob(jobID, snapshot)
	}
}

// runJob executes a job via the ExecuteFunc and transitions the job
// to Completed or Failed based on the result.
func (c *Coordinator) runJob(jobID string, snapshot *Job) {
	defer c.wg.Done()

	result, err := c.execute(c.ctx, snapshot)

	c.mu.Lock()
	defer c.mu.Unlock()

	job, ok := c.jobs[jobID]
	if !ok || job.Status != JobStatusRunning {
		// Completed/failed externally (e.g., during shutdown).
		return
	}

	// Record cost even on failure — the AI session may have consumed
	// tokens before failing (e.g., timeout, no changes produced).
	if c.costs != nil && result.CostUSD > 0 {
		c.costs.Record(result.CostUSD)
	}

	if err != nil {
		c.failLocked(job, err)
	} else {
		c.completeLocked(job, result)
	}

	c.running--
	c.tryDispatch()
}

func (c *Coordinator) completeLocked(job *Job, result JobResult) {
	now := c.clock()
	job.Status = JobStatusCompleted
	job.CompletedAt = now
	r := result
	job.Result = &r

	delete(c.ticketJobs, job.TicketKey)
	delete(c.failureCounts, job.TicketKey)
	c.breaker.recordSuccess()

	c.logger.Info("Job completed",
		zap.String("job_id", job.ID),
		zap.String("ticket", job.TicketKey),
		zap.Float64("cost_usd", result.CostUSD))
}

func (c *Coordinator) failLocked(job *Job, err error) {
	now := c.clock()
	job.Status = JobStatusFailed
	job.CompletedAt = now
	job.Err = err

	delete(c.ticketJobs, job.TicketKey)
	c.failureCounts[job.TicketKey]++
	c.breaker.recordFailure(now)

	c.logger.Warn("Job failed",
		zap.String("job_id", job.ID),
		zap.String("ticket", job.TicketKey),
		zap.Int("total_failures", c.failureCounts[job.TicketKey]),
		zap.Error(err))
}

// snapshot returns a deep copy of the job safe for use outside the
// lock.
func (c *Coordinator) snapshot(job *Job) *Job {
	s := *job
	if job.Result != nil {
		r := *job.Result
		s.Result = &r
	}
	if job.CommandSource != nil {
		cs := *job.CommandSource
		s.CommandSource = &cs
	}
	return &s
}

// generateJobID returns a random job identifier.
func generateJobID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("job-%x", b)
}

// --- circuit breaker ---

// circuitBreaker tracks consecutive failures and trips when the
// threshold is exceeded within a time window. It automatically resets
// after a cooldown period. A threshold of 0 disables the breaker.
type circuitBreaker struct {
	recentFailures []time.Time
	threshold      int
	window         time.Duration
	cooldown       time.Duration
	openedAt       time.Time
	open           bool
}

// recordFailure records a failure and trips the breaker if the
// threshold is reached within the window.
func (cb *circuitBreaker) recordFailure(now time.Time) {
	if cb.threshold <= 0 {
		return
	}
	cb.recentFailures = append(cb.recentFailures, now)
	cb.pruneOutsideWindow(now)
	if len(cb.recentFailures) >= cb.threshold {
		cb.open = true
		cb.openedAt = now
	}
}

// recordSuccess resets the failure history and closes the breaker.
func (cb *circuitBreaker) recordSuccess() {
	cb.recentFailures = cb.recentFailures[:0]
	cb.open = false
}

// isOpen reports whether the breaker is currently tripped. Auto-resets
// after the cooldown period has elapsed.
func (cb *circuitBreaker) isOpen(now time.Time) bool {
	if cb.threshold <= 0 {
		return false
	}
	if !cb.open {
		return false
	}
	if now.Sub(cb.openedAt) >= cb.cooldown {
		cb.open = false
		cb.recentFailures = cb.recentFailures[:0]
		return false
	}
	return true
}

func (cb *circuitBreaker) pruneOutsideWindow(now time.Time) {
	cutoff := now.Add(-cb.window)
	i := 0
	for i < len(cb.recentFailures) && cb.recentFailures[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		cb.recentFailures = cb.recentFailures[i:]
	}
}
