# GitLab Hosting Support -- Full Code Review

## Executive Summary

| Metric | Value |
|--------|-------|
| **Commit** | `5c5d2fc feat: add per-project GitLab hosting support` |
| **Files changed** | 12 |
| **Lines added** | +3,139 |
| **Lines removed** | -425 |
| **Net new code** | +2,714 |
| **New files** | 6 (`gitops.go`, `gitlab.go`, `gitlab_test.go`, `gitops_test.go`, `router.go`, `router_test.go`, `gitlab-support.md`) |
| **Modified files** | 5 (`github.go`, `config.go`, `main.go`, `config.example.yaml`, `AGENTS.md`) |

### Changed Files

| File | Lines | Nature |
|------|-------|--------|
| `services/gitops.go` | +476 | Extracted shared git operations |
| `services/gitops_test.go` | +126 | Tests for shared git operations |
| `services/gitlab.go` | +935 | GitLab API implementation |
| `services/gitlab_test.go` | +346 | GitLab service tests |
| `services/hosting/router.go` | +302 | Multi-provider dispatch layer |
| `services/hosting/router_test.go` | +282 | Router dispatch tests |
| `services/github.go` | -406 | Replaced with one-liner delegates |
| `models/config.go` | +152 / -57 | GitLab config, conditional validation |
| `main.go` | +68 / -1 | Conditional service creation, router wiring |
| `config.example.yaml` | +82 | GitLab config section |
| `docs/gitlab-support.md` | +379 | Deployment guide |
| `AGENTS.md` | +10 | Updated package docs |

---

## Why

The application was hard-coupled to GitHub as its sole VCS backend. All git and PR operations flowed through `GitHubServiceImpl`, which mixed two concerns:

1. **Provider-agnostic local git commands** -- branch, commit, fetch, merge, strip auth, etc.
2. **GitHub-specific API calls** -- Git Data API for verified commits, PR creation, review comments, check runs, etc.

Organizations whose repos live on GitLab (including self-managed instances like `gitlab.cee.redhat.com`) could not use the bot without mirroring to GitHub. This change adds GitLab as a second hosting provider so teams can point the bot at GitLab repos natively.

**Key constraint**: zero breaking changes for existing GitHub-only deployments. No config migration, no new required fields unless GitLab hosting is explicitly used.

---

## Architecture Overview

### System-Level Flow

```mermaid
flowchart TD
    Jira[Jira Tickets] -->|polls| WIS[WorkItemScanner]
    WIS -->|submits job| Coord[Coordinator]
    Coord -->|enforces limits| Pipeline[Executor Pipeline]
    Pipeline -->|clones repo| WS[Workspace Manager]
    Pipeline -->|runs AI| Container[Container Manager]
    Pipeline -->|"creates PR/MR"| Router[Hosting Router]
    Router -->|"hosting: github"| GH[GitHubServiceImpl]
    Router -->|"hosting: gitlab"| GL[GitLabServiceImpl]
    GH -->|App auth| GitHub[GitHub]
    GL -->|PAT auth| GitLab[GitLab Instance]
    GH -->|delegates| GitOps[GitOps shared]
    GL -->|delegates| GitOps
    FS[FeedbackScanner] -->|review comments| Coord
```

### Three-Layer Architecture

```mermaid
flowchart LR
    subgraph layer1 [Layer 1: Shared GitOps]
        GitOps["services/gitops.go\n(476 lines)\nProvider-agnostic git commands"]
    end
    subgraph layer2 [Layer 2: Provider Implementations]
        GH["services/github.go\n(delegates to GitOps)\nGitHub API + Git Data API"]
        GL["services/gitlab.go\n(935 lines)\nGitLab REST API v4 + git push"]
    end
    subgraph layer3 [Layer 3: Router + Wiring]
        Router["services/hosting/router.go\n(302 lines)\nDispatches by owner/repo"]
        Main["main.go\nConditional creation + wiring"]
        Config["models/config.go\nHosting field + validation"]
    end
    GH --> GitOps
    GL --> GitOps
    Router --> GH
    Router --> GL
    Main --> Router
    Config --> Main
```

---

## What Changed

### Layer 1 -- Extract Shared GitOps (Pure Refactor)

Nine methods were extracted from `GitHubServiceImpl` into a new `GitOps` struct:

