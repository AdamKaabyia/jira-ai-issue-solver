// Package scanner implements event-based polling scanners that
// discover work from external systems and emit events to the
// job manager.
//
// # WorkItemScanner
//
// Polls the issue tracker for tickets in "todo" status and emits
// [jobmanager.JobTypeNewTicket] events. The scanner is stateless:
// it queries each cycle and relies on the job manager for
// deduplication.
//
// # FeedbackScanner
//
// Polls the issue tracker for tickets in "in review" status, then
// checks GitHub for new PR comments. Applies bot-loop prevention
// filters (ignored users, known bots, thread depth) via the
// [commentfilter] package before emitting [jobmanager.JobTypeFeedback]
// events.
//
// # Consumer-defined interfaces
//
// The scanner defines narrow interfaces for its dependencies
// ([IssueSearcher], [JobSubmitter], [PRFetcher], [RepoLocator])
// rather than importing shared interface packages. The underlying
// implementations (e.g., Jira adapter, GitHub service) satisfy these
// interfaces implicitly.
//
// Test doubles are provided in the [scannertest] subpackage.
package scanner

import (
	"context"
	"time"

	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
)

// Scanner manages the lifecycle of a polling scanner.
type Scanner interface {
	// Start begins polling in a background goroutine. The first
	// scan runs immediately. Returns an error if already running.
	// The scanner stops when the context is cancelled or [Stop]
	// is called.
	Start(ctx context.Context) error

	// Stop cancels polling and blocks until the background
	// goroutine exits. Safe to call multiple times or without
	// a prior Start.
	Stop()
}

// IssueSearcher searches for work items in the issue tracker.
type IssueSearcher interface {
	SearchWorkItems(criteria models.SearchCriteria) ([]models.WorkItem, error)
}

// JobSubmitter creates jobs from scanner events.
type JobSubmitter interface {
	Submit(event jobmanager.Event) (*jobmanager.Job, error)
}

// RetryResetter clears a ticket's failure count so it can be
// resubmitted after exhausting the retry limit.
type RetryResetter interface {
	ResetRetries(ticketKey string) error
}

// LabelRemover removes labels from work items.
type LabelRemover interface {
	RemoveLabel(key, label string) error
}

// LabelManager adds and removes labels on work items. Used by
// [FeedbackScanner] for failure-state label management.
type LabelManager interface {
	AddLabel(key, label string) error
	RemoveLabel(key, label string) error
}

// FailureLabelResolver resolves per-project failure label
// configuration for a work item.
type FailureLabelResolver interface {
	ResolveFailureLabels(item models.WorkItem) models.FailureLabels
}

// LifecycleLabelResolver resolves per-project lifecycle label
// configuration for a work item.
type LifecycleLabelResolver interface {
	ResolveLifecycleLabels(item models.WorkItem) models.LifecycleLabels
}

// TriageLabelResolver resolves per-project triage label
// configuration for a work item.
type TriageLabelResolver interface {
	ResolveTriageLabels(item models.WorkItem) models.TriageLabels
}

// MergedStatusResolver resolves the merged status transition for a
// work item's project and ticket type.
type MergedStatusResolver interface {
	ResolveMergedStatus(item models.WorkItem) string
}

// StatusTransitioner transitions a work item to a new status.
type StatusTransitioner interface {
	TransitionStatus(key, status string) error
}

// PRFetcher retrieves PR details and comments from GitHub.
type PRFetcher interface {
	// GetPRForBranch finds the open pull request whose head
	// branch matches the given name. Returns nil, nil when no
	// matching PR is found.
	GetPRForBranch(owner, repo, head string) (*models.PRDetails, error)

	// GetClosedPRForBranch finds a closed (not merged) pull request
	// whose head branch matches the given name. Returns nil, nil
	// when no rejected PR exists.
	GetClosedPRForBranch(owner, repo, head string) (*models.PRDetails, error)

	// GetMergedPRForBranch finds a merged pull request whose head
	// branch matches the given name. Returns nil, nil when no
	// merged PR exists.
	GetMergedPRForBranch(owner, repo, head string) (*models.PRDetails, error)

	// GetPRComments returns comments on the given pull request.
	// Pass time.Time{} to retrieve all comments.
	GetPRComments(owner, repo string, number int, since time.Time) ([]models.PRComment, error)
}

// RepoLocator maps work items to their GitHub repository coordinates.
type RepoLocator interface {
	// LocateRepo returns the GitHub owner and repo name for the
	// given work item's component-to-repo mapping. For multi-repo
	// workspaces, returns the first repo.
	LocateRepo(workItem models.WorkItem) (owner, repo string, err error)

	// LocateRepos returns all repositories for the given work item.
	// For single-repo workspaces this returns one entry; for multi-repo
	// workspaces it returns all repos in the workspace.
	LocateRepos(workItem models.WorkItem) ([]models.RepoCoord, error)

	// ForkOwnerHeads returns candidate PR head refs in priority
	// order. Fork-mode projects return the fork head first with a
	// direct-mode fallback.
	ForkOwnerHeads(workItem models.WorkItem, branchName string) []string
}

// MergeabilityChecker retrieves the merge status of a pull request.
// Used by [MergeScanner] to detect PRs that cannot be merged due to
// conflicts with the target branch.
type MergeabilityChecker interface {
	GetPRMergeability(owner, repo string, number int) (*models.PRMergeState, error)
}

// PRLabeler manages labels on pull requests. Used by [MergeScanner]
// for idle-label management and by [FeedbackScanner] for skip-label
// checking.
type PRLabeler interface {
	AddPRLabel(owner, repo string, number int, label string) error
	RemovePRLabel(owner, repo string, number int, label string) error
	HasPRLabel(owner, repo string, number int, label string) (bool, error)
	LastLabelRemoval(owner, repo string, number int, label string) (time.Time, error)
}

// CIChecker checks the CI status of a commit.
type CIChecker interface {
	ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error)
}

// WorkspaceCleaner removes workspaces matching a caller-provided
// predicate. Used by [WorkspaceCleanupScanner] to remove workspaces
// for tickets in terminal states.
type WorkspaceCleaner interface {
	CleanupByFilter(shouldRemove func(ticketKey string) bool) (int, error)
}

// TicketStatusChecker retrieves the current status of a ticket. Used
// by [WorkspaceCleanupScanner] to determine whether a workspace's
// ticket is still active.
type TicketStatusChecker interface {
	GetWorkItem(key string) (*models.WorkItem, error)
}
