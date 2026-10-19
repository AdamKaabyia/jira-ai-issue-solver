package scanner

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/models"
)

// Compile-time check that TriageLabelScanner implements Scanner.
var _ Scanner = (*TriageLabelScanner)(nil)

// TriageLabelScannerConfig holds configuration for [TriageLabelScanner].
type TriageLabelScannerConfig struct {
	// Criteria defines the search query for tickets with active
	// triage labels.
	Criteria models.SearchCriteria

	// PollInterval is the time between scan cycles.
	PollInterval time.Duration

	// BotUsername is the bot's Jira username, used to distinguish
	// bot-assigned tickets from human-assigned ones.
	BotUsername string
}

// TriageLabelScanner polls for tickets carrying active triage labels
// and replaces them with the stale label when the ticket has left the
// triage phase (no longer in NewStatus and assigned to a human).
type TriageLabelScanner struct {
	searcher      IssueSearcher
	labels        LabelManager
	labelResolver TriageLabelResolver
	cfg           TriageLabelScannerConfig
	logger        *zap.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewTriageLabelScanner creates a TriageLabelScanner with the given
// dependencies. Returns an error if any required parameter is invalid.
func NewTriageLabelScanner(
	searcher IssueSearcher,
	labels LabelManager,
	labelResolver TriageLabelResolver,
	cfg TriageLabelScannerConfig,
	logger *zap.Logger,
) (*TriageLabelScanner, error) {
	if searcher == nil {
		return nil, errors.New("issue searcher must not be nil")
	}
	if labels == nil {
		return nil, errors.New("label manager must not be nil")
	}
	if labelResolver == nil {
		return nil, errors.New("triage label resolver must not be nil")
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

	return &TriageLabelScanner{
		searcher:      searcher,
		labels:        labels,
		labelResolver: labelResolver,
		cfg:           cfg,
		logger:        logger,
	}, nil
}

// Start begins polling in a background goroutine.
func (s *TriageLabelScanner) Start(ctx context.Context) error {
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
func (s *TriageLabelScanner) Stop() {
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

func (s *TriageLabelScanner) run(ctx context.Context) {
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

func (s *TriageLabelScanner) scan(ctx context.Context) {
	items, err := s.searcher.SearchWorkItems(s.cfg.Criteria)
	if err != nil {
		s.logger.Error("Failed to search for triage-labeled tickets", zap.Error(err))
		return
	}

	if len(items) == 0 {
		s.logger.Debug("No triage-labeled tickets found")
		return
	}

	s.logger.Info("Found triage-labeled tickets", zap.Int("count", len(items)))

	for _, item := range items {
		if ctx.Err() != nil {
			return
		}
		s.processItem(item)
	}
}

func (s *TriageLabelScanner) processItem(item models.WorkItem) {
	logger := s.logger.With(zap.String("ticket", item.Key))

	tl := s.labelResolver.ResolveTriageLabels(item)
	if len(tl.Active) == 0 {
		return
	}

	if item.Status == tl.NewStatus {
		return
	}

	if item.Assignee == nil {
		return
	}

	if strings.EqualFold(item.Assignee.Username, s.cfg.BotUsername) {
		return
	}

	var present []string
	for _, active := range tl.Active {
		if active == "" {
			continue
		}
		if slices.ContainsFunc(item.Labels, func(l string) bool {
			return strings.EqualFold(l, active)
		}) {
			present = append(present, active)
		}
	}

	if len(present) == 0 {
		return
	}

	hasStale := tl.Stale != "" && slices.ContainsFunc(item.Labels, func(l string) bool {
		return strings.EqualFold(l, tl.Stale)
	})

	if tl.Stale != "" && !hasStale {
		if err := s.labels.AddLabel(item.Key, tl.Stale); err != nil {
			logger.Warn("Failed to add stale label, skipping cleanup to preserve discoverability",
				zap.String("label", tl.Stale), zap.Error(err))
			return
		}
	}

	for _, active := range present {
		if err := s.labels.RemoveLabel(item.Key, active); err != nil {
			logger.Warn("Failed to remove triage label",
				zap.String("label", active), zap.Error(err))
		}
	}

	logger.Info("Cleaned up triage labels",
		zap.String("stale", tl.Stale),
		zap.String("assignee", item.Assignee.Username))
}