| Method | What it does |
|--------|-------------|
| `CreateBranch` | Fetch, checkout base, reset to latest, delete old branch if exists, create new |
| `SwitchBranch` | Reset dirty state, fetch, checkout existing branch |
| `HasChanges` | Check working tree + unpushed commits |
| `StripRemoteAuth` | Remove credentials from origin URL |
| `FetchRemote` | `git fetch origin` |
| `SyncWithRemote` | Preserve excluded dirs, fetch, hard reset, restore dirs |
| `MergeBase` | Merge a base branch, handle untracked blockers and conflicts |
| `CloneImport` | Shallow clone an auxiliary repo |
| `StageAndCommitLocal` | `git add -A` + `git commit` to normalize AI output |

Plus helpers: `ConfigureUser`, `ConfigureSSHSigning`, `SetRemoteURL`, `preserveExcludedDirs`, `restoreExcludedDirs`, `listConflictFiles`, `runMerge`.

`GitHubServiceImpl` methods are now one-liner delegates:

```go
func (s *GitHubServiceImpl) CreateBranch(directory, branchName, baseBranch string) error {
    return s.gitOps.CreateBranch(directory, branchName, baseBranch)
}
```

**Zero behavior change.** All existing tests pass without modification.

### Layer 2 -- GitLab Service Implementation

`GitLabServiceImpl` implements the same `Provider` interface as `GitHubServiceImpl`. Uses plain `net/http` with GitLab REST API v4 -- no external SDK dependency.

### Layer 3 -- Hosting Router + Wiring

`Router` satisfies all consumer-defined interfaces and dispatches each method call to the correct backend. `main.go` conditionally creates services and builds the router at startup.

---

## File-by-File Walkthrough

---

### `services/gitops.go` (476 lines -- NEW)

Provider-agnostic git operations shared by both GitHub and GitLab services.

#### Struct

```go
type GitOps struct {
    executor models.CommandExecutor  // func with exec.Command signature, injectable for tests
    logger   *zap.Logger             // structured logging
}
```

#### `NewGitOps(executor, logger)` -- line 33

Constructor. Stores the executor and logger.

#### `CreateBranch(directory, branchName, baseBranch)` -- line 43

Creates a fresh feature branch from a base branch. Steps:

1. `git reset --hard HEAD` -- clean leftover dirty state from prior failed runs. Non-fatal (warns on failure).
2. `git fetch origin` -- pull latest refs from remote.
3. `git checkout <baseBranch>` -- switch to the base (e.g., `main`).
4. `git reset --hard origin/<baseBranch>` -- force-align local with remote.
5. `git show-ref --verify --quiet refs/heads/<branchName>` -- check if branch exists locally. If yes, `git branch -D` to delete it (handles retry scenarios).
6. `git checkout -b <branchName>` -- create and switch to the new branch.

#### `SwitchBranch(directory, branchName)` -- line 93

Simplified branch switch for workspace reuse:

1. `git reset --hard HEAD` -- clean dirty state.
2. `git fetch origin` -- get latest refs.
3. `git checkout <branchName>` -- switch to existing branch.

#### `HasChanges(directory, baseBranch)` -- line 120

Two-part check returning true if there is anything to push:

1. `hasWorkingTreeChanges()` -- runs `git status --porcelain`. Any output = uncommitted changes.
2. `hasUnpushedCommits()` -- compares HEAD against the remote ref to find local-only commits.

#### `hasWorkingTreeChanges(directory, fn)` -- line 134

`git status --porcelain` -- machine-readable status. `cmd.hasStdout()` returns true if there is any output (modified/added/deleted files).

#### `hasUnpushedCommits(directory, baseBranch, fn)` -- line 143

1. Check if origin remote exists (if not, no comparison possible).
2. Get current branch name via `git rev-parse --abbrev-ref HEAD`.
3. Check if `origin/<currentBranch>` exists. If the branch was never pushed, fall back to `origin/<baseBranch>`.
4. `git log origin/<ref>..HEAD --oneline` -- any output means unpushed commits.

#### `StageAndCommitLocal(directory, fn)` -- line 185

Normalizes the three possible states an AI session can leave behind (committed, staged, or unstaged) into a single committed state:

1. `git status --porcelain` -- if clean, return early.
2. `git add -A` -- stage everything.
3. `git commit -m "AI changes (local only)"` -- temporary message, replaced later via `--amend`.

#### `StripRemoteAuth(directory)` -- line 214

Security operation before mounting workspace into AI container:

1. `git remote get-url origin` -- read current URL (e.g., `https://oauth2:token@gitlab.com/org/repo.git`).
2. Parse with `url.Parse`, set `parsed.User = nil` to strip credentials.
3. `git remote set-url origin <cleaned>` -- apply.

#### `FetchRemote(directory)` -- line 239

Runs `git fetch origin`. Wrapped for reuse and logging.

#### `SyncWithRemote(directory, branch, importExcludes)` -- line 255

