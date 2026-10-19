package scanner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/commentfilter"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
)

// Compile-time check that MergeScanner implements Scanner.
var _ Scanner = (*MergeScanner)(nil)

// MergeScannerConfig holds configuration for [MergeScanner].
type MergeScannerConfig struct {
	// Criteria defines the search query for "in review" tickets.
	Criteria models.SearchCriteria

	// PollInterval is the time between scan cycles.
	PollInterval time.Duration

	// BotUsername is the bot's GitHub username, used for branch
	// name construction and filtering bot comments from activity
	// detection.
	BotUsername string

	// IdleDays is the number of days without human PR comment
	// activity before a PR is considered idle. Idle unmergeable
	// PRs are labeled instead of merged. Zero disables idle
	// detection.
	IdleDays int

	// IdleLabel is the GitHub label applied to idle unmergeable
	// PRs. Removing the label re-activates auto-merging.
	IdleLabel string

	// IgnoredUsernames lists users whose comments are not
	// considered human activity for idle detection.
	IgnoredUsernames []string

	// KnownBotUsernames lists other bots whose comments are not
	// considered human activity for idle detection.
	KnownBotUsernames []string

	// SkipPRLabel is the GitHub label that tells the bot to skip
	// a PR entirely. Empty disables the check.
	SkipPRLabel string
}

// MergeScanner polls for tickets in "in review" status and checks
// whether their PRs are mergeable with the target branch. When a PR
// has merge conflicts, the scanner emits [jobmanager.JobTypeMerge]
// events. PRs without recent human activity are labeled as idle
// instead of merged.
//
// When configured with [WithMergeCommands], the scanner also detects
// @botname sync commands in PR comments and submits merge events
// for them.
type MergeScanner struct {
	searcher             IssueSearcher
	submitter            JobSubmitter
	prs                  PRFetcher
	repos                RepoLocator
	mergeCheck           MergeabilityChecker
	labeler              PRLabeler
	mergeCommandsEnabled bool
	cfg                  MergeScannerConfig
	logger               *zap.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// MergeScannerOption configures optional behavior on a
// [MergeScanner]. Pass to [NewMergeScanner].
type MergeScannerOption func(*MergeScanner)

// WithMergeCommands enables detection of @botname sync commands in
// PR comments. When a user posts @botname sync on a PR, the scanner
// submits a merge event. The executor posts a reply to acknowledge
// the command and edits it with the outcome.
func WithMergeCommands() MergeScannerOption {
	return func(s *MergeScanner) {
		s.mergeCommandsEnabled = true
	}
}

// NewMergeScanner creates a MergeScanner with the given dependencies.
// Returns an error if any required parameter is invalid.
func NewMergeScanner(
	searcher IssueSearcher,
	submitter JobSubmitter,
	prs PRFetcher,
	repos RepoLocator,
	mergeCheck MergeabilityChecker,
	labeler PRLabeler,
	cfg MergeScannerConfig,
	logger *zap.Logger,
	opts ...MergeScannerOption,
) (*MergeScanner, error) {
	if searcher == nil {
		return nil, errors.New("issue searcher must not be nil")
	}
	if submitter == nil {
		return nil, errors.New("job submitter must not be nil")
	}
	if prs == nil {
		return nil, errors.New("PR fetcher must not be nil")
	}
	if repos == nil {
		return nil, errors.New("repo locator must not be nil")
	}
	if mergeCheck == nil {
		return nil, errors.New("mergeability checker must not be nil")
	}
	if labeler == nil {
		return nil, errors.New("PR labeler must not be nil")
	}
	if cfg.PollInterval <= 0 {
		return nil, errors.New("poll interval must be positive")
	}
	if cfg.BotUsername == "" {
		return nil, errors.New("bot username must not be empty")
	}
	if logger == nil {
		return nil, errors.New("logger must not be nil")
	}

	s := &MergeScanner{
		searcher:   searcher,
		submitter:  submitter,
		prs:        prs,
		repos:      repos,
		mergeCheck: mergeCheck,
		labeler:    labeler,
		cfg:        cfg,
		logger:     logger,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Start begins polling in a background goroutine.
func (s *MergeScanner) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		return errors.New("scanner already running")
	}

	ctx, s.cancel = context.WithCancel(ctx)
	s.done = make(chan struct{})
	go s.run(ctx)
	return nil
}

// Stop cancels polling and blocks until the goroutine exits.
func (s *MergeScanner) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *MergeScanner) run(ctx context.Context) {
	defer close(s.done)

	s.scan(ctx)

	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scan(ctx)
		}
	}
}

