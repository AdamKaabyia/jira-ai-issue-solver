# AGENTS.md

This file provides guidance to AIs when working with code in this repository.

## Project Overview

Jira AI Issue Solver is a Go-based service that automatically processes Jira tickets and creates GitHub pull requests with AI-generated solutions. It runs AI providers (Claude, Gemini) inside containers against cloned repositories, managing the full lifecycle from ticket detection through PR creation and review feedback handling.

## Architecture

### Package Structure

The application uses consumer-defined interfaces and clear package boundaries:

- **`tracker/`** — `IssueTracker` interface for work item operations; `jira/` sub-package adapts `services.JiraService` to this interface
- **`workspace/`** — `Manager` interface for ticket-scoped workspace lifecycle (clone, cleanup, TTL); `FSManager` implementation
- **`container/`** — `Manager` interface for container lifecycle; `Runner` (CLI executor), `Resolver` (image/config resolution), `RuntimeManager` (orchestration)
- **`taskfile/`** — `Writer` interface for generating AI task files; `MarkdownWriter` implementation; appends universal instructions and (for new tickets only) workflow from project-config overrides or repo-level files
- **`repoconfig/`** — Parses `.ai-bot/config.yaml` from target repositories for per-repo AI/container settings and repo imports
- **`projectresolver/`** — `Resolver` interface mapping ticket keys to project settings (component-to-repo, status transitions, imports)
- **`executor/`** — `Pipeline` implementing new-ticket and PR-feedback execution flows
- **`jobmanager/`** — `Coordinator` with concurrency control, retry tracking, and circuit breaker
- **`scanner/`** — `WorkItemScanner` (new tickets), `FeedbackScanner` (PR review comments), and `TriageLabelScanner` (cleans up triage labels when tickets leave the triage phase); stateless, event-driven
- **`commentfilter/`** — Shared bot-loop prevention (ignored users, known bots, thread depth limits)
- **`recovery/`** — `StartupRunner` for crash recovery (orphan container cleanup, stuck ticket reset, workspace TTL)
- **`costtracker/`** — `FileTracker` for daily AI session cost tracking with budget enforcement
- **`services/`** — Infrastructure implementations: `JiraService` (Jira REST API), `GitHubService` (GitHub App auth, Git Data API, PR operations), `GitLabService` (GitLab PAT auth, MR operations), `GitOps` (shared provider-agnostic git commands)
- **`services/hosting/`** — `Router` that dispatches VCS operations to the correct backend (GitHub or GitLab) based on owner/repo mapping from project config
- **`models/`** — Configuration (`Config`), Jira API types, domain types (`WorkItem`, `SearchCriteria`, `ProjectSettings`)

### Design Principles

- **Consumer-defined interfaces**: Each consumer declares only the methods it needs (no shared service interfaces)
- **Test doubles via func-field stubs**: `*test/` sub-packages (e.g., `executortest/stubs.go`) with assignable function fields
- **Deterministic output**: Map keys sorted when building strings/commands
- **Nil slice normalization**: Empty results return `[]T{}`, not nil
- **Stateless scanning**: Scanners derive "addressed" state from bot replies, no timestamp markers

### Configuration Model

Multi-project configuration system (`models/config.go`):

- **Project-based**: Each project has its own status transitions, component-to-repo mappings, and PR field settings
- **Ticket type-specific status transitions**: Different issue types (Bug, Story, Task) can have different workflow statuses
- **Workspaces**: Configurable base directory and TTL for ticket-scoped workspace cleanup
- **Container**: Runtime selection (podman/docker/auto), default image, resource limits
- **Guardrails**: Concurrency limits, retry limits, circuit breaker, daily cost budget, container timeout
- **Environment variable support**: All configuration via `JIRA_AI_<SECTION>_<FIELD>` env vars or YAML