Reconciles local workspace with remote after an API-created or pushed commit:

1. Move import artifact directories to `<name>.preserve` (they only exist locally).
2. `git fetch origin` + `git reset --hard origin/<branch>`.
3. Move preserved directories back.

#### `MergeBase(dir, branch, fetchURL)` -- line 282

Merges a base branch into the current branch (merge conflict resolution):

1. If `fetchURL` is set (fork workflows), fetch from that URL and merge `FETCH_HEAD`. Otherwise fetch from origin and merge `origin/<branch>`.
2. If merge fails due to untracked files blocking it, parse those paths from git output, `git clean -f -- <files>`, retry.
3. If merge still fails, check `git status --porcelain` for conflict markers (`UU`, `AA`, `DD`, `AU`, `UA`, `DU`, `UD`). Return `ErrMergeConflict` with file list if found.

#### `runMerge(dir, mergeRef)` -- line 318

Helper: `git merge --no-edit <ref>`. Returns combined output.

#### `listConflictFiles(dir)` -- line 325

Parses `git status --porcelain` for unmerged entries. Returns `[]string{}` (not nil) per project convention.

#### `CloneImport(repoURL, destDir, ref)` -- line 352

Shallow clone (`--depth 1`) of an auxiliary reference repo. If `ref` is set, adds `--branch <ref>`.

#### `preserveExcludedDirs(directory, excludes, fn)` -- line 375

Moves directories to `<name>.preserve` before a hard reset. Returns list of preserved names.

#### `restoreExcludedDirs(directory, preserved, fn)` -- line 396

Moves `<name>.preserve` back to `<name>` after reset.

#### `SetRemoteURL(directory, remoteURL)` -- line 408

Runs `git remote set-url origin <url>`. Used by `RestoreRemoteAuth` in both providers.

#### `ConfigureUser(directory, name, email)` -- line 419

Sets `git config user.name` and `git config user.email` in the workspace.

#### `ConfigureSSHSigning(directory, sshKeyPath)` -- line 440

If SSH key path is set and file exists, configures three git config values:
- `gpg.format = ssh`
- `user.signingkey = <path>`
- `commit.gpgsign = true`

No-op if path is empty or file doesn't exist.

---

### `services/gitlab.go` (936 lines -- NEW)

Full GitLab API v4 implementation. Uses plain `net/http` -- no external SDK.

#### Struct

```go
type GitLabServiceImpl struct {
    config   *models.Config
    client   *http.Client            // 30-second timeout
    executor models.CommandExecutor
    logger   *zap.Logger
    gitOps   *GitOps                 // shared local git operations
}
```

#### `NewGitLabService(config, logger, executor...)` -- line 33

Constructor. Defaults executor to `exec.Command`. Accepts optional override for tests.

#### Delegate Methods (lines 50-80)

Eight one-liners forwarding to `gitOps`: `CreateBranch`, `SwitchBranch`, `HasChanges`, `StripRemoteAuth`, `FetchRemote`, `SyncWithRemote`, `MergeBase`, `CloneImport`.

#### `RestoreRemoteAuth(directory, owner, repo)` -- line 84

Builds authenticated URL: `https://oauth2:<token>@<host>/<owner>/<repo>.git`. Calls `gitOps.SetRemoteURL` to apply.

> **Note**: Line 88 contains `strings.Replace(baseURL, "https://", "https://", 1)` which replaces a string with itself -- a no-op. The URL construction works correctly regardless.

#### `SyncFork(forkOwner, repo, branch)` -- line 96

No-op. GitLab doesn't have GitHub's "merge upstream" API. The clone fetches from upstream before branching, achieving the same result.

#### `RemoteBranchExists(owner, repo, branch)` -- line 107

`GET /api/v4/projects/:id/repository/branches/:branch` -- returns true on 200, false on 404.

#### `DeleteRemoteBranch(owner, repo, branch)` -- line 128

`DELETE /api/v4/projects/:id/repository/branches/:branch` -- silently succeeds if branch is already gone (404).

#### `CommitChanges(...)` -- line 152

The core difference from GitHub. GitHub creates commits server-side via the Git Data API. GitLab commits locally and pushes:

```mermaid
flowchart TD
    A[StageAndCommitLocal] --> B{HasChanges?}
    B -->|No| C[Return ErrNoChanges]
    B -->|Yes| D["Amend commit with\nproper message + co-author"]
    D --> E[RestoreRemoteAuth]
    E --> F["git push --force origin branch"]
    F --> G["git rev-parse HEAD\n(get pushed SHA)"]
    G --> H[Return SHA]
```

