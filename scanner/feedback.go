package scanner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/commentfilter"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
)

// Compile-time check that FeedbackScanner implements Scanner.
var _ Scanner = (*FeedbackScanner)(nil)

// FeedbackScannerConfig holds configuration for [FeedbackScanner].
type FeedbackScannerConfig struct {
	// Criteria defines the search query for "in review" tickets.
	Criteria models.SearchCriteria

	// PollInterval is the time between scan cycles.
	PollInterval time.Duration

	// BotUsername is the bot's GitHub username, used for branch
	// name construction and comment filtering.
	BotUsername string

	// IgnoredUsernames lists users whose comments are skipped
	// entirely.
	IgnoredUsernames []string

	// KnownBotUsernames lists other bots for loop prevention.
	KnownBotUsernames []string

	// MaxThreadDepth limits bot replies per thread. Zero or
	// negative means no limit.
	MaxThreadDepth int

	// IgnoredCheckNames lists check run names excluded from CI
	// failure detection (case-insensitive).
	IgnoredCheckNames []string

	// MaxCIFixAttempts limits CI fix attempts per PR. Zero
	// disables CI failure detection. Negative means unlimited.
	MaxCIFixAttempts int

	// SkipPRLabel is the GitHub label that tells the bot to skip
	// a PR entirely. Empty disables the check.
	SkipPRLabel string
}