Key configuration features:
- `GetProjectConfigForTicket()` retrieves the appropriate project config based on ticket key
- `StatusTransitions` maps ticket types to their workflow statuses (todo, in_progress, in_review, and optionally merged)
- **Workspaces** group one or more repos into a named working environment. A single-repo project is a workspace with one entry. Multi-repo workspaces clone all repos into subdirectories and run one AI session against the whole workspace. An optional `root_repo` URL clones a scaffold repo as the workspace root before child repos are placed inside it; the scaffold provides context files (e.g., CLAUDE.md) but is never branched, committed to, or PR'd.
- **Hosting**: Each workspace declares a `hosting` field (`"github"` or `"gitlab"`, defaults to `"github"`). The hosting router in `services/hosting/` dispatches VCS operations to the correct backend based on this setting.
- **GitLab configuration**: When any workspace uses `hosting: gitlab`, the top-level `gitlab` section must be configured with `base_url`, `access_token`, `bot_username`, and `bot_email`. Auth uses Personal Access Tokens (PAT) or Project/Group Access Tokens.
- **Profiles** bundle container, imports, instructions, and workflow settings. Repos within workspaces reference profiles by name. Profile settings override repo-level `.ai-bot/` files when set, enabling prototyping without committing to the source repo.
- `Components` maps Jira component names to workspaces (case-insensitive). `DefaultWorkspace` is used when tickets have no matching component.
- **Fork mode**: `fork_mode: true` on a project config requires fork-based contributions. Commits are pushed to the assignee's fork (looked up via `jira.assignee_to_github_username`) and PRs are created as cross-repo PRs. When disabled (default), commits go directly to the upstream repo. Missing assignee mappings in fork-mode projects apply the `fork_user_missing` failure label and skip the ticket.
- **Container resolution**: workspace-level container overrides per-repo profile containers. Multi-repo workspaces require a workspace-level container (fat container with all toolchains).
- `Imports` (per-repo profile) declares auxiliary repos to clone into the workspace; merged with repo-level imports from `.ai-bot/config.yaml`; optional `install` command runs inside the container after cloning
- `Instructions` (per-repo profile) provides universal instructions (validation commands, coding standards); appended to all task types; overrides `.ai-bot/instructions.md` when set
- `NewTicketWorkflow` (per-repo profile) provides workflow instructions appended only to new-ticket task files; overrides `.ai-bot/new-ticket-workflow.md` when set
- `FeedbackWorkflow` (per-repo profile) provides workflow instructions appended only to feedback task files; overrides `.ai-bot/feedback-workflow.md` when set

### Workflow

1. **Ticket Discovery**: `WorkItemScanner` polls for tickets in "todo" status via the `IssueTracker` interface
2. **Job Submission**: Scanner submits jobs to `Coordinator`, which enforces concurrency, retry, and circuit breaker limits
3. **Execution Pipeline** (`executor.Pipeline`):
   - Resolves project config and maps ticket component to workspace
   - Creates/reuses a workspace (single-repo clone or multi-repo subdirectory layout)
   - Creates branches in each repo
   - Loads repo config (`.ai-bot/config.yaml`) per repo and clones any declared imports
   - Generates a task file describing the work (per-repo instructions sections for multi-repo)
   - Resolves container image (workspace-level for multi-repo, profile for single-repo)
   - Starts a container, runs import install commands (if configured), then runs the AI provider
   - Reads AI-generated PR description (`.ai-session/pr.md`) if present; falls back to Jira-derived content
   - For single-repo: commits changes and creates one PR
   - For multi-repo: fans out commit + PR creation per repo with changes (N repos → up to N PRs)
   - Transitions the ticket through configured statuses and posts PR link(s)
4. **PR Feedback Processing**: `FeedbackScanner` monitors "in review" tickets, checks all repos for PRs with unaddressed review comments (filtering bots and ignored users), and submits feedback jobs through the same `Coordinator` → `Pipeline` path. Multi-repo feedback aggregates comments across repos' PRs into one AI session, then fans out commits and replies.
5. **Crash Recovery**: On startup, `StartupRunner` cleans up orphan containers, resets stuck "in progress" tickets, and purges expired workspaces

### Bot-Loop Prevention

Configurable via `github.known_bot_usernames`, `github.ignored_usernames`, and `github.max_thread_depth`:
- **Known bots**: Comments are processed initially but loop prevention stops bot-to-bot reply chains
- **Ignored usernames**: Comments completely skipped (for CI bots like packit-as-a-service[bot])
- **Thread depth**: Maximum bot replies per thread (default: 5)

### Skip PR Label

Configurable via `github.skip_pr_label` (default: `ai-bot-skip`). When this GitHub label is present on a PR, the bot skips all processing for that PR — no review comment handling, no CI failure detection, no merge conflict resolution. Removing the label re-enables processing on the next scan cycle. Set to empty string to disable the feature. The check is fail-open: API errors are logged and the PR is processed normally.

### Failure-State Labels

