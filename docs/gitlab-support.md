# GitLab Support

Jira AI Issue Solver watches Jira tickets, runs AI agents (Claude or Gemini) in ephemeral containers against your codebase, and creates pull requests or merge requests with the results. It handles the full lifecycle: ticket discovery, workspace management, container orchestration, PR/MR creation, and review feedback processing. This guide covers deploying and using the bot with GitLab-hosted repositories.

## Architecture

```mermaid
flowchart TD
    Jira[Jira Tickets] -->|polls| WIS[WorkItemScanner]
    WIS -->|submits job| Coord[Coordinator]
    Coord -->|enforces limits| Pipeline[Executor Pipeline]
    Pipeline -->|clones repo| WS[Workspace Manager]
    Pipeline -->|runs AI| Container[Container Manager]
    Pipeline -->|creates MR/PR| Router[Hosting Router]
    Router -->|"hosting: github"| GH[GitHubService]
    Router -->|"hosting: gitlab"| GL[GitLabService]
    GL -->|PAT auth| GitLab[GitLab Instance]
    GH -->|App auth| GitHub[GitHub]

    FS[FeedbackScanner] -->|review comments| Coord
    MS[MergeScanner] -->|branch updates| Coord
```

### Key Packages

| Package | Role |
|---------|------|
| `scanner/` | Polls Jira for new tickets and monitors PRs/MRs for review comments |
| `jobmanager/` | Enforces concurrency limits, retries, circuit breaker, and cost budget |
| `executor/` | Orchestrates the full pipeline: clone, branch, AI session, commit, create MR |
| `workspace/` | Manages per-ticket workspace directories (clone, TTL, cleanup) |
| `container/` | Detects runtime (podman/docker), resolves images, runs AI sessions |
| `services/` | Jira REST API, GitHub App API, GitLab PAT API, shared git commands |
| `services/hosting/` | Routes VCS operations to the correct backend per workspace |
| `models/` | Configuration loading, validation, and domain types |
| `recovery/` | Startup crash recovery (orphan cleanup, stuck ticket reset) |

### How the Hosting Router Works

Each workspace in the project config declares a `hosting` field (`"github"` or `"gitlab"`). At startup, `main.go` builds a map of `owner/repo` to provider. The hosting router satisfies all consumer interfaces and dispatches each method call to the correct backend:

- **API-based calls** (e.g., `CreatePR`, `GetPRComments`) are routed by owner/repo.
- **Directory-based calls** (e.g., `CreateBranch`, `HasChanges`) are routed via a workspace registry populated during `CloneRepository`.
- **Fallback**: repos not explicitly mapped go to the default provider (GitHub if configured, otherwise GitLab).

## GitLab Configuration

### 1. Create a GitLab Access Token

Create a Personal Access Token (PAT) or a Project/Group Access Token on your GitLab instance:

1. Go to **Settings > Access Tokens** (user-level or project-level)
2. Set a descriptive name (e.g., `jira-ai-bot`)
3. Select scopes: `api`, `read_repository`, `write_repository`
4. Copy the token (starts with `glpat-`)

For self-managed GitLab instances, project/group access tokens are preferred over personal tokens because they survive employee offboarding.

### 2. Configure the `gitlab:` Block

Add to your `config.yaml`:

```yaml
gitlab:
  base_url: "https://gitlab.example.com"       # Your GitLab instance URL
  access_token: "glpat-xxxxxxxxxxxxxxxxxxxx"    # PAT or project token
  bot_username: "ai-bot"                        # Git commit author name
  bot_email: "ai-bot@example.com"              # Git commit author email
  mr_label: "ai-mr"                            # Label applied to bot MRs
  skip_mr_label: "ai-bot-skip"                 # Label to skip processing an MR
  max_thread_depth: 5                          # Max bot replies per thread
  # ssh_key_path: "/path/to/key"              # Optional: SSH signing
  # known_bot_usernames: ["project_bot"]      # Prevent bot-to-bot loops
  # ignored_usernames: []                     # Comments to completely skip
```

### 3. Set Workspace Hosting to GitLab

In your project configuration under `jira.projects[].workspaces`, set `hosting: gitlab`:

```yaml
jira:
  projects:
    - project_keys: ["MYPROJECT"]
      status_transitions:
        Bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        backend:
          hosting: gitlab                       # <-- routes to GitLab
          repos:
            - name: backend
              url: https://gitlab.example.com/org/backend.git
              profile: default
      profiles:
        default:
          container:
            image: "registry.example.com/team/dev-image:latest"
```

### 4. Environment Variables

All GitLab config fields can be set via environment variables:

| Variable | Config equivalent |
|----------|-------------------|
| `JIRA_AI_GITLAB_BASE_URL` | `gitlab.base_url` |
| `JIRA_AI_GITLAB_ACCESS_TOKEN` | `gitlab.access_token` |
| `JIRA_AI_GITLAB_BOT_USERNAME` | `gitlab.bot_username` |
| `JIRA_AI_GITLAB_BOT_EMAIL` | `gitlab.bot_email` |
| `JIRA_AI_GITLAB_MR_LABEL` | `gitlab.mr_label` |
| `JIRA_AI_GITLAB_SSH_KEY_PATH` | `gitlab.ssh_key_path` |
| `JIRA_AI_GITLAB_SKIP_MR_LABEL` | `gitlab.skip_mr_label` |
| `JIRA_AI_GITLAB_MAX_THREAD_DEPTH` | `gitlab.max_thread_depth` |

## Deployment Guide

### Prerequisites

- **Jira**: API token with read/write access to your project(s)
- **GitLab**: Personal Access Token with `api` + `write_repository` scopes
- **AI Provider**: Claude API key (or Vertex AI credentials) or Gemini API key
- **Container Runtime**: podman or docker (for running AI sessions)
- **AI Dev Container Image**: a container image with your project's toolchain (compilers, linters, dependencies)

### Step 1: Create Configuration

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml`:
- Fill in `jira.*` credentials
- Fill in `gitlab.*` credentials
- Set `ai_provider` and the corresponding API key
- Configure at least one project with `hosting: gitlab` workspaces
- Set `workspaces.base_dir` to a writable directory

### Step 2: Build and Run

**Option A: Run directly**

```bash
go build -o jira-ai-issue-solver main.go
./jira-ai-issue-solver -config config.yaml
```

**Option B: Container deployment**

```bash
make build
make run
```

**Option C: Podman Compose**

```bash
podman-compose -f podman-compose.yml up -d
```

### Step 3: Verify

```bash
curl http://localhost:8080/health
# Should return: OK
```

Check the logs for successful scanner startup:

```
INFO    Starting work item scanner
INFO    Starting feedback scanner
INFO    Starting merge scanner
```

### Step 4: Create a Test Ticket

1. Create a Jira ticket in your configured project
2. Set the ticket type to one with configured status transitions (e.g., Bug)
3. Set the status to the configured `todo` value (e.g., "To Do")
4. Assign a component that maps to your GitLab workspace (or use `default_workspace`)

The bot will:
1. Pick up the ticket within one poll cycle (default: 5 minutes)
2. Transition it to "In Progress"
3. Clone the repo, run the AI agent in a container
4. Push a branch and create a merge request on GitLab
5. Transition the ticket to "In Review" and post the MR link

## User Workflow

### Day-to-Day Usage

```mermaid
sequenceDiagram
    participant Dev as Developer
    participant Jira as Jira
    participant Bot as AI Bot
    participant GL as GitLab

    Dev->>Jira: Create ticket (Bug/Task/Story)
    Dev->>Jira: Set status to "To Do"
    Bot->>Jira: Picks up ticket (polling)
    Bot->>Jira: Transitions to "In Progress"
    Bot->>GL: Clones repo, creates branch
    Bot->>Bot: Runs AI in container
    Bot->>GL: Pushes commits, creates MR
    Bot->>Jira: Transitions to "In Review", posts MR link
    Dev->>GL: Reviews MR, leaves comments
    Bot->>GL: Detects comments, responds with fixes
    Dev->>GL: Approves and merges MR
    Bot->>Jira: Transitions to "Merged" (if configured)
```

### Interacting with the Bot

**The bot responds to MR review comments automatically.** Leave comments on specific lines or as general notes on the MR, and the bot will:
1. Detect the unaddressed comment on its next poll cycle
2. Run another AI session with the review context
3. Push new commits addressing the feedback
4. Reply to the comment thread

**Skipping a merge request:** Add the `ai-bot-skip` label (configurable via `gitlab.skip_mr_label`) to any MR to prevent the bot from processing it.

**Re-triggering after rejection:** If a reviewer closes the MR without merging, the bot applies the `rejected` failure label (if configured). Reopen or recreate the ticket to retry.

### Jira Commands (via ticket status)

The bot is driven by Jira ticket status, not comments:

| Action | How |
|--------|-----|
| Start processing | Set ticket status to the configured `todo` value |
| Stop processing | Transition ticket away from `todo`/`in_progress` |
| Retry after failure | Reset ticket status back to `todo` |

## Mixed GitHub + GitLab Deployment

A single bot instance can serve both GitHub and GitLab repositories simultaneously. Each workspace declares its hosting provider independently:

```yaml
workspaces:
  github-frontend:
    # hosting defaults to "github" when omitted
    repos:
      - name: frontend
        url: https://github.com/org/frontend.git
        profile: node-dev

  gitlab-backend:
    hosting: gitlab
    repos:
      - name: backend
        url: https://gitlab.example.com/org/backend.git
        profile: go-dev