1. Stage and commit uncommitted changes via `StageAndCommitLocal`.
2. Check if there are actual changes vs base branch. Return `ErrNoChanges` if not.
3. Amend the last commit with the real commit message and optional `Co-authored-by:` trailer.
4. Restore auth on the remote URL.
5. `git push --force origin <branch>` -- force push because commit was amended.
6. `git rev-parse HEAD` -- return the pushed commit SHA.

#### `CloneRepository(repoURL, directory)` -- line 210

Two paths:

**Existing repo** (`.git` exists -- workspace reuse):
- `git fetch origin` + `git reset --hard origin/main` (fallback to `origin/master`) + `git clean -fdx`

**Fresh clone**:
- Inject PAT into URL via `injectAuth`, then `git clone`

Both paths finish with `ConfigureUser` + `ConfigureSSHSigning` + `RestoreRemoteAuth`.

#### `CreatePR(params)` -- line 265

`POST /api/v4/projects/:id/merge_requests` with body:

```json
{
    "source_branch": "feature",
    "target_branch": "main",
    "title": "...",
    "description": "...",
    "labels": "label1,label2"
}
```

Returns `iid` (project-scoped MR number), `web_url`, `state`.

#### `GetPRForBranch` / `GetClosedPRForBranch` / `GetMergedPRForBranch` -- lines 311-321

All delegate to `findMRByBranch` with state filter `opened`, `closed`, or `merged`.

#### `findMRByBranch(owner, repo, head, state)` -- line 323

`GET /api/v4/projects/:id/merge_requests?source_branch=<branch>&state=<state>&per_page=1`

Strips fork owner prefix (`forkuser:branch` -> `branch`). Returns first match or nil.

#### `GetPRComments(owner, repo, number, since)` -- line 374

Paginated fetch of MR notes:

`GET /api/v4/projects/:id/merge_requests/:iid/notes?per_page=100&page=N&sort=asc`

- Skips system notes (`system: true`).
- Skips notes older than `since`.
- Extracts file path and line number from `position` field if present.
- `Resolvable = true` maps to `IsReviewComment = true`.
- Paginates: if 100 results returned, fetches next page.
- Returns `[]models.PRComment{}` (not nil) when empty.

#### `ReplyToComment(owner, repo, prNumber, commentID, body)` -- line 445

GitLab threading requires the **discussion ID** (not note ID):

1. `findDiscussionForNote` -- searches all discussions for the note.
2. If found: `POST .../discussions/:discussion_id/notes` (threaded reply).
3. If not found: falls back to `PostIssueComment` (standalone note).

#### `PostIssueComment(owner, repo, prNumber, body)` -- line 472

`POST /api/v4/projects/:id/merge_requests/:iid/notes` -- new top-level note.

#### `ListIssueComments(owner, repo, prNumber)` -- line 491

Fetches all non-system notes. Returns `[]models.IssueComment{}` (not nil).

#### `UpdateIssueComment(owner, repo, commentID, body)` -- line 530

`PUT /api/v4/projects/:id/notes/:note_id` -- updates note body.

#### `AddCommentReaction(owner, repo, comment, reaction)` -- line 551

No-op. Emoji API needs MR IID which isn't available from a `PRComment`. Logs debug and returns nil.

#### `AddPRLabel` / `RemovePRLabel` -- lines 565-571

Delegate to `updateMRLabels` with `add=true` or `add=false`.

#### `HasPRLabel(owner, repo, number, label)` -- line 573

GET the MR, linear scan of `labels` array.

#### `LastLabelRemoval(owner, repo, number, label)` -- line 599

Returns zero time. GitLab has no label events timeline API.

#### `ListCheckRunsForRef(owner, repo, ref)` -- line 607

Maps GitLab pipelines to GitHub's check runs:

1. `GET .../pipelines?sha=<ref>&per_page=1` -- find pipeline.
2. `allComplete` = true if status is not `running`/`pending`/`created`.
3. If `success`, return empty failures.
4. `GET .../pipelines/:id/jobs?scope[]=failed&per_page=100` -- get failed jobs.

#### `ListCheckRunAnnotations(owner, repo, checkRunID)` -- line 668

Returns empty. GitLab has no annotation equivalent.

#### `GetFailedJobLogs(owner, repo, headSHA, maxBytesPerStep)` -- line 673

Fetches actual log output from failed CI jobs:

1. Find pipeline for SHA.
2. Get failed jobs.
3. For each: `GET .../jobs/:id/trace` with `io.LimitReader` for size control.

#### `BranchHasCommits(owner, repo, branch, base)` -- line 738