// FeedbackScanner polls for tickets in "in review" status and checks
// GitHub for actionable PR comments. Applies bot-loop prevention via
// [commentfilter.HasNewActionable] before emitting events.
type FeedbackScanner struct {
	searcher               IssueSearcher
	submitter              JobSubmitter
	prs                    PRFetcher
	repos                  RepoLocator
	ci                     CIChecker
	labels                 LabelManager
	prLabeler              PRLabeler
	labelResolver          FailureLabelResolver
	lifecycleLabelResolver LifecycleLabelResolver
	mergedStatusResolver   MergedStatusResolver
	statusTransitioner     StatusTransitioner
	cfg                    FeedbackScannerConfig
	logger                 *zap.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewFeedbackScanner creates a FeedbackScanner with the given
// dependencies. Returns an error if any required parameter is invalid.
func NewFeedbackScanner(
	searcher IssueSearcher,
	submitter JobSubmitter,
	prs PRFetcher,
	repos RepoLocator,
	ci CIChecker,
	cfg FeedbackScannerConfig,
	logger *zap.Logger,
	opts ...FeedbackScannerOption,
) (*FeedbackScanner, error) {
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
	if cfg.PollInterval <= 0 {
		return nil, errors.New("poll interval must be positive")
	}
	if cfg.BotUsername == "" {
		return nil, errors.New("bot username must not be empty")
	}
	if logger == nil {
		return nil, errors.New("logger must not be nil")
	}

	fs := &FeedbackScanner{
		searcher:  searcher,
		submitter: submitter,
		prs:       prs,
		repos:     repos,
		ci:        ci,
		cfg:       cfg,
		logger:    logger,
	}
	for _, opt := range opts {
		opt(fs)
	}
	if fs.lifecycleLabelResolver != nil && fs.labels == nil {
		logger.Warn("Lifecycle label resolver configured without a LabelManager — lifecycle labeling will be disabled")
	}
	return fs, nil
}

// FeedbackScannerOption configures optional behavior on a
// [FeedbackScanner]. Pass to [NewFeedbackScanner].
type FeedbackScannerOption func(*FeedbackScanner)

// WithLabelManager enables failure-state label management on the
// scanner. Both a [LabelManager] and a [FailureLabelResolver] are
// required; if either is nil, labeling is silently disabled.
func WithLabelManager(lm LabelManager, lr FailureLabelResolver) FeedbackScannerOption {
	return func(fs *FeedbackScanner) {
		if lm != nil && lr != nil {
			fs.labels = lm
			fs.labelResolver = lr
		}
	}
}

// WithLifecycleLabelManager enables lifecycle label management and
// merged-status transitions on the scanner. Requires [WithLabelManager]
// to also be configured; lifecycle operations use the same [LabelManager]
// for add/remove calls. If lr is nil, lifecycle labeling is silently
// disabled. mr and st are optional — nil disables merged-status
// transitions without affecting label management.
func WithLifecycleLabelManager(lr LifecycleLabelResolver, mr MergedStatusResolver, st StatusTransitioner) FeedbackScannerOption {
	return func(fs *FeedbackScanner) {
		if lr != nil {
			fs.lifecycleLabelResolver = lr
		}
		if mr != nil {
			fs.mergedStatusResolver = mr
		}
		if st != nil {
			fs.statusTransitioner = st
		}
	}
}

// WithPRLabeler enables skip-label checking on PRs. When configured
// with a non-empty [FeedbackScannerConfig.SkipPRLabel], the scanner
// skips PRs carrying that label.
func WithPRLabeler(pl PRLabeler) FeedbackScannerOption {
	return func(fs *FeedbackScanner) {
		if pl != nil {
			fs.prLabeler = pl
		}
	}
}

// Start begins polling in a background goroutine.
func (s *FeedbackScanner) Start(ctx context.Context) error {
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
func (s *FeedbackScanner) Stop() {
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

func (s *FeedbackScanner) run(ctx context.Context) {
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

func (s *FeedbackScanner) scan(ctx context.Context) {
	items, err := s.searcher.SearchWorkItems(s.cfg.Criteria)
	if err != nil {
		s.logger.Error("Failed to search for in-review tickets", zap.Error(err))
		return
	}

	if len(items) == 0 {
		s.logger.Debug("No in-review tickets found")
		return
	}

	s.logger.Info("Found in-review tickets", zap.Int("count", len(items)))

	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		if s.checkAndSubmit(item) {
			return
		}
	}
}

// checkAndSubmit checks a ticket for actionable PR comments across all
// repos in its workspace and submits a feedback event if found. Returns
// true if the scan cycle should stop (circuit breaker open or shutdown).
func (s *FeedbackScanner) checkAndSubmit(item models.WorkItem) bool {
	logger := s.logger.With(zap.String("ticket", item.Key))

	repos, err := s.repos.LocateRepos(item)
	if err != nil {
		logger.Warn("Failed to locate repos, skipping", zap.Error(err))
		return false
	}

	branchName := fmt.Sprintf("%s/%s", s.cfg.BotUsername, item.Key)
	heads := s.repos.ForkOwnerHeads(item, branchName)

	obs := s.observeRepos(logger, repos, heads)

	var allLabels []string
	var fl models.FailureLabels
	var ll models.LifecycleLabels
	if s.labels != nil {
		if s.labelResolver != nil {
			fl = s.labelResolver.ResolveFailureLabels(item)
		}
		if s.lifecycleLabelResolver != nil {
			ll = s.lifecycleLabelResolver.ResolveLifecycleLabels(item)
		}
		allLabels = models.AllPipelineLabels(fl, ll)
	}

	currentLabels := labelSet(item.Labels)
	s.updateFailureLabels(logger, item.Key, repos, heads, obs, fl, ll, allLabels, currentLabels)
	s.checkAndApplyMergedLabel(logger, item, repos, heads, ll, allLabels, currentLabels)

	if !obs.actionable {
		return false
	}

	event := jobmanager.Event{
		Type:      jobmanager.JobTypeFeedback,
		TicketKey: item.Key,
	}

	_, err = s.submitter.Submit(event)
	if err == nil {
		logger.Info("Submitted feedback event")
		return false
	}

	switch {
	case errors.Is(err, jobmanager.ErrDuplicateJob):
		logger.Debug("Skipping duplicate feedback")
	case errors.Is(err, jobmanager.ErrRetriesExhausted):
		logger.Debug("Skipping exhausted ticket")
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
		logger.Error("Failed to submit feedback event", zap.Error(err))
	}

	return false
}

// repoObservation summarises PR/CI state across all repos for a ticket.
type repoObservation struct {
	actionable  bool // at least one repo has actionable comments or CI failures
	hasOpenPR   bool // at least one repo has an open PR
	ciChecked   bool // CI status was successfully determined for at least one repo
	ciIsFailing bool // at least one repo has failing CI (all checks completed)
}

// observeRepos checks all repos for PRs with actionable review
// comments or CI failures and captures the aggregate state. Each
// repo independently tries all candidate heads so that mixed-head
// scenarios (e.g., fork migration) are handled correctly.
func (s *FeedbackScanner) observeRepos(
	logger *zap.Logger,
	repos []models.RepoCoord,
	heads []string,
) repoObservation {
	var obs repoObservation
	for _, r := range repos {
		pr := s.findOpenPRForRepo(logger, r, heads)
		if pr == nil {
			continue
		}
		obs.hasOpenPR = true

		if s.hasSkipLabel(logger, r, pr) {
			continue
		}

		comments, err := s.prs.GetPRComments(r.Owner, r.Repo, pr.Number, time.Time{})
		if err != nil {
			logger.Warn("Failed to fetch PR comments",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.Error(err))
			continue
		}

		ciResult := s.checkCI(logger, r.Owner, r.Repo, pr, comments)
		if ciResult.checked {
			obs.ciChecked = true
			if ciResult.hasFailures {
				obs.ciIsFailing = true
			}
		}

		if commentfilter.HasNewActionable(comments, s.filterConfig()) {
			obs.actionable = true
		}

		if ciResult.actionable {
			obs.actionable = true
		}
	}

	if !obs.actionable {
		logger.Debug("No actionable feedback")
	}
	return obs
}

// findOpenPRForRepo tries each candidate head for a repo and returns
// the first open PR found, or nil if none match.
func (s *FeedbackScanner) findOpenPRForRepo(
	logger *zap.Logger,
	r models.RepoCoord,
	heads []string,
) *models.PRDetails {
	for _, head := range heads {
		pr, err := s.prs.GetPRForBranch(r.Owner, r.Repo, head)
		if err != nil {
			logger.Warn("Error looking up PR for repo",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.String("head", head),
				zap.Error(err))
			continue
		}
		if pr != nil {
			return pr
		}
	}
	return nil
}

// hasSkipLabel checks whether the skip-PR label is present on the
// given PR. Returns false when skip-label checking is not configured
// or on API error (fail-open).
func (s *FeedbackScanner) hasSkipLabel(
	logger *zap.Logger,
	r models.RepoCoord,
	pr *models.PRDetails,
) bool {
	if s.prLabeler == nil || s.cfg.SkipPRLabel == "" {
		return false
	}
	has, err := s.prLabeler.HasPRLabel(r.Owner, r.Repo, pr.Number, s.cfg.SkipPRLabel)
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

// ciCheckResult captures the CI state for a single repo.
type ciCheckResult struct {
	checked     bool // CI was successfully queried and all checks completed
	hasFailures bool // non-ignored, non-pre-existing failures exist
	actionable  bool // failures exist and fix attempts are not exhausted
}

// updateFailureLabels applies or removes failure-state labels based on
// observed PR/CI state. No-op when labeling is not configured.
func (s *FeedbackScanner) updateFailureLabels(
	logger *zap.Logger,
	ticketKey string,
	repos []models.RepoCoord,
	heads []string,
	obs repoObservation,
	fl models.FailureLabels,
	ll models.LifecycleLabels,
	allLabels []string,
	currentLabels map[string]bool,
) {
	if s.labels == nil || fl == (models.FailureLabels{}) {
		return
	}

	switch {
	case !obs.hasOpenPR:
		if s.detectRejection(logger, repos, heads) {
			if fl.Rejected != "" {
				s.applyPipelineLabel(logger, ticketKey, allLabels, currentLabels, fl.Rejected)
			}
			return
		}
	case obs.ciIsFailing:
		if fl.CIFailing != "" {
			s.applyPipelineLabel(logger, ticketKey, allLabels, currentLabels, fl.CIFailing)
		}
		return
	case obs.ciChecked && !obs.ciIsFailing:
		if fl.CIFailing != "" {
			if ll.Review != "" {
				s.applyPipelineLabel(logger, ticketKey, allLabels, currentLabels, ll.Review)
			} else if currentLabels[fl.CIFailing] {
				if err := s.labels.RemoveLabel(ticketKey, fl.CIFailing); err != nil {
					logger.Debug("Failed to remove CI-failing label", zap.Error(err))
				}
			}
		}
	}
}

// detectRejection checks for a closed (not merged) PR across all
// repos, trying each candidate head per repo. Returns true if at
// least one repo has a rejected PR.
func (s *FeedbackScanner) detectRejection(
	logger *zap.Logger,
	repos []models.RepoCoord,
	heads []string,
) bool {
	for _, r := range repos {
		for _, head := range heads {
			pr, err := s.prs.GetClosedPRForBranch(r.Owner, r.Repo, head)
			if err != nil {
				logger.Debug("Error checking for closed PR",
					zap.String("repo", r.Owner+"/"+r.Repo),
					zap.String("head", head),
					zap.Error(err))
				continue
			}
			if pr != nil {
				logger.Info("PR rejected (closed without merge)",
					zap.String("pr_url", pr.URL))
				return true
			}
		}
	}
	return false
}

// applyPipelineLabel sets one pipeline label and removes all others
// from both failure and lifecycle groups. This enforces mutual
// exclusivity — a ticket should have exactly one pipeline label.
// Idempotent: skips API calls when labels are already correct.
// No-op when target is empty (not configured).
func (s *FeedbackScanner) applyPipelineLabel(
	logger *zap.Logger,
	ticketKey string,
	allLabels []string,
	currentLabels map[string]bool,
	target string,
) {
	if target == "" {
		return
	}
	for _, label := range allLabels {
		if label != "" && label != target && currentLabels[label] {
			if err := s.labels.RemoveLabel(ticketKey, label); err != nil {
				logger.Debug("Failed to remove pipeline label",
					zap.String("label", label), zap.Error(err))
			}
		}
	}
	if !currentLabels[target] {
		if err := s.labels.AddLabel(ticketKey, target); err != nil {
			logger.Warn("Failed to add pipeline label",
				zap.String("label", target), zap.Error(err))
		}
	}
}

// labelSet converts a label slice to a set for O(1) lookups.
func labelSet(labels []string) map[string]bool {
	s := make(map[string]bool, len(labels))
	for _, l := range labels {
		s[l] = true
	}
	return s
}

// checkAndApplyMergedLabel checks whether all repos that had a PR
// have it merged and, if so, applies the "merged" lifecycle label
// and optionally transitions the ticket to the configured merged
// status. Repos that never had a PR are skipped. The "review"
// lifecycle label is handled by the executor at PR creation time.
// No-op when lifecycle labeling is not configured.
func (s *FeedbackScanner) checkAndApplyMergedLabel(
	logger *zap.Logger,
	item models.WorkItem,
	repos []models.RepoCoord,
	heads []string,
	ll models.LifecycleLabels,
	allLabels []string,
	currentLabels map[string]bool,
) {
	if s.labels == nil || ll == (models.LifecycleLabels{}) {
		return
	}

	if !s.detectMerge(logger, repos, heads) {
		return
	}

	s.applyPipelineLabel(logger, item.Key, allLabels, currentLabels, ll.Merged)

	if s.mergedStatusResolver == nil || s.statusTransitioner == nil {
		return
	}
	mergedStatus := s.mergedStatusResolver.ResolveMergedStatus(item)
	if mergedStatus != "" {
		if err := s.statusTransitioner.TransitionStatus(item.Key, mergedStatus); err != nil {
			logger.Warn("Failed to transition to merged status",
				zap.String("status", mergedStatus), zap.Error(err))
		}
	}
}

// detectMerge checks whether all repos that had a PR have it merged.
// Each repo independently tries all candidate heads so that
// mixed-head scenarios (e.g., fork migration) are handled correctly.
// Repos where no PR was ever created are skipped — in multi-repo
// workspaces, the AI may only change a subset of repos. Returns
// false when any repo still has an unmerged PR or when no repo had
// a PR at all.
func (s *FeedbackScanner) detectMerge(
	logger *zap.Logger,
	repos []models.RepoCoord,
	heads []string,
) bool {
	hadPR := 0
	for _, r := range repos {
		state := s.detectRepoPRState(logger, r, heads)
		switch state {
		case prStateMerged:
			hadPR++
		case prStateOpen, prStateClosed:
			return false
		case prStateError:
			return false
		case prStateNone:
			logger.Debug("Repo skipped — no PR ever created",
				zap.String("repo", r.Owner+"/"+r.Repo))
		}
	}
	if hadPR > 0 {
		logger.Info("All PRs merged")
	}
	return hadPR > 0
}

type prState int

const (
	prStateNone   prState = iota // no PR ever created under any head
	prStateMerged                // merged PR found
	prStateOpen                  // open (unmerged) PR found
	prStateClosed                // closed (not merged) PR found
	prStateError                 // API error during lookup
)

// detectRepoPRState resolves the PR state for a single repo by
// checking all candidate heads per state in priority order
// (merged > open > closed). This ensures a merged PR under one
// head is not masked by a stale closed PR under a different head.
func (s *FeedbackScanner) detectRepoPRState(
	logger *zap.Logger,
	r models.RepoCoord,
	heads []string,
) prState {
	for _, head := range heads {
		merged, err := s.prs.GetMergedPRForBranch(r.Owner, r.Repo, head)
		if err != nil {
			logger.Warn("Error checking for merged PR",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.String("head", head),
				zap.Error(err))
			return prStateError
		}
		if merged != nil {
			return prStateMerged
		}
	}
	for _, head := range heads {
		open, err := s.prs.GetPRForBranch(r.Owner, r.Repo, head)
		if err != nil {
			logger.Warn("Error checking for open PR",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.String("head", head),
				zap.Error(err))
			return prStateError
		}
		if open != nil {
			return prStateOpen
		}
	}
	for _, head := range heads {
		closed, err := s.prs.GetClosedPRForBranch(r.Owner, r.Repo, head)
		if err != nil {
			logger.Warn("Error checking for closed PR",
				zap.String("repo", r.Owner+"/"+r.Repo),
				zap.String("head", head),
				zap.Error(err))
			return prStateError
		}
		if closed != nil {
			return prStateClosed
		}
	}
	return prStateNone
}

// checkCI queries CI status for a PR and returns the full state:
// whether CI was checked, whether failures exist, and whether the
// failures are actionable (not exhausted, not disabled). This avoids
// redundant API calls by producing all CI signals in one pass.
func (s *FeedbackScanner) checkCI(
	logger *zap.Logger,
	owner, repo string,
	pr *models.PRDetails,
	comments []models.PRComment,
) ciCheckResult {
	if s.ci == nil || pr.HeadSHA == "" {
		return ciCheckResult{}
	}

	failures, allCompleted, err := s.ci.ListCheckRunsForRef(owner, repo, pr.HeadSHA)
	if err != nil {
		logger.Warn("Failed to check CI status",
			zap.String("repo", owner+"/"+repo),
			zap.Error(err))
		return ciCheckResult{}
	}
	if !allCompleted {
		logger.Debug("CI checks still running, skipping CI analysis",
			zap.String("repo", owner+"/"+repo))
		return ciCheckResult{}
	}

	filtered := filterIgnoredChecks(failures, s.cfg.IgnoredCheckNames)
	if len(filtered) == 0 {
		return ciCheckResult{checked: true}
	}

	if pr.BaseBranch != "" {
		baseFailures, _, baseErr := s.ci.ListCheckRunsForRef(owner, repo, pr.BaseBranch)
		if baseErr == nil {
			filtered = filterPreExistingFailures(filtered, baseFailures)
			if len(filtered) == 0 {
				return ciCheckResult{checked: true}
			}
		}
	}

	result := ciCheckResult{checked: true, hasFailures: true}

	if s.cfg.MaxCIFixAttempts == 0 {
		return result
	}

	if s.cfg.MaxCIFixAttempts > 0 {
		attempts := commentfilter.CountCIFixAttempts(comments, s.cfg.BotUsername)
		if attempts >= s.cfg.MaxCIFixAttempts {
			logger.Debug("CI fix attempts exhausted",
				zap.String("repo", owner+"/"+repo),
				zap.Int("attempts", attempts),
				zap.Int("max", s.cfg.MaxCIFixAttempts))
			return result
		}
	}

	logger.Info("Found actionable CI failures",
		zap.String("repo", owner+"/"+repo),
		zap.Int("count", len(filtered)))
	result.actionable = true
	return result
}

// filterPreExistingFailures removes failures whose check name also
// appears in base branch failures.
func filterPreExistingFailures(
	prFailures, baseFailures []models.CheckRunFailure,
) []models.CheckRunFailure {
	if len(baseFailures) == 0 {
		if prFailures == nil {
			return []models.CheckRunFailure{}
		}
		return prFailures
	}

	baseNames := make(map[string]bool, len(baseFailures))
	for _, f := range baseFailures {
		baseNames[f.Name] = true
	}

	var filtered []models.CheckRunFailure
	for _, f := range prFailures {
		if !baseNames[f.Name] {
			filtered = append(filtered, f)
		}
	}
	if filtered == nil {
		filtered = []models.CheckRunFailure{}
	}
	return filtered
}

// filterIgnoredChecks removes check runs whose names match the ignore
// list (case-insensitive).
func filterIgnoredChecks(
	failures []models.CheckRunFailure, ignored []string,
) []models.CheckRunFailure {
	if len(ignored) == 0 {
		if failures == nil {
			return []models.CheckRunFailure{}
		}
		return failures
	}

	ignoredSet := make(map[string]bool, len(ignored))
	for _, name := range ignored {
		ignoredSet[strings.ToLower(name)] = true
	}

	var filtered []models.CheckRunFailure
	for _, f := range failures {
		if !ignoredSet[strings.ToLower(f.Name)] {
			filtered = append(filtered, f)
		}
	}
	if filtered == nil {
		filtered = []models.CheckRunFailure{}
	}
	return filtered
}

func (s *FeedbackScanner) filterConfig() commentfilter.Config {
	return commentfilter.Config{
		BotUsername:       s.cfg.BotUsername,
		IgnoredUsernames:  s.cfg.IgnoredUsernames,
		KnownBotUsernames: s.cfg.KnownBotUsernames,
		MaxThreadDepth:    s.cfg.MaxThreadDepth,
	}
}