Optional per-project Jira labels (`failure_labels` in project config) that mark ticket failure states for dashboard visibility. All four are mutually exclusive by lifecycle; empty string disables the label:
- **`ci_failing`**: Applied when the bot's PR exists but CI checks are failing. Removed when CI passes or the bot pushes new code.
- **`rejected`**: Applied when a human reviewer closes the PR without merging.
- **`blocked`**: Applied when the bot cannot proceed (workspace errors, infra failures). Applied by the executor on pipeline failure; removed on success.
- **`fork_user_missing`**: Applied when a fork-mode project cannot resolve the ticket assignee's GitHub username from `jira.assignee_to_github_username`. Cleared on successful PR creation.

Label management is best-effort — failures are logged but never block core operations. The feedback scanner handles `ci_failing` and `rejected` detection; the executor handles `blocked` and `fork_user_missing`.

### Lifecycle Labels

Optional per-project Jira labels (`lifecycle_labels` in project config) that track ticket progression through the autofix pipeline. Labels are mutually exclusive: setting one removes the others. Empty string disables the label:
- **`queued`**: Set externally (e.g., by a triage bot) to indicate the ticket is waiting. This bot never sets it, but removes it when applying `review`.
- **`review`**: Applied by the executor when a PR is created and the ticket transitions to "in review".
- **`merged`**: Applied by the feedback scanner when all repos' PRs are merged. For multi-repo workspaces, requires every repo's PR to be merged.

When the `merged` label is applied, the scanner also transitions the ticket to the configured `merged` status (e.g., "MODIFIED") if set in `status_transitions`. The `merged` status field is optional; omitting it disables the transition.

### PR Validation Labels

Configurable GitHub PR labels (`pr_validation_labels` in project config) applied when the AI session reports a problem or the bot cannot proceed. Labels are mutually exclusive: at most one is set on a PR at any time. Empty strings disable the corresponding label. Suggested values: `ai-validation-failed`, `ai-nonzero-exit`, and `ai-budget-exceeded`.
- **`validation_failed`**: Applied when the AI session explicitly reports `validation_passed: false`.
- **`nonzero_exit`**: Applied when the AI container exits with a non-zero code (and validation was not explicitly reported as failed).
- **`cost_cap_exceeded`**: Applied when the per-ticket AI session cost cap has been reached. **Defaults to `"ai-budget-exceeded"` when not configured** (the only label with a non-empty default). Set to `""` to disable. Reflects live state: re-applied if removed while the condition holds, removed when the bot successfully runs again (e.g., after the cap is raised).

Labels are applied when code is pushed (both new-ticket and feedback paths) and cleared when a subsequent push passes validation. When the AI produces no code changes, labels are left unchanged. Label management is best-effort — failures are logged but never block core operations.

### Security Features

- **Security level redaction**: Tickets with security levels get redacted PR titles/descriptions
- **SSH key signing**: Optional commit signing via SSH keys (`github.ssh_key_path`)
- **Container isolation**: AI runs inside containers with configurable resource limits
- **Cost budget**: Daily AI session cost tracking with automatic pausing

## Common Development Commands

### Running the Application

```bash
# Using config file
go run main.go -config config.yaml

# Using environment variables (container mode)
export JIRA_AI_JIRA_BASE_URL=...
export JIRA_AI_JIRA_USERNAME=...
# ... (see config.example.yaml for all options)
go run main.go
```

### Testing

```bash
# Run all tests
go test ./...

# Run tests with verbose output and race detection
go test -v -race ./...

# Run tests for a specific package
go test -v ./executor

# Run a specific test
go test -v ./tracker/jira -run TestAdapter_SearchWorkItems
```

### Building

```bash
# Build the binary
go build -o jira-ai-issue-solver main.go

# Build container image
make build
```

### Linting and Formatting

```bash
# Auto-format code (gofmt + gci import ordering)
make fmt

# Run linter (golangci-lint)
make lint
```

### Debugging

```bash
# Interactive debug session
./debug.sh

# Debug with Delve directly
make debug

# Debug tests
make debug-tests

# VS Code debugging: Press F5 and select configuration
```

See docs/debugging.md for breakpoint locations and common troubleshooting.

### Container Operations

```bash
# Build and run locally
make build
make run

# View logs
make logs

# Stop and clean up
make stop
make clean
```

## Important Implementation Details

### Multi-Project Configuration

When adding new projects or modifying configuration:
- Each project requires at least one ticket type with complete status transitions (todo, in_progress, in_review)
- Project keys are matched case-insensitively via `GetProjectConfigForTicket()`
- Component names in `component_to_repo` are matched case-insensitively (viper lowercases YAML map keys)
- Status names are case-sensitive and must exactly match the Jira workflow status names

### PR URL Handling

