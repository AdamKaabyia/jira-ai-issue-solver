// Package executor implements the job execution pipeline that
// processes work items from the job manager.
//
// The executor handles plumbing: workspace preparation, container
// lifecycle, task file creation, committing, PR management, and
// ticket status transitions. The AI agent handles thinking: code
// generation, validation, and fixing.
//
// # Consumer-defined interfaces
//
// The executor defines narrow interfaces for its dependencies
// ([GitService], [ProjectResolver]) rather than importing a shared
// interface package. This follows the Go convention of consumer-
// defined interfaces. Each consumer declares only the methods it
// requires; the underlying implementation satisfies all consumers.
// See docs/architecture.md for rationale.
//
// # Integration with JobManager
//
// The [Pipeline.Execute] method matches the [jobmanager.ExecuteFunc]
// signature, allowing direct injection into the Coordinator:
//
//	pipeline, _ := executor.NewPipeline(cfg, ...)
//	coordinator, _ := jobmanager.NewCoordinator(jmCfg, pipeline.Execute, logger)
//
// # Pipeline steps (new ticket)
//
//  1. Fetch work item details from the issue tracker
//  2. Resolve project-specific settings (repo, statuses, etc.)
//  3. Transition ticket to "in progress"
//  4. Prepare workspace (clone or reuse)
//  5. Create or switch to ticket branch
//  6. Write task file for the AI agent
//  7. Write provider-specific wrapper script
//  8. Load repo-level configuration hints
//  9. Resolve and start dev container
//  10. Execute AI agent inside container
//  11. Check for changes; fail if none
//  12. Commit changes via GitHub API
//  13. Sync workspace with remote
//  14. Create pull request and apply validation labels if needed
//  15. Update ticket with PR URL and transition status
//  16. Stop container (workspace retained for future jobs)
//
// # Pipeline steps (PR feedback)
//
//  1. Fetch work item and resolve project settings
//  2. Find open PR by branch name
//  3. Find or recreate workspace (self-healing if cleaned up)
//  4. Switch to PR branch and sync with remote
//  5. Fetch and categorize PR comments (new vs addressed)
//  6. Write feedback task file for the AI agent
//  7. Resolve container, execute AI, check for changes
//  8. Commit changes via GitHub API
//  9. Sync workspace with remote
//  10. Reply to addressed PR comments
//  11. Stop container (workspace retained)
//
// Key differences from the new-ticket pipeline: the feedback pipeline
// does not transition ticket status, does not create branches or PRs,
// reuses the existing workspace and PR branch, and replies to review
// comments after committing.
//
// Test doubles are provided in the [executortest] subpackage.
package executor

import (
	"context"
	"time"

	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
)

// Executor runs jobs to completion. The Execute method matches
// [jobmanager.ExecuteFunc] for direct injection into the Coordinator.
type Executor interface {
	Execute(ctx context.Context, job *jobmanager.Job) (jobmanager.JobResult, error)
}