`GET .../repository/compare?from=<base>&to=<branch>` -- true if comparison has commits.

#### `GetPRMergeability(owner, repo, number)` -- line 761

Reads MR: `mergeable = !has_conflicts && merge_status == "can_be_merged"`.

#### Internal Helpers

| Helper | Line | Purpose |
|--------|------|---------|
| `projectPath(owner, repo)` | 791 | URL-encode `owner/repo` as `owner%2Frepo` |
| `injectAuth(repoURL)` | 796 | Insert PAT into clone URL: `https://oauth2:<token>@<host>/...` |
| `doRequest(method, url, body)` | 809 | HTTP helper, sets `PRIVATE-TOKEN` header |
| `doJSONRequest(method, url, body)` | 818 | JSON HTTP helper, sets `Content-Type` + `PRIVATE-TOKEN` |
| `updateMRLabels(owner, repo, number, label, add)` | 832 | GET current labels, modify list, PUT back |
| `findDiscussionForNote(projectID, mrIID, noteID)` | 891 | Iterate all discussions to find which contains a note |
| `extractGitLabRepoInfo(repoURL)` | 922 | Parse URL into `owner` (with subgroups) and `repo` |

---

### `services/hosting/router.go` (302 lines -- NEW)

Multi-provider dispatch layer that satisfies all consumer-defined interfaces.

#### Router Dispatch Flow

```mermaid
flowchart TD
    Consumer[Consumer calls method] --> HasOwnerRepo{Method has\nowner/repo params?}
    HasOwnerRepo -->|Yes| ForRepo["forRepo(owner, repo)\nlookup in repoProviders map"]
    HasOwnerRepo -->|No| ForDir["forDir(directory)\nlookup in workspaceDirs map"]
    ForRepo --> Found1{Found?}
    Found1 -->|Yes| Provider1[Route to matched Provider]
    Found1 -->|No| Fallback1[Route to fallback Provider]
    ForDir --> Found2{Found?}
    Found2 -->|Yes| Provider2[Route to matched Provider]
    Found2 -->|No| Fallback2[Route to fallback Provider]
```

#### `Provider` Interface (line 25)

Union of all methods any consumer in the application needs. Both `GitHubServiceImpl` and `GitLabServiceImpl` satisfy this. Grouped by consumer:

- `workspace.Cloner` -- `CloneRepository`
- `executor.GitService` -- `CreateBranch`, `CommitChanges`, `CreatePR`, `GetPRComments`, etc.
- `scanner.PRFetcher` -- `GetClosedPRForBranch`, `GetMergedPRForBranch`
- `scanner.PRLabeler` -- `HasPRLabel`, `LastLabelRemoval`
- `scanner.MergeabilityChecker` -- `GetPRMergeability`
- `recovery.GitService` -- `BranchHasCommits`

#### `Router` Struct (line 76)

```go
type Router struct {
    repoProviders   map[string]Provider    // "owner/repo" -> Provider (built at startup)
    workspaceDirs   map[string]Provider    // "/path/to/dir" -> Provider (built at runtime)
    workspaceDirsMu sync.RWMutex           // concurrent access safety
    fallback        Provider               // default for unmapped repos
}
```

#### `NewRouter(repoProviders, fallback)` -- line 92

Normalizes all keys to lowercase for case-insensitive matching. Initializes workspace dir map.

#### `RegisterWorkspace(dir, owner, repo)` -- line 106

Manually associates a directory with a provider. Uses write lock.

#### `forRepo(owner, repo)` -- line 112

Lowercases `owner/repo`, looks up in `repoProviders`. Falls back to default.

#### `forDir(dir)` -- line 120

Looks up directory in `workspaceDirs`. Uses read lock. Falls back to default.

#### `CloneRepository(repoURL, directory)` -- line 131

Special -- both delegates AND registers:

1. Parse URL to get owner/repo.
2. Route to correct provider.
3. Call provider's `CloneRepository`.
4. On success, register directory in `workspaceDirs` so future dir-based calls route correctly.

#### Dir-Based Methods (lines 145-175)

Eight methods using `forDir(dir)`:

`CreateBranch`, `SwitchBranch`, `HasChanges`, `StripRemoteAuth`, `FetchRemote`, `SyncWithRemote`, `MergeBase`, `CloneImport`

#### Repo-Based Methods (lines 179-275)

All methods receiving `owner, repo` use `forRepo(owner, repo)`:

`SyncFork`, `RemoteBranchExists`, `DeleteRemoteBranch`, `CommitChanges`, `RestoreRemoteAuth`, `CreatePR`, `GetPRForBranch`, `GetClosedPRForBranch`, `GetMergedPRForBranch`, `GetPRComments`, `ReplyToComment`, `PostIssueComment`, `ListIssueComments`, `AddCommentReaction`, `ListCheckRunsForRef`, `ListCheckRunAnnotations`, `GetFailedJobLogs`, `AddPRLabel`, `RemovePRLabel`, `HasPRLabel`, `LastLabelRemoval`, `GetPRMergeability`, `BranchHasCommits`

> **Note**: `UpdateIssueComment` (line 232) uses `fallback` directly because the caller only has a comment ID, not owner/repo.

#### `extractOwnerRepo(repoURL)` -- line 279

Parses clone URLs into owner and repo. Handles both formats:

- **SSH**: `git@host:owner/repo.git` -- split on `:`, then split path on `/`
- **HTTPS**: `https://host/owner/[subgroups/]repo.git` -- repo is last segment, owner is everything between host and repo

---

### `models/config.go` Changes (+152 / -57)

#### `WorkspaceConfig.Hosting` -- line 112

New field on workspace config:

```go
type WorkspaceConfig struct {
    Hosting string `yaml:"hosting" mapstructure:"hosting"`
    // ... existing fields
}
```

Valid values: `"github"` (default when empty), `"gitlab"`.

#### `GitLab` Config Struct -- line 525

New top-level config section:

```go
GitLab struct {
    BaseURL           string   // e.g., "https://gitlab.com"
    AccessToken       string   // PAT or project/group access token
    BotUsername        string   // Git commit author name
    BotEmail          string   // Git commit author email
    MRLabel           string   // Default label for bot MRs
    SSHKeyPath        string   // Optional: SSH key for commit signing
    SkipMRLabel       string   // Label to skip MR processing
    MaxThreadDepth    int      // Max bot replies per thread
    KnownBotUsernames []string // Prevent bot-to-bot loops
    IgnoredUsernames  []string // Comments to completely skip
} `yaml:"gitlab" mapstructure:"gitlab"`
```

#### Constants -- line 807

```go
const (
    HostingGitHub = "github"
    HostingGitLab = "gitlab"
)
```

#### `WorkspaceConfig.HostingProvider()` -- line 814

Normalizes the hosting field. Returns `"gitlab"` if explicitly set, `"github"` for everything else (including empty).

#### `Config.RequiredHostingProviders()` -- line 826

Scans all project workspaces, returns `(needsGitHub, needsGitLab bool)`. Falls back to `needsGitHub=true` if no workspaces exist.

#### Validation Changes

```mermaid
flowchart TD
    Start[validate] --> Scan["RequiredHostingProviders()"]
    Scan --> NeedsGH{needsGitHub?}
    NeedsGH -->|Yes| ValidateGH["Validate github.app_id,\nprivate_key_path,\nbot_username, bot_email"]
    NeedsGH -->|No| SkipGH[Skip GitHub validation]
    ValidateGH --> NeedsGL{needsGitLab?}
    SkipGH --> NeedsGL
    NeedsGL -->|Yes| ValidateGL["Validate gitlab.base_url,\naccess_token,\nbot_username, bot_email"]
    NeedsGL -->|No| SkipGL[Skip GitLab validation]
    ValidateGL --> ValidateWS[Validate workspaces]
    SkipGL --> ValidateWS
    ValidateWS --> CheckHosting["Per workspace:\nhosting must be\ngithub, gitlab, or empty"]
```

Previously, GitHub credentials were always required. Now validation is conditional:
- GitHub credentials only required when any workspace uses github hosting.
- GitLab credentials only required when any workspace uses gitlab hosting.
- The `Hosting` field itself is validated to reject unknown values.

#### `LoadConfig` Additions

New `bindEnv` calls for all GitLab fields (`JIRA_AI_GITLAB_BASE_URL`, etc.).

New defaults:
- `gitlab.mr_label` = `"ai-mr"`
- `gitlab.skip_mr_label` = `"ai-bot-skip"`
- `gitlab.max_thread_depth` = `5`

---

### `main.go` Changes (+68 / -1)

#### Startup Wiring (line 51)

Replaces `gitService := services.NewGitHubService(config, logger)` with conditional construction:

```go
needsGitHub, needsGitLab := config.RequiredHostingProviders()

var ghService *services.GitHubServiceImpl
if needsGitHub {
    ghService = services.NewGitHubService(config, logger)
}

var glService *services.GitLabServiceImpl
if needsGitLab {
    glService = services.NewGitLabService(config, logger)
}

repoProviders := buildRepoProviderMap(config, ghService, glService)
var fallback hosting.Provider
if ghService != nil {
    fallback = ghService
} else {
    fallback = glService
}
gitService := hosting.NewRouter(repoProviders, fallback)
```