func (s *MergeScanner) scan(ctx context.Context) {
	items, err := s.searcher.SearchWorkItems(s.cfg.Criteria)
	if err != nil {
		s.logger.Error("Failed to search for in-review tickets", zap.Error(err))
		return
	}

	if len(items) == 0 {
		s.logger.Debug("No in-review tickets found")
		return
	}

	s.logger.Debug("Checking mergeability", zap.Int("tickets", len(items)))

	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		if s.checkAndSubmit(item) {
			return
		}
	}
}

// checkAndSubmit checks a ticket's PRs for merge conflicts and
// submits a merge event if any PR is unmergeable. When merge commands
// are enabled, also checks for @botname sync commands in PR comments.
// Returns true if the scan cycle should stop (circuit breaker open or
// shutdown).
func (s *MergeScanner) checkAndSubmit(item models.WorkItem) bool {
	logger := s.logger.With(zap.String("ticket", item.Key))

	repos, err := s.repos.LocateRepos(item)
	if err != nil {
		logger.Warn("Failed to locate repos, skipping", zap.Error(err))
		return false
	}

	branchName := fmt.Sprintf("%s/%s", s.cfg.BotUsername, item.Key)
	heads := s.repos.ForkOwnerHeads(item, branchName)

	// Check for merge conflicts (existing behavior).
	shouldSubmit := false
	for _, head := range heads {
		if s.hasUnmergeablePR(logger, repos, head) {
			shouldSubmit = true
			break
		}
	}

	event := jobmanager.Event{
		Type:      jobmanager.JobTypeMerge,
		TicketKey: item.Key,
	}

	// If no conflicts, check for sync commands.
	if !shouldSubmit && s.mergeCommandsEnabled {
		for _, head := range heads {
			if src := s.findMergeCommand(logger, repos, head); src != nil {
				event.CommandSource = src
				shouldSubmit = true
				break
			}
		}
	}

	if !shouldSubmit {
		return false
	}

	return s.submitMergeEvent(logger, event)
}

// submitMergeEvent submits a merge event and handles errors. Returns
// true if the scan cycle should stop.
func (s *MergeScanner) submitMergeEvent(logger *zap.Logger, event jobmanager.Event) bool {
	_, err := s.submitter.Submit(event)
	if err == nil {
		logger.Info("Submitted merge event")
		return false
	}

	switch {
	case errors.Is(err, jobmanager.ErrDuplicateJob):
		logger.Debug("Skipping duplicate merge")
	case errors.Is(err, jobmanager.ErrRetriesExhausted):
		logger.Debug("Skipping exhausted merge ticket")
	case errors.Is(err, jobmanager.ErrCircuitOpen):
		logger.Warn("Circuit breaker open, stopping scan cycle")
		return true
	case errors.Is(err, jobmanager.ErrBudgetExceeded):
		logger.Warn("Daily budget exceeded, stopping scan cycle")
		return true
	case errors.Is(err, jobmanager.ErrShutdown):
		logger.Info("Job manager shut down, stopping scan cycle")
		return true
	default:
		logger.Error("Failed to submit merge event", zap.Error(err))
	}

	return false
}