// GitService defines the git and GitHub API operations needed by the
// execution pipelines. This is a consumer-defined interface containing
// the methods required by both the new-ticket and feedback pipelines.
//
// The underlying implementation (e.g., services.GitHubServiceImpl)
// satisfies this interface.
type GitService interface {
	// SyncFork syncs a fork's default branch with its upstream
	// parent via the GitHub merge-upstream API. Called before
	// CreateBranch in fork-based workflows to prevent stale
	// branches that produce massive diffs.
	SyncFork(forkOwner, repo, branch string) error

	// CreateBranch creates a new git branch in the workspace and
	// switches to it. baseBranch is the branch to fork from (e.g.,
	// "main").
	CreateBranch(dir, name, baseBranch string) error

	// SwitchBranch switches to an existing branch. Used when a
	// workspace is reused on retry or when checking out a PR branch
	// for feedback processing.
	SwitchBranch(dir, name string) error

	// RemoteBranchExists reports whether the named branch exists on
	// the remote repository. Used to detect when a user has deleted
	// a branch so the pipeline can start fresh.
	RemoteBranchExists(owner, repo, branch string) (bool, error)

	// DeleteRemoteBranch deletes a branch from the remote repository.
	// Returns nil if the branch does not exist (idempotent). Deleting
	// a branch auto-closes any open PR whose head matches it.
	DeleteRemoteBranch(owner, repo, branch string) error

	// HasChanges reports whether the workspace has uncommitted
	// changes (modified, added, or deleted tracked files).
	// baseBranch is used as a comparison ref when the remote
	// branch does not exist.
	HasChanges(dir, baseBranch string) (bool, error)

	// CommitChanges creates a verified commit via the GitHub API
	// from local workspace changes. Returns the commit SHA.
	// Returns services.ErrNoChanges if all changes are bot
	// artifacts and there is nothing to commit.
	//
	// upstreamOwner is the GitHub owner of the upstream repository.
	// In non-fork workflows it equals owner. In fork workflows it
	// identifies where the parent commit originated so the tree
	// can be resolved there when the fork API cannot find it.
	// importExcludes lists additional directories (from import
	// config) to exclude from commits beyond the built-in .ai-bot
	// and .ai-session exclusions.
	CommitChanges(upstreamOwner, owner, repo, branch, message, dir, baseBranch string,
		coAuthor *models.Author, importExcludes []string, skipFileGuardrail ...bool) (string, error)

	// StripRemoteAuth removes authentication credentials from the
	// workspace's origin remote URL, preventing push operations.
	// Used before handing control to the AI agent.
	StripRemoteAuth(dir string) error

	// RestoreRemoteAuth restores authentication credentials on the
	// workspace's origin remote URL using a fresh token. Must be
	// called after AI execution, before any operation that needs
	// remote access (e.g., SyncWithRemote).
	RestoreRemoteAuth(dir, owner, repo string) error

	// FetchRemote fetches all refs from the origin remote. Used in
	// fork-based workflows to fetch fork branches into a workspace
	// that was cloned from upstream.
	FetchRemote(dir string) error

	// SyncWithRemote reconciles the local workspace with the remote
	// branch after an API-created commit. importExcludes lists
	// additional directories to preserve across the hard reset.
	SyncWithRemote(dir, branch string, importExcludes []string) error

	// CreatePR creates a pull request.
	CreatePR(params models.PRParams) (*models.PR, error)

	// GetPRForBranch finds the open pull request whose head branch
	// matches the given branch name. Returns nil, nil when no
	// matching PR is found.
	GetPRForBranch(owner, repo, head string) (*models.PRDetails, error)

	// GetPRComments returns comments on the given pull request.
	// If since is the zero time, all comments are returned.
	GetPRComments(owner, repo string, number int,
		since time.Time) ([]models.PRComment, error)

	// ReplyToComment posts a threaded reply to a PR review comment.
	ReplyToComment(owner, repo string, prNumber int,
		commentID int64, body string) error

	// PostIssueComment posts a top-level comment on a PR (via the
	// issues endpoint) and returns the created comment's ID. Used
	// for replying to conversation comments and bot command
	// acknowledgments.
	PostIssueComment(owner, repo string, prNumber int,
		body string) (int64, error)

	// ListIssueComments returns all top-level comments on a PR.
	// Used to find existing bot comments for update-in-place.
	ListIssueComments(owner, repo string, prNumber int) ([]models.IssueComment, error)

	// UpdateIssueComment edits an existing top-level comment on a PR.
	UpdateIssueComment(owner, repo string, commentID int64, body string) error

	// AddCommentReaction adds an emoji reaction to a PR comment. Uses
	// the pull request reactions API for review comments and the issue
	// comment reactions API for conversation comments.
	AddCommentReaction(owner, repo string, comment models.PRComment, reaction string) error

	// MergeBase merges a base branch into the current branch in the
	// workspace. When fetchURL is non-empty, the base branch is
	// fetched from that URL (for fork-mode merges where origin
	// points to the fork but the merge target is upstream).
	// When fetchURL is empty, origin is used. On a clean merge,
	// returns nil and an empty slice. On conflict, returns
	// [services.ErrMergeConflict] and the list of conflicted file
	// paths (conflict markers are left in the working tree).
	MergeBase(dir, branch, fetchURL string) ([]string, error)

	// CloneImport clones an auxiliary repository into destDir. If ref
	// is non-empty, that branch/tag/commit is checked out after
	// cloning. Used to make shared resources (workflow skills,
	// scripts) available in the workspace before AI execution.
	CloneImport(url, destDir, ref string) error

	// ListCheckRunsForRef returns failed check runs for a commit ref.
	// The second return value is true when all checks have completed.
	ListCheckRunsForRef(owner, repo, ref string) ([]models.CheckRunFailure, bool, error)

	// ListCheckRunAnnotations returns annotations for a check run.
	ListCheckRunAnnotations(owner, repo string, checkRunID int64) ([]models.CheckAnnotation, error)

	// GetFailedJobLogs returns truncated log output from failed
	// workflow job steps, keyed by job name.
	GetFailedJobLogs(owner, repo, headSHA string, maxBytesPerStep int) (map[string][]models.FailedStep, error)

	// AddPRLabel adds a label to a GitHub pull request. Creates the
	// label if it does not already exist on the repository.
	AddPRLabel(owner, repo string, number int, label string) error

	// RemovePRLabel removes a label from a GitHub pull request.
	// Returns nil if the label is not present (idempotent).
	RemovePRLabel(owner, repo string, number int, label string) error
}