The rest of `main.go` is unchanged. `gitService` (now a `*hosting.Router`) is passed to all the same consumers as before because the router satisfies all their interfaces.

#### `buildRepoProviderMap(config, ghService, glService)` -- line 507

Iterates every `project -> workspace -> repo` and maps each repo URL's `owner/repo` to the correct provider based on `ws.HostingProvider()`.

#### `extractOwnerRepoFromURL(repoURL)` -- line 537

Parses HTTPS clone URLs into owner/repo. Same logic as `router.go`'s `extractOwnerRepo` but only handles HTTPS (config URLs are always HTTPS).

---

### `services/github.go` Changes (-406 lines)

Pure deletions. Nine methods replaced by one-liner delegates to `gitOps`:

| Deleted Method | Replacement |
|---------------|-------------|
| `CreateBranch` (60 lines) | `s.gitOps.CreateBranch(...)` |
| `SwitchBranch` (30 lines) | `s.gitOps.SwitchBranch(...)` |
| `HasChanges` (15 lines) | `s.gitOps.HasChanges(...)` |
| `hasWorkingTreeChanges` (15 lines) | Moved to `GitOps` |
| `hasUnpushedCommits` (40 lines) | Moved to `GitOps` |
| `stageAndCommitLocal` (25 lines) | `s.gitOps.StageAndCommitLocal(...)` |
| `StripRemoteAuth` (20 lines) | `s.gitOps.StripRemoteAuth(...)` |
| `FetchRemote` (15 lines) | `s.gitOps.FetchRemote(...)` |
| `SyncWithRemote` (35 lines) | `s.gitOps.SyncWithRemote(...)` |
| `preserveExcludedDirs` (20 lines) | Moved to `GitOps` |
| `restoreExcludedDirs` (10 lines) | Moved to `GitOps` |
| `MergeBase` (40 lines) | `s.gitOps.MergeBase(...)` |
| `runMerge` (5 lines) | Moved to `GitOps` |
| `listConflictFiles` (25 lines) | Moved to `GitOps` |
| `CloneImport` (20 lines) | `s.gitOps.CloneImport(...)` |

Added: `gitOps *GitOps` field, initialized in `NewGitHubService`.

Removed: `net/url` import (no longer needed after `StripRemoteAuth` moved).

---

## Commit Flow Comparison

```mermaid
flowchart LR
    subgraph github [GitHub Commit Flow]
        direction TB
        GH1[Stage + commit locally] --> GH2[Read local tree with git diff-tree]
        GH2 --> GH3["Create blobs via API\n(POST /git/blobs)"]
        GH3 --> GH4["Create tree via API\n(POST /git/trees)"]
        GH4 --> GH5["Create commit via API\n(POST /git/commits)"]
        GH5 --> GH6["Update ref via API\n(PATCH /git/refs)"]
        GH6 --> GH7["Verified commit\n(attributed to App)"]
    end
    subgraph gitlab [GitLab Commit Flow]
        direction TB
        GL1[Stage + commit locally] --> GL2[Amend with real message]
        GL2 --> GL3[Restore PAT in remote URL]
        GL3 --> GL4["git push --force origin branch"]
        GL4 --> GL5["Commit attributed\nto PAT owner"]
    end
```

**GitHub** uses server-side commits (Git Data API) producing "Verified" badges. **GitLab** uses local commit + push producing commits attributed to the PAT owner (optionally SSH-signed).

---

## Test Coverage Analysis

### `services/gitops_test.go` (126 lines)

| Test | What it covers |
|------|---------------|
| `TestGitOps_StripRemoteAuth` | Real git repo, sets auth URL, strips it, verifies clean URL |
| `TestGitOps_ConfigureUser` | Real git repo, sets user.name/email, reads back with `git config` |
| `TestGitOps_ConfigureSSHSigning_NoKey` | Empty path = no-op, nonexistent path = no-op |
| `TestGitOps_CloneImport` | Clones a bare repo, verifies `.git` exists in destination |

### `services/gitlab_test.go` (346 lines)