Two modes for storing PR URLs:
1. **Designated field mode**: If `git_pull_request_field_name` is configured, updates that custom Jira field
2. **Comment mode**: If no designated field, adds structured comment `[AI-BOT-PR] <url>` for easy extraction

### Error Handling

Error handling can be configured per project:
- `disable_error_comments`: If true, errors are logged but not posted to Jira
- Failed ticket processing reverts to original status (no status change on error)
- All errors logged with structured logging (zap) including ticket key and context

## Code Patterns

### Service Initialization

Services are initialized in `main.go` with dependency injection. The startup sequence:
1. Load config and create logger
2. Create infrastructure services (JiraService, GitHubService)
3. Create adapters (IssueTracker, ProjectResolver, WorkspaceManager, ContainerManager)
4. Create cost tracker
5. Create executor pipeline
6. Create job coordinator
7. Run crash recovery
8. Start scanners and HTTP server

### Configuration Validation

All configuration is validated in `models.Config.validate()`:
- Required fields checked (Jira credentials, GitHub App auth, bot username, workspace base dir)
- Status transitions validated per ticket type
- At least one project must be configured
- Each project must have at least one ticket type and component mapping

### Logging

Use structured logging with zap throughout:
```go
logger.Info("Processing ticket", zap.String("ticket", ticketKey))
logger.Error("Failed to process", zap.String("ticket", ticketKey), zap.Error(err))
```

Log levels: debug, info, warn, error (configured via `logging.level`)

### Test Requirements

**Every code change must include corresponding unit tests.** Code and tests are always committed together — never defer test writing to a later step.

When making changes:
- **New functions/methods**: Add tests covering the happy path, error cases, and edge cases
- **Changed behavior**: Update existing tests to cover the new behavior; add new tests for new code paths
- **Interface changes**: Update all stubs/mocks and any tests that use the old signature
- **Bug fixes**: Add a test that reproduces the bug first, then verify it passes with the fix

Run `go test -v -race ./...` after every change and fix failures before continuing.

### Testing with Stubs

Each package provides test doubles in a `*test/` sub-package with func-field stubs:
```go
stub := &executortest.StubPipeline{
    ExecuteFunc: func(ctx context.Context, job jobmanager.Job) (jobmanager.Result, error) {
        return jobmanager.Result{}, nil
    },
}
```

## Environment Variable Naming

Environment variables follow the pattern `JIRA_AI_<SECTION>_<FIELD>`:
- `JIRA_AI_JIRA_BASE_URL` -> `jira.base_url`
- `JIRA_AI_GITHUB_APP_ID` -> `github.app_id`
- `JIRA_AI_GITLAB_BASE_URL` -> `gitlab.base_url`
- `JIRA_AI_GITLAB_ACCESS_TOKEN` -> `gitlab.access_token`
- `JIRA_AI_AI_PROVIDER` -> `ai_provider`
- `JIRA_AI_WORKSPACES_BASE_DIR` -> `workspaces.base_dir`
- `JIRA_AI_GUARDRAILS_MAX_CONCURRENT_JOBS` -> `guardrails.max_concurrent_jobs`

See `models/config.go` LoadConfig() for complete environment variable binding.

## File Structure Notes

- `main.go`: Application entry point, service wiring, HTTP server, graceful shutdown
- `models/`: Configuration and data structures (Jira types, domain types)
- `services/`: Infrastructure service implementations (Jira REST API, GitHub App/Git Data API, GitLab PAT/MR API, shared GitOps)
- `services/hosting/`: Multi-provider routing (dispatches to GitHub or GitLab based on workspace config)
- `tracker/`: IssueTracker interface and Jira adapter
- `workspace/`: Ticket-scoped workspace management
- `container/`: Container runtime detection, image resolution, lifecycle management
- `executor/`: New-ticket and PR-feedback execution pipelines
- `jobmanager/`: Concurrency control, retry tracking, circuit breaker
- `scanner/`: Polling-based ticket and feedback discovery
- `commentfilter/`: Bot-loop prevention logic
- `recovery/`: Crash recovery and startup cleanup
- `costtracker/`: Daily AI cost tracking
- `projectresolver/`: Ticket-to-project-config mapping
- `taskfile/`: AI task file generation (universal instructions + new-ticket workflow from project-config overrides or repo files)
- `repoconfig/`: Per-repo `.ai-bot/config.yaml` parsing (PR, AI, imports)
- `config.example.yaml`: Complete configuration reference with comments
- `docs/`: Architecture, debugging, setup guides, and [repo-level configuration](docs/repo-configuration.md)