// ProjectResolver maps work items to their project-specific settings.
// The implementation bridges between the bot's configuration model
// and the executor's needs.
type ProjectResolver interface {
	// ResolveProject returns the project-specific settings for the
	// given work item. Returns an error if the work item cannot be
	// mapped to a known project or repository.
	ResolveProject(workItem models.WorkItem) (*models.ProjectSettings, error)
}

// Config holds construction parameters for [Pipeline].
type Config struct {
	// BotUsername is used for branch naming
	// ("{bot-username}/{ticket-key}").
	BotUsername string

	// DefaultProvider is the AI provider used when the project
	// doesn't specify one (e.g., "claude", "gemini").
	DefaultProvider string

	// AIAPIKeys maps provider names to API key values injected
	// into the container environment (e.g., {"claude": "sk-..."}).
	AIAPIKeys map[string]string

	// ClaudeVertex holds Vertex AI authentication settings for
	// Claude. When configured, the pipeline injects Vertex-specific
	// env vars and mounts the credentials file into the container
	// instead of setting ANTHROPIC_API_KEY.
	ClaudeVertex *ClaudeVertexConfig

	// SessionTimeout is the maximum duration for an AI session
	// inside the container. Zero means no explicit timeout (only
	// the parent context controls cancellation).
	SessionTimeout time.Duration

	// IgnoredUsernames lists users whose PR comments are excluded
	// from feedback processing entirely (e.g., CI bots).
	IgnoredUsernames []string

	// KnownBotUsernames lists other bots for loop prevention.
	// Their replies to our bot's comments are excluded.
	KnownBotUsernames []string

	// MaxThreadDepth limits how many times our bot can appear in
	// a comment thread's ancestry before further comments in that
	// thread are excluded. Zero or negative disables the limit.
	MaxThreadDepth int

	// DefaultClaudeModel is the Claude model to use when the
	// repo-level config doesn't specify one (e.g., "claude-sonnet-4-6").
	// Empty means Claude Code's built-in default.
	DefaultClaudeModel string

	// DefaultGeminiModel is the Gemini model to use when the
	// repo-level config doesn't specify one (e.g., "gemini-2.5-pro").
	DefaultGeminiModel string

	// MaxRetries is the maximum retry count from the job manager.
	// Used by the feedback pipeline to post an honest "unable to
	// address" reply on the final attempt instead of failing
	// silently and looping.
	MaxRetries int

	// GeminiPricing holds per-million-token prices for computing
	// Gemini session costs from token counts.
	GeminiPricing GeminiPricing

	// IgnoredCheckNames lists check run names excluded from CI
	// failure detection (case-insensitive).
	IgnoredCheckNames []string

	// MaxCIFixAttempts limits CI fix attempts per PR. Zero
	// disables CI failure detection. Negative means unlimited.
	MaxCIFixAttempts int

	// RetryLabel is the Jira label users add to request a retry
	// after exhaustion. Included in the status comment hint.
	RetryLabel string

	// JiraUsername is the Jira account email used to filter out
	// the bot's own comments when building the issue file.
	JiraUsername string

	// MinCommentLength is the minimum character length for Jira
	// ticket comments to be included in the AI task file.
	MinCommentLength int
}

// ClaudeVertexConfig holds Vertex AI authentication settings for
// Claude Code. These are injected into the container as environment
// variables, and the credentials file is mounted read-only.
type ClaudeVertexConfig struct {
	// ProjectID is the GCP project ID
	// (env: ANTHROPIC_VERTEX_PROJECT_ID).
	ProjectID string

	// Region is the GCP region (env: CLOUD_ML_REGION).
	Region string

	// CredentialsFile is the host path to the GCP service account
	// JSON key file. Mounted read-only into the container at
	// /run/secrets/gcp-sa-key.json.
	CredentialsFile string
}