| Test | What it covers |
|------|---------------|
| `TestGitLabCreatePR` | POST method, auth header, response parsing (IID, URL) |
| `TestGitLabGetPRForBranch` | GET, response parsing (IID, source_branch, SHA) |
| `TestGitLabGetPRForBranch_NotFound` | Empty array returns nil |
| `TestGitLabGetPRComments` | System note filtered, file path and line extracted from position |
| `TestGitLabListCheckRunsForRef` | Two-call flow: pipeline status + failed jobs |
| `TestGitLabAddRemovePRLabel` | GET current labels + PUT updated labels |
| `TestGitLabRemoteBranchExists` | 200 = exists |
| `TestGitLabRemoteBranchNotExists` | 404 = not exists |
| `TestGitLabCloneRepository` | Fake executor, existing .git dir triggers fetch path |
| `TestExtractGitLabRepoInfo` | URL parsing with subgroups, error on invalid |

### `services/hosting/router_test.go` (282 lines)

| Test | What it covers |
|------|---------------|
| `TestRouter_RoutesToCorrectProvider` | gitlab-org/repo -> gitlab, github-org/repo -> github |
| `TestRouter_FallbackToDefault` | Unknown repo -> fallback |
| `TestRouter_DirBasedMethodsUseWorkspaceRegistry` | Registered dir -> correct provider, unknown -> fallback |
| `TestRouter_CloneRegistersWorkspace` | Clone auto-registers dir for future dir-based calls |
| `TestRouter_CaseInsensitiveRouting` | `GitLab-Org/Repo` matches `gitlab-org/repo` |
| `TestExtractOwnerRepo` | HTTPS, SSH, subgroups |

### Missing Coverage

| Area | What's missing |
|------|---------------|
| `CommitChanges` (GitLab) | Complex flow: stage, amend, push -- not tested |
| `GetPRMergeability` | Not tested |
| `BranchHasCommits` | Not tested |
| `GetFailedJobLogs` | Not tested |
| `DeleteRemoteBranch` | Not tested |
| Error paths | Non-2xx responses, JSON decode failures |
| `GitOps.CreateBranch` | The extracted method itself (covered indirectly by existing `github_test.go`) |
| `GitOps.SwitchBranch` | Same -- covered indirectly |
| `GitOps.HasChanges` | Same -- covered indirectly |
| `GitOps.MergeBase` | Same -- covered indirectly |

---

## Strengths

1. **Zero impact on existing users** -- default hosting is GitHub, validation is conditional, no config migration needed.
2. **Clean three-layer separation** -- shared GitOps, provider implementations, routing.
3. **No new dependencies** -- pure stdlib `net/http` client, no `go-gitlab` SDK.
4. **Extensible router pattern** -- adding a new provider (e.g., Bitbucket) would require: implement `Provider`, add config, register in `buildRepoProviderMap`.
5. **Thorough documentation** -- `docs/gitlab-support.md`, rich `config.example.yaml` comments, updated `AGENTS.md`.
6. **Consumer-agnostic** -- no consumer package knows about GitHub vs GitLab. The router satisfies all their interfaces transparently.
7. **Concurrent-safe** -- `workspaceDirs` uses `sync.RWMutex` for concurrent clone/lookup.

---

## Areas for Improvement

### Code Issues

| Issue | Location | Severity | Details |
|-------|----------|----------|---------|
| Duplicate URL parser | `main.go:extractOwnerRepoFromURL` vs `router.go:extractOwnerRepo` | Low | Same logic in two packages. Consider exporting one. |
| No pagination | `gitlab.go` multiple endpoints | Medium | `per_page=100` is GitLab's max. Repos with 100+ comments or jobs will lose data. |
| No-op string replace | `gitlab.go:88` | Low | `strings.Replace(baseURL, "https://", "https://", 1)` does nothing. |
| `AddCommentReaction` silently skipped | `gitlab.go:551` | Low | Returns nil without doing anything. Should log at Info level. |
| `LastLabelRemoval` returns zero time | `gitlab.go:599` | Low | Callers may behave differently thinking the label was never removed. |
| `CloneImport` routing | `router.go:173` | Low | `forDir(destDir)` is called before dir is registered -- always falls back. Works in practice (same credentials). |
| `workspaceDirs` grows monotonically | `router.go:82` | Low | No cleanup mechanism. Fine for short-lived instances but could accumulate over long runs. |
| Thread depth enforcement | `gitlab.go` | Medium | `max_thread_depth` is configured but enforcement depends on `commentfilter` package understanding GitLab discussions. |

### Design Considerations

- **Fork support**: GitLab `SyncFork` is a no-op. Fork-based workflows are limited to same-project branches for now.
- **Assignee resolution**: `CreatePR` receives usernames but GitLab needs user IDs. Currently passes empty `assignee_ids`.
- **No webhook support**: GitLab mode relies entirely on polling. Webhooks would reduce latency.
- **SSH key signing**: Optional and tested for no-key case, but the actual signing path is only tested indirectly via existing GitHub tests.