```

The hosting router dispatches operations to the correct provider based on repository ownership. Both the `github:` and `gitlab:` config sections must be present when both providers are in use.

## Differences from GitHub Mode

| Aspect | GitHub | GitLab |
|--------|--------|--------|
| Authentication | GitHub App (JWT + installation tokens) | Personal/Project Access Token |
| Commit strategy | Git Data API (create blob, tree, commit, update ref) | Local `git commit` + `git push` |
| PR/MR creation | `POST /repos/:owner/:repo/pulls` | `POST /projects/:id/merge_requests` |
| Review comments | PR review comments API (file-level threading) | MR discussion notes (position-based) |
| CI status | Check Runs API | Pipeline + Jobs API |
| Fork support | Full (cross-repo PRs via `source_project_id`) | Limited (same-project branches) |
| Label events | Timeline events API (tracks label add/remove) | Not available (label removal time unknown) |
| Merge status | Computed async, polled with retries | `has_conflicts` + `merge_status` fields |
| Bot identity | App name + `[bot]` suffix | PAT owner or project token name |

### Commit Strategy Differences

**GitHub** uses the Git Data API to create "verified" commits server-side without ever running `git push`. This produces commits attributed to the GitHub App with a "Verified" badge.

**GitLab** commits locally and pushes with the PAT embedded in the remote URL. Commits are attributed to the token owner. SSH signing (optional) provides commit verification.

### What This Means in Practice

- GitLab MRs work identically to GitHub PRs from the user's perspective
- The bot creates branches, commits code, opens MRs, and responds to review comments
- The main difference is the authentication model: one PAT vs a GitHub App installation per org
- GitLab's simpler auth model means faster setup (no App registration, no webhook configuration)

## Contributing Upstream

This section documents the strategy for contributing GitLab support back to the upstream `flightctl/jira-ai-issue-solver` repository.

### PR Split Strategy

The changeset is split into two PRs for easier review:

**PR 1: Extract shared GitOps (pure refactor)**

Files:
- `services/gitops.go` — provider-agnostic git operations extracted from `GitHubServiceImpl`
- `services/gitops_test.go` — tests for shared operations
- `services/github.go` — methods now delegate to `GitOps` (thin wrappers)

This PR has **zero behavior change**. All existing tests pass without modification. It prepares the codebase for multi-provider support by extracting operations that both GitHub and GitLab share: `CreateBranch`, `SwitchBranch`, `HasChanges`, `StripRemoteAuth`, `FetchRemote`, `SyncWithRemote`, `MergeBase`, `CloneImport`, `StageAndCommitLocal`.

**PR 2: Add GitLab hosting support (builds on PR 1)**

Files:
- `services/gitlab.go` + `services/gitlab_test.go` — GitLab service implementation
- `services/hosting/router.go` + `services/hosting/router_test.go` — multi-provider router
- `models/config.go` — `GitLab` config struct, workspace `Hosting` field, conditional validation
- `main.go` — conditional service creation, router wiring
- `config.example.yaml` — GitLab configuration section
- `AGENTS.md` — documentation updates
- `docs/gitlab-support.md` — this guide

### PR Description Template

```markdown
## Summary

Add per-project GitLab hosting alongside existing GitHub support. Each workspace
declares its hosting provider (`github` or `gitlab`), and a hosting router
dispatches VCS operations to the correct backend.

## Design

- **Hosting router** (`services/hosting/router.go`): satisfies all consumer
  interfaces, dispatches by owner/repo for API calls and by workspace directory
  for local git operations. Falls back to the default provider for unmapped repos.
- **GitLab service** (`services/gitlab.go`): uses PAT auth and the GitLab REST
  API v4. Commits via local `git push` (vs GitHub's Git Data API). No external
  dependencies beyond stdlib `net/http`.
- **Shared GitOps** (`services/gitops.go`): provider-agnostic git commands
  extracted from GitHubServiceImpl. Both providers embed this struct.
- **Conditional validation**: GitHub credentials are only required when
  workspaces use `hosting: github`; same for GitLab.

## Backward Compatibility

- Zero changes to existing GitHub-only deployments
- GitHub validation is only enforced when GitHub hosting is in use
- Default hosting is `github` (no config change needed for existing users)
- All existing tests pass without modification

## Test Coverage

- `services/gitlab_test.go`: HTTP mock tests for MR creation, comments, labels, CI
- `services/hosting/router_test.go`: dispatch logic, fallback, workspace registry
- `services/gitops_test.go`: shared git operations (real git commands on temp repos)

## Configuration

Workspace-level `hosting` field:

```yaml
workspaces:
  my-gitlab-project:
    hosting: gitlab
    repos:
      - name: backend
        url: https://gitlab.example.com/org/backend.git
        profile: default
```
```

### What to Include

- Motivation: teams using GitLab can now adopt the bot without a mirror/sync layer
- The architectural fit: consumer-defined interfaces already isolate VCS concerns
- No new external dependencies (pure stdlib HTTP client, no go-gitlab SDK)
- Test results: `go test ./...` passes

### What NOT to Include

- Deployment-specific details (container images, internal GitLab URLs, team names)
- References to specific internal projects or Jira boards
- Fork/licensing history (resolved privately before the PR)
- Future plans (triage, pushback, webhooks) — these are separate features