// findMergeCommand checks all repos for PRs containing an unaddressed
// @botname sync command in conversation comments. Uses the same
// addressed-marker pattern as the feedback pipeline: the executor
// posts a reply with <!-- addressed: ID -->, and BotRepliedTo detects
// it on subsequent scans. Returns nil if no unaddressed command exists.
func (s *MergeScanner) findMergeCommand(
	logger *zap.Logger,
	repos []models.RepoCoord,
	head string,
) *jobmanager.CommandSource {
	normBot := commentfilter.NormalizeUsername(s.cfg.BotUsername)
	mergeRe := commentfilter.BotCommandRe(s.cfg.BotUsername, "sync")

	for _, r := range repos {
		pr, err := s.prs.GetPRForBranch(r.Owner, r.Repo, head)
		if err != nil {
			logger.Warn("Error looking up PR for sync command",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.Error(err))
			continue
		}
		if pr == nil {
			continue
		}

		if s.hasSkipLabel(logger, r, pr) {
			continue
		}

		comments, err := s.prs.GetPRComments(r.Owner, r.Repo, pr.Number, time.Time{})
		if err != nil {
			logger.Warn("Failed to fetch PR comments for sync command check",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.Error(err))
			continue
		}

		// Filter to conversation comments only — sync commands on
		// file diffs (review comments) are not supported, and the
		// addressed-marker reply is always a conversation comment.
		convComments := make([]models.PRComment, 0, len(comments))
		for _, c := range comments {
			if !c.IsReviewComment {
				convComments = append(convComments, c)
			}
		}

		addressed := commentfilter.BotRepliedTo(convComments, normBot)

		for _, c := range convComments {
			if commentfilter.NormalizeUsername(c.Author.Username) == normBot {
				continue
			}

			if !mergeRe.MatchString(c.Body) {
				continue
			}

			if addressed[c.ID] {
				continue
			}

			logger.Info("Found sync command",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.Int("pr", pr.Number),
				zap.String("author", c.Author.Username))

			return &jobmanager.CommandSource{
				Owner:     r.Owner,
				Repo:      r.Repo,
				PRNumber:  pr.Number,
				CommentID: c.ID,
			}
		}
	}

	return nil
}

// hasUnmergeablePR checks all repos for PRs with merge conflicts.
// Applies idle detection: if the PR has no recent human activity,
// it is labeled and skipped. Returns true if any PR is unmergeable
// and active.
func (s *MergeScanner) hasUnmergeablePR(
	logger *zap.Logger,
	repos []models.RepoCoord,
	head string,
) bool {
	for _, r := range repos {
		pr, err := s.prs.GetPRForBranch(r.Owner, r.Repo, head)
		if err != nil {
			logger.Warn("Error looking up PR for repo",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.Error(err))
			continue
		}
		if pr == nil {
			continue
		}

		if s.hasSkipLabel(logger, r, pr) {
			continue
		}

		state, err := s.mergeCheck.GetPRMergeability(r.Owner, r.Repo, pr.Number)
		if err != nil {
			logger.Warn("Failed to check mergeability",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.Error(err))
			continue
		}

		if state.Mergeable == nil {
			logger.Debug("Mergeability unknown, skipping until next cycle",
				zap.String("repo", r.Owner+"/"+r.Repo))
			continue
		}

		if *state.Mergeable {
			continue
		}

		// PR is unmergeable — check idle status.
		if s.isIdlePR(logger, r.Owner, r.Repo, pr) {
			continue
		}

		logger.Info("Found unmergeable PR",
			zap.String("repo", r.Owner+"/"+r.Repo),
			zap.Int("pr", pr.Number))
		return true
	}

	return false
}

// isIdlePR checks whether a PR should be skipped due to inactivity.
// Returns true if the PR is idle (labeled or newly labeled). Returns
// false if idle detection is disabled, the PR is active, or on error.
//
// Human activity is determined from three signals: PR comments from
// non-bot users, the PR creation date (when no human has commented),
// and manual removal of the idle label. A PR is idle when the most
// recent of these signals is older than the IdleDays threshold.
func (s *MergeScanner) isIdlePR(
	logger *zap.Logger,
	owner, repo string,
	pr *models.PRDetails,
) bool {
	if s.cfg.IdleDays <= 0 || s.cfg.IdleLabel == "" {
		return false
	}

	hasLabel, err := s.labeler.HasPRLabel(owner, repo, pr.Number, s.cfg.IdleLabel)
	if err != nil {
		logger.Warn("Failed to check idle label, treating as active",
			zap.String("repo", owner+"/"+repo),
			zap.Error(err))
		return false
	}

	idleThreshold := time.Now().AddDate(0, 0, -s.cfg.IdleDays)

	// Only fetch comments within the idle window — older comments
	// can't change the idle decision and would waste API calls on
	// long-idle PRs with large comment histories.
	comments, err := s.prs.GetPRComments(owner, repo, pr.Number, idleThreshold)
	if err != nil {
		logger.Warn("Failed to fetch PR comments for idle check, treating as active",
			zap.String("repo", owner+"/"+repo),
			zap.Error(err))
		return false
	}

	lastHuman := lastHumanCommentTime(comments, s.cfg.BotUsername,
		s.cfg.KnownBotUsernames, s.cfg.IgnoredUsernames)

	// When no human has commented within the window, use the PR
	// creation date as baseline. A new PR isn't idle — it just
	// hasn't been reviewed.
	if lastHuman.IsZero() {
		lastHuman = pr.CreatedAt
	}
	idle := lastHuman.IsZero() || lastHuman.Before(idleThreshold)

	// Check for recent manual label removal before declaring idle.
	// Only pay the API cost when the PR appears idle from comments.
	if idle {
		lastRemoval, err := s.labeler.LastLabelRemoval(owner, repo, pr.Number, s.cfg.IdleLabel)
		if err != nil {
			logger.Warn("Failed to check label removal history, treating as active",
				zap.String("repo", owner+"/"+repo),
				zap.Error(err))
			return false
		}
		if !lastRemoval.IsZero() && lastRemoval.After(idleThreshold) {
			logger.Info("Idle label was recently removed, respecting manual override",
				zap.String("repo", owner+"/"+repo),
				zap.Int("pr", pr.Number),
				zap.Time("last_removal", lastRemoval))
			idle = false
		}
	}

	if hasLabel {
		if idle {
			logger.Debug("PR has idle label, skipping",
				zap.String("repo", owner+"/"+repo),
				zap.Int("pr", pr.Number))
			return true
		}
		logger.Info("PR reactivated, removing idle label",
			zap.String("repo", owner+"/"+repo),
			zap.Int("pr", pr.Number),
			zap.Time("last_human_comment", lastHuman))
		if err := s.labeler.RemovePRLabel(owner, repo, pr.Number, s.cfg.IdleLabel); err != nil {
			logger.Warn("Failed to remove idle label",
				zap.String("repo", owner+"/"+repo),
				zap.Error(err))
		}
		return false
	}

	if idle {
		logger.Info("PR is idle, adding label",
			zap.String("repo", owner+"/"+repo),
			zap.Int("pr", pr.Number),
			zap.Time("last_human_comment", lastHuman))
		if err := s.labeler.AddPRLabel(owner, repo, pr.Number, s.cfg.IdleLabel); err != nil {
			logger.Warn("Failed to add idle label, treating as active",
				zap.String("repo", owner+"/"+repo),
				zap.Error(err))
			return false
		}
		return true
	}

	return false
}

// hasSkipLabel checks whether the skip-PR label is present on the
// given PR. Returns false when skip-label checking is not configured
// or on API error (fail-open).
func (s *MergeScanner) hasSkipLabel(
	logger *zap.Logger,
	r models.RepoCoord,
	pr *models.PRDetails,
) bool {
	if s.cfg.SkipPRLabel == "" {
		return false
	}
	has, err := s.labeler.HasPRLabel(r.Owner, r.Repo, pr.Number, s.cfg.SkipPRLabel)
	if err != nil {
		logger.Warn("Failed to check skip-PR label, treating as not skipped",
			zap.String("repo", r.Owner+"/"+r.Repo),
			zap.Int("pr", pr.Number),
			zap.Error(err))
		return false
	}
	if has {
		logger.Debug("PR has skip label, skipping",
			zap.String("repo", r.Owner+"/"+r.Repo),
			zap.Int("pr", pr.Number),
			zap.String("label", s.cfg.SkipPRLabel))
	}
	return has
}

// lastHumanCommentTime returns the timestamp of the most recent PR
// comment not authored by the bot or any known bot/ignored user.
// Returns the zero time if no human comments exist.
func lastHumanCommentTime(
	comments []models.PRComment,
	botUsername string,
	knownBots, ignoredUsers []string,
) time.Time {
	excluded := make(map[string]bool)
	excluded[commentfilter.NormalizeUsername(botUsername)] = true
	for _, u := range knownBots {
		excluded[commentfilter.NormalizeUsername(u)] = true
	}
	for _, u := range ignoredUsers {
		excluded[commentfilter.NormalizeUsername(u)] = true
	}

	var latest time.Time
	for _, c := range comments {
		if excluded[commentfilter.NormalizeUsername(c.Author.Username)] {
			continue
		}
		if c.Timestamp.After(latest) {
			latest = c.Timestamp
		}
	}
	return latest
}
