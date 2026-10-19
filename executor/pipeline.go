package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/repoconfig"
	"jira-ai-issue-solver/services"
	"jira-ai-issue-solver/taskfile"
	"jira-ai-issue-solver/tracker"
	"jira-ai-issue-solver/workspace"
)

// Compile-time check that Pipeline implements Executor.
var _ Executor = (*Pipeline)(nil)

// Pipeline implements the job execution pipelines for new tickets
// and PR feedback. It coordinates workspace preparation, container
// lifecycle, AI execution, committing, and PR management.
type Pipeline struct {
	tracker    tracker.IssueTracker
	git        GitService
	containers container.Manager
	workspaces workspace.Manager
	taskWriter taskfile.Writer
	projects   ProjectResolver
	cfg        Config
	logger     *zap.Logger
}

// NewPipeline creates a Pipeline with the given dependencies.
// Returns an error if any required parameter is invalid.
func NewPipeline(
	cfg Config,
	issueTracker tracker.IssueTracker,
	git GitService,
	containers container.Manager,
	workspaces workspace.Manager,
	taskWriter taskfile.Writer,
	projects ProjectResolver,
	logger *zap.Logger,
) (*Pipeline, error) {
	if cfg.BotUsername == "" {
		return nil, errors.New("bot username must not be empty")
	}
	if cfg.DefaultProvider == "" {
		return nil, errors.New("default AI provider must not be empty")
	}
	if cfg.SessionTimeout < 0 {
		return nil, errors.New("session timeout must not be negative")
	}
	if issueTracker == nil {
		return nil, errors.New("issue tracker must not be nil")
	}
	if git == nil {
		return nil, errors.New("git service must not be nil")
	}
	if containers == nil {
		return nil, errors.New("container manager must not be nil")
	}
	if workspaces == nil {
		return nil, errors.New("workspace manager must not be nil")
	}
	if taskWriter == nil {
		return nil, errors.New("task file writer must not be nil")
	}
	if projects == nil {
		return nil, errors.New("project resolver must not be nil")
	}
	if logger == nil {
		return nil, errors.New("logger must not be nil")
	}

	return &Pipeline{
		tracker:    issueTracker,
		git:        git,
		containers: containers,
		workspaces: workspaces,
		taskWriter: taskWriter,
		projects:   projects,
		cfg:        cfg,
		logger:     logger,
	}, nil
}

// Execute dispatches a job by type. Matches [jobmanager.ExecuteFunc].
func (p *Pipeline) Execute(ctx context.Context, job *jobmanager.Job) (jobmanager.JobResult, error) {
	switch job.Type {
	case jobmanager.JobTypeNewTicket:
		return p.executeNewTicket(ctx, job)
	case jobmanager.JobTypeFeedback:
		return p.executeFeedback(ctx, job)
	case jobmanager.JobTypeMerge:
		return p.executeMerge(ctx, job)
	default:
		return jobmanager.JobResult{}, fmt.Errorf("unknown job type: %s", job.Type)
	}
}

func (p *Pipeline) executeNewTicket(ctx context.Context, job *jobmanager.Job) (result jobmanager.JobResult, retErr error) {
	logger := p.logger.With(
		zap.String("ticket", job.TicketKey),
		zap.String("job_id", job.ID),
		zap.Int("attempt", job.AttemptNum),
	)
	logger.Info("Starting new ticket pipeline")

	// --- Step 1: Fetch work item ---
	workItem, err := p.tracker.GetWorkItem(job.TicketKey)
	if err != nil {
		return result, fmt.Errorf("get work item: %w", err)
	}

	// --- Step 2: Resolve project settings ---
	settings, err := p.projects.ResolveProject(*workItem)
	if err != nil {
		return result, fmt.Errorf("resolve project: %w", err)
	}

	// --- Step 2a: Validate fork-mode requirements ---
	if err := p.validateForkMode(logger, job.TicketKey, workItem, settings); err != nil {
		return result, err
	}

	// --- Step 2b: Check per-ticket cost cap ---
	if p.ticketCostCapExceeded(logger, job.TicketKey, settings.MaxTicketCostUSD) {
		logger.Info("Per-ticket cost cap exceeded, skipping",
			zap.String("ticket", job.TicketKey),
			zap.Float64("cap_usd", settings.MaxTicketCostUSD))
		p.applyCostCapPRLabel(logger, job.TicketKey, settings, true)
		return result, errTicketCostCapExceeded
	}
	p.applyCostCapPRLabel(logger, job.TicketKey, settings, false)

	// --- Clean retry: delete stale branches and workspace ---
	if job.CleanRetry {
		p.cleanRetryState(logger, job.TicketKey, settings)
	}

	// --- Step 3: Transition to in-progress ---
	if err := p.tracker.TransitionStatus(job.TicketKey, settings.InProgressStatus); err != nil {
		return result, fmt.Errorf("transition to in-progress: %w", err)
	}
	statusTransitioned := true

	// Track container for cleanup.
	var ctr *container.Container

	defer func() {
		// Always stop container if started.
		if ctr != nil {
			if stopErr := p.containers.Stop(context.Background(), ctr); stopErr != nil {
				logger.Warn("Failed to stop container", zap.Error(stopErr))
			}
		}
		// On failure: revert status and optionally post error comment.
		if retErr != nil && statusTransitioned {
			p.handleFailure(logger, job.TicketKey, settings, job.AttemptNum, retErr)
		}
	}()

	if settings.IsMultiRepo() {
		return p.executeMultiRepoNewTicket(ctx, job, logger, workItem, settings)
	}

	// --- Step 4: Prepare workspace ---
	wsPath, reused, err := p.workspaces.FindOrCreate(job.TicketKey, settings.Repos[0].CloneURL)
	if err != nil {
		return result, fmt.Errorf("prepare workspace: %w", err)
	}
	logger.Info("Workspace ready",
		zap.String("path", wsPath),
		zap.Bool("reused", reused))

	// --- Step 5: Create or switch to branch ---
	branchName := fmt.Sprintf("%s/%s", p.cfg.BotUsername, job.TicketKey)
	if err := p.prepareBranch(logger, wsPath, branchName, reused, settings); err != nil {
		return result, err
	}

	// --- Step 6: Load repo config ---
	repoCfg, err := repoconfig.Load(wsPath)
	if err != nil {
		logger.Warn("Failed to load repo config, using defaults", zap.Error(err))
		repoCfg = repoconfig.Default()
	}

	// --- Step 7: Clone imports ---
	mergedImports, err := p.cloneImports(logger, wsPath, settings, repoCfg)
	if err != nil {
		return result, err
	}

	// --- Step 8: Download attachments, write issue and task files ---
	if err := p.writeNewTicketFiles(logger, *workItem, wsPath, settings); err != nil {
		return result, err
	}

	// --- Step 9: Determine AI provider ---
	provider := p.resolveProvider(settings)
	logger.Info("AI provider selected", zap.String("provider", provider))

	// --- Step 10: Build AI command ---
	sp := buildScriptParams(provider, p.cfg.DefaultClaudeModel, p.cfg.DefaultGeminiModel, repoCfg)
	execCommand := buildExecCommand(sp)

	// --- Step 11: Resolve and start container ---
	ctr, err = p.startContainer(ctx, wsPath, job.TicketKey, provider, settings)
	if err != nil {
		return result, fmt.Errorf("start container: %w", err)
	}

	// --- Step 11a: Run import install commands inside container ---
	if err := p.runImportInstalls(ctx, logger, ctr, mergedImports); err != nil {
		return result, fmt.Errorf("import install: %w", err)
	}

	// --- Step 11b: Strip remote auth before AI execution ---
	// Prevent the AI from pushing directly to the remote.
	if err := p.git.StripRemoteAuth(wsPath); err != nil {
		return result, fmt.Errorf("strip remote auth: %w", err)
	}
	authStripped := true
	defer func() {
		if authStripped {
			if restoreErr := p.git.RestoreRemoteAuth(wsPath, settings.CommitOwner(), settings.Repos[0].Repo); restoreErr != nil {
				logger.Warn("Failed to restore remote auth", zap.Error(restoreErr))
			}
		}
	}()

	// --- Step 12: Execute AI agent ---
	execCtx := ctx
	if p.cfg.SessionTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, p.cfg.SessionTimeout)
		defer cancel()
	}

	_, exitCode, execErr := p.containers.Exec(
		execCtx, ctr, execCommand)
	if execErr != nil {
		if ctx.Err() != nil {
			// Parent context cancelled (shutdown).
			return result, fmt.Errorf("job cancelled: %w", ctx.Err())
		}
		logger.Warn("AI agent exec failed", zap.Error(execErr))
	}

	// Read session metadata (may be absent on abnormal exit).
	session := readSessionOutput(wsPath)
	p.applyCostEstimate(&session)

	logger.Info("AI session completed",
		zap.Int("exit_code", exitCode),
		zap.Float64("cost_usd", session.CostUSD),
		zap.Any("validation_passed", session.ValidationPassed),
		zap.String("summary", session.Summary))
	result.CostUSD = session.CostUSD
	p.recordTicketCost(logger, wsPath, settings.MaxTicketCostUSD, result.CostUSD)

	// --- Step 12a: Restore remote auth ---
	// Must happen before SyncWithRemote which needs fetch access.
	// In fork mode, origin is set to the fork so that SyncWithRemote
	// fetches from the fork (where the API commit was created).
	if err := p.git.RestoreRemoteAuth(wsPath, settings.CommitOwner(), settings.Repos[0].Repo); err != nil {
		return result, fmt.Errorf("restore remote auth: %w", err)
	}
	authStripped = false

	// Exec runtime error (not just non-zero exit) is fatal.
	if execErr != nil {
		if execCtx.Err() != nil {
			return result, fmt.Errorf("session timeout exceeded: %w", execErr)
		}
		return result, fmt.Errorf("AI session failed: %w", execErr)
	}

	// --- Step 13: Check for changes ---
	hasChanges, err := p.git.HasChanges(wsPath, settings.Repos[0].BaseBranch)
	if err != nil {
		return result, fmt.Errorf("check changes: %w", err)
	}
	if !hasChanges {
		return result, fmt.Errorf("AI produced no changes (exit code: %d)", exitCode)
	}

	// --- Step 14: Commit via GitHub API ---
	importExcludes := collectExcludes(mergedImports)
	commitMsg := fmt.Sprintf("%s: %s", job.TicketKey, workItem.Summary)
	_, err = p.git.CommitChanges(
		settings.Repos[0].Owner, settings.CommitOwner(), settings.Repos[0].Repo, branchName,
		commitMsg, wsPath, settings.Repos[0].BaseBranch, workItem.Assignee, importExcludes,
	)
	if errors.Is(err, services.ErrNoChanges) {
		return result, fmt.Errorf("AI produced no committable changes (exit code: %d)", exitCode)
	}
	if err != nil {
		return result, fmt.Errorf("commit changes: %w", err)
	}

	// --- Step 15: Post-commit sync ---
	if err := p.git.SyncWithRemote(wsPath, branchName, importExcludes); err != nil {
		return result, fmt.Errorf("sync with remote: %w", err)
	}

	// --- Step 16: Create PR ---
	aiPR := readPRDescription(wsPath)
	prTitle, prBody := buildPRContent(workItem, job.TicketKey, repoCfg.PR.TitlePrefix, aiPR)

	pr, err := p.git.CreatePR(models.PRParams{
		Owner:     settings.Repos[0].Owner,
		Repo:      settings.Repos[0].Repo,
		Title:     prTitle,
		Body:      prBody,
		Head:      settings.PRHead(branchName),
		Base:      settings.Repos[0].BaseBranch,
		Draft:     repoCfg.PR.Draft,
		Labels:    repoCfg.PR.Labels,
		Assignees: assigneesFromSettings(settings),
	})
	if err != nil {
		return result, fmt.Errorf("create PR: %w", err)
	}

	result.PRURL = pr.URL
	result.PRNumber = pr.Number
	result.Draft = repoCfg.PR.Draft
	result.ValidationPassed = validationPassed(session, exitCode)

	logger.Info("PR created",
		zap.String("url", pr.URL),
		zap.Int("number", pr.Number),
		zap.Bool("draft", repoCfg.PR.Draft))

	// --- Step 16a: Apply validation labels ---
	vlTarget := validationLabel(session, exitCode, settings.PRValidationLabels)
	if vlTarget != "" {
		p.setPRValidationLabel(logger, settings.Repos[0].Owner, settings.Repos[0].Repo,
			pr.Number, settings.PRValidationLabels, vlTarget)
	}

	// --- Step 17: Update ticket ---
	p.setPRURL(logger, job.TicketKey, settings, pr.URL)
	p.cleanupStatusComment(logger, job.TicketKey)
	p.clearFailureLabels(logger, job.TicketKey, settings.FailureLabels)
	p.postOrUpdateCostComment(logger,
		settings.Repos[0].Owner, settings.Repos[0].Repo,
		pr.Number, result.CostUSD, "New ticket", 0)

	allLabels := models.AllPipelineLabels(settings.FailureLabels, settings.LifecycleLabels)
	p.setPipelineLabel(logger, job.TicketKey, allLabels, settings.LifecycleLabels.Review)
	if err := p.tracker.TransitionStatus(job.TicketKey, settings.InReviewStatus); err != nil {
		logger.Warn("Failed to transition to in-review", zap.Error(err))
	}

	return result, nil
}

// startContainer resolves configuration and starts a container.
func (p *Pipeline) startContainer(
	ctx context.Context,
	wsPath, ticketKey, provider string,
	settings *models.ProjectSettings,
) (*container.Container, error) {
	profileOverride := toSettingsOverride(settings)
	containerCfg, err := p.containers.ResolveConfig(wsPath, profileOverride)
	if err != nil {
		return nil, fmt.Errorf("resolve container config: %w", err)
	}

	// Mount the GCP credentials file for Vertex AI authentication.
	if provider == "claude" && p.cfg.ClaudeVertex != nil {
		containerCfg.ExtraMounts = append(containerCfg.ExtraMounts, container.Mount{
			Source:  p.cfg.ClaudeVertex.CredentialsFile,
			Target:  containerCredsMountTarget,
			Options: "ro",
		})
	}

	env := p.buildContainerEnv(provider)
	return p.containers.Start(ctx, containerCfg, wsPath, ticketKey, env)
}

// toSettingsOverride converts profile container settings to the
// container package's override type. Returns nil if no container
// settings are configured (zero-value ContainerSettings).
func toSettingsOverride(settings *models.ProjectSettings) *container.SettingsOverride {
	cs := settings.ResolvedContainer()
	if cs.Image == "" && cs.ResourceLimits.Memory == "" && cs.ResourceLimits.CPUs == "" &&
		len(cs.Env) == 0 && len(cs.Tmpfs) == 0 && len(cs.ExtraMounts) == 0 {
		return nil
	}
	mounts := make([]container.Mount, len(cs.ExtraMounts))
	for i, m := range cs.ExtraMounts {
		mounts[i] = container.Mount{Source: m.Source, Target: m.Target, Options: m.Options}
	}
	return &container.SettingsOverride{
		Image: cs.Image,
		Limits: container.ResourceLimits{
			Memory: cs.ResourceLimits.Memory,
			CPUs:   cs.ResourceLimits.CPUs,
		},
		Env:         cs.Env,
		Tmpfs:       cs.Tmpfs,
		ExtraMounts: mounts,
	}
}

// cloneImports merges project-level and repo-level imports (repo-level
// wins on path conflicts) and clones each into the workspace. Existing
// directories are skipped (workspace reuse). Import directories are
// excluded from commits at the GitHub API level (see isBotArtifact in
// services/github.go), and the nested .git directory prevents git from
// tracking their contents locally.
// Returns the merged import list for use by runImportInstalls.
func (p *Pipeline) cloneImports(
	logger *zap.Logger,
	wsPath string,
	settings *models.ProjectSettings,
	repoCfg *repoconfig.Config,
) ([]importEntry, error) {
	merged := mergeImports(settings, repoCfg)
	if len(merged) == 0 {
		return merged, nil
	}

	for _, imp := range merged {
		destDir := filepath.Join(wsPath, imp.Path)

		// Skip if already cloned (workspace reuse).
		if _, err := os.Stat(destDir); err == nil {
			logger.Debug("Import already exists, skipping",
				zap.String("path", imp.Path))
			continue
		}

		logger.Info("Cloning import",
			zap.String("repo", imp.Repo),
			zap.String("path", imp.Path),
			zap.String("ref", imp.Ref))

		if err := p.git.CloneImport(imp.Repo, destDir, imp.Ref); err != nil {
			return nil, fmt.Errorf("clone import %s into %s: %w", imp.Repo, imp.Path, err)
		}
	}

	return merged, nil
}

// runImportInstalls executes install commands for imports that declare
// one. Commands run inside the container from /workspace, with access
// to the container's full toolchain. This is plumbing — the bot sets
// up the environment so the AI finds it ready to use.
func (p *Pipeline) runImportInstalls(
	ctx context.Context,
	logger *zap.Logger,
	ctr *container.Container,
	imports []importEntry,
) error {
	for _, imp := range imports {
		if imp.Install == "" {
			continue
		}

		logger.Info("Running import install command",
			zap.String("path", imp.Path),
			zap.String("command", imp.Install))

		cmd := []string{
			"sh", "-c",
			"cd /workspace && " + imp.Install,
		}
		output, exitCode, err := p.containers.Exec(ctx, ctr, cmd)
		if err != nil {
			return fmt.Errorf("install command for import %s: %w", imp.Path, err)
		}
		if exitCode != 0 {
			return fmt.Errorf(
				"install command for import %s exited with code %d: %s",
				imp.Path, exitCode, output)
		}

		logger.Info("Import install completed",
			zap.String("path", imp.Path))
	}
	return nil
}

// importEntry is the unified type used during import merging.
type importEntry struct {
	Repo     string
	Path     string
	Ref      string
	Install  string
	Excludes []string
}

// mergeImports combines project-level and repo-level imports. When both
// sources declare the same destination path, the repo-level import wins
// (teams own their environment). Paths are normalized to avoid
// duplicates from trailing slashes.
func mergeImports(
	settings *models.ProjectSettings,
	repoCfg *repoconfig.Config,
) []importEntry {
	byPath := make(map[string]importEntry)

	// Profile-level imports go in first.
	for _, imp := range settings.Repos[0].Imports {
		p := filepath.Clean(imp.Path)
		byPath[p] = importEntry{Repo: imp.Repo, Path: p, Ref: imp.Ref, Install: imp.Install, Excludes: imp.Excludes}
	}

	// Repo-level imports override on path conflict.
	for _, imp := range repoCfg.Imports {
		p := filepath.Clean(imp.Path)
		byPath[p] = importEntry{Repo: imp.Repo, Path: p, Ref: imp.Ref, Install: imp.Install, Excludes: imp.Excludes}
	}

	// Deterministic order: sort by path.
	result := make([]importEntry, 0, len(byPath))
	for _, e := range byPath {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})

	return result
}

// collectExcludes gathers all exclude paths declared by imports into
// a single list. These paths are directories that import tools may
// create as output and should be excluded from commits.
func collectExcludes(imports []importEntry) []string {
	var excludes []string
	for _, imp := range imports {
		excludes = append(excludes, imp.Excludes...)
	}
	return excludes
}

// maxAttachmentSize is the maximum size (in bytes) of a single Jira
// attachment that will be downloaded. Larger files are skipped.
const maxAttachmentSize = 1 << 20 // 1 MiB

// downloadAttachments fetches qualifying Jira attachments into the
// workspace's .ai-session/attachments/ directory and returns the list of
// filenames that were written. Attachments exceeding maxAttachmentSize
// are skipped with a log message; download failures are logged and
// skipped (non-fatal).
func (p *Pipeline) downloadAttachments(
	logger *zap.Logger,
	workItem models.WorkItem,
	wsPath string,
) ([]string, error) {
	if len(workItem.Attachments) == 0 {
		return nil, nil
	}

	destDir := filepath.Join(wsPath, taskfile.AttachmentsDirPath)
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, fmt.Errorf("create attachments dir: %w", err)
	}

	var downloaded []string
	for _, att := range workItem.Attachments {
		// Sanitize filename early to check for existing file.
		safeName := filepath.Base(att.Filename)
		destPath := filepath.Join(destDir, safeName)

		// Skip attachments already on disk (workspace persists across sessions).
		if _, err := os.Stat(destPath); err == nil {
			downloaded = append(downloaded, safeName)
			logger.Debug("Attachment already exists, skipping download",
				zap.String("filename", safeName))
			continue
		}

		if att.Size > maxAttachmentSize {
			logger.Info("Skipping large attachment",
				zap.String("filename", att.Filename),
				zap.Int64("size_bytes", att.Size),
				zap.Int64("max_bytes", maxAttachmentSize))
			continue
		}

		data, err := p.tracker.DownloadAttachment(att.URL)
		if err != nil {
			logger.Warn("Failed to download attachment",
				zap.String("filename", att.Filename),
				zap.Error(err))
			continue
		}

		if err := os.WriteFile(destPath, data, 0o644); err != nil { // #nosec G306
			return nil, fmt.Errorf("write attachment %s: %w", safeName, err)
		}

		downloaded = append(downloaded, safeName)
		logger.Info("Downloaded attachment",
			zap.String("filename", safeName),
			zap.Int64("size_bytes", att.Size))
	}

	return downloaded, nil
}

func (p *Pipeline) resolveProvider(settings *models.ProjectSettings) string {
	if settings.AIProvider != "" {
		return settings.AIProvider
	}
	return p.cfg.DefaultProvider
}

// containerCredsMountTarget is the fixed path inside the container
// where the GCP service account key is mounted for Vertex AI auth.
const containerCredsMountTarget = "/run/secrets/gcp-sa-key.json" // #nosec G101 -- mount path, not a credential

func (p *Pipeline) buildContainerEnv(provider string) map[string]string {
	env := map[string]string{
		"AI_PROVIDER": provider,
		"PROJECT_DIR": "/workspace",
	}

	switch provider {
	case "claude":
		if p.cfg.ClaudeVertex != nil {
			env["CLAUDE_CODE_USE_VERTEX"] = "1"
			env["ANTHROPIC_VERTEX_PROJECT_ID"] = p.cfg.ClaudeVertex.ProjectID
			env["CLOUD_ML_REGION"] = p.cfg.ClaudeVertex.Region
			env["GOOGLE_APPLICATION_CREDENTIALS"] = containerCredsMountTarget
		} else if key, ok := p.cfg.AIAPIKeys["claude"]; ok {
			env["ANTHROPIC_API_KEY"] = key
		}
	case "gemini":
		if key, ok := p.cfg.AIAPIKeys["gemini"]; ok {
			env["GEMINI_API_KEY"] = key
		}
	}

	return env
}

// setPRURL stores the PR URL on the ticket via either a custom field
// or a structured comment.
func (p *Pipeline) setPRURL(logger *zap.Logger, ticketKey string, settings *models.ProjectSettings, prURL string) {
	if settings.PRURLFieldName != "" {
		if err := p.tracker.SetFieldValue(ticketKey, settings.PRURLFieldName, prURL); err != nil {
			logger.Warn("Failed to set PR URL field", zap.Error(err))
		}
	} else {
		comment := fmt.Sprintf("[AI-BOT-PR] %s", prURL)
		if err := p.tracker.AddComment(ticketKey, comment); err != nil {
			logger.Warn("Failed to add PR URL comment", zap.Error(err))
		}
	}
}

func (p *Pipeline) setMultiRepoPRURLs(logger *zap.Logger, ticketKey string, settings *models.ProjectSettings, prs []repoPR) {
	commentPRs := prs
	if settings.PRURLFieldName != "" {
		if err := p.tracker.SetFieldValue(ticketKey, settings.PRURLFieldName, prs[0].url); err != nil {
			logger.Warn("Failed to set PR URL field", zap.Error(err))
		}
		commentPRs = prs[1:]
	}
	for _, pr := range commentPRs {
		comment := fmt.Sprintf("[AI-BOT-PR] %s", pr.url)
		if err := p.tracker.AddComment(ticketKey, comment); err != nil {
			logger.Warn("Failed to add PR URL comment", zap.Error(err))
		}
	}
}

// cleanupStatusComment removes any [AI-BOT-STATUS] comment from the
// ticket. Called after a PR is created so that communication moves
// entirely to the PR. Errors are logged but not propagated.
func (p *Pipeline) cleanupStatusComment(logger *zap.Logger, ticketKey string) {
	comments, err := p.tracker.GetComments(ticketKey)
	if err != nil {
		logger.Warn("Failed to fetch comments for status cleanup", zap.Error(err))
		return
	}

	existing := findStatusComment(comments)
	if existing == nil {
		return
	}

	if err := p.tracker.DeleteComment(ticketKey, existing.ID); err != nil {
		logger.Warn("Failed to delete status comment", zap.Error(err))
	}
}

// handleFailure reverts the ticket status and upserts a status comment.
// If a previous [AI-BOT-STATUS] comment exists, it is updated in place;
// otherwise a new comment is created. This keeps at most one status
// comment per ticket.
func (p *Pipeline) handleFailure(logger *zap.Logger, ticketKey string, settings *models.ProjectSettings, attempt int, jobErr error) {
	if err := p.tracker.TransitionStatus(ticketKey, settings.TodoStatus); err != nil {
		logger.Error("Failed to revert ticket status",
			zap.String("target_status", settings.TodoStatus),
			zap.Error(err))
	}

	allLabels := models.AllPipelineLabels(settings.FailureLabels, settings.LifecycleLabels)
	p.setPipelineLabel(logger, ticketKey, allLabels, settings.FailureLabels.Blocked)

	if settings.DisableErrorComments {
		return
	}

	body := formatStatusComment(attempt, p.cfg.MaxRetries, p.cfg.RetryLabel, jobErr, time.Now())

	comments, err := p.tracker.GetComments(ticketKey)
	if err != nil {
		logger.Warn("Failed to fetch comments for status upsert, falling back to new comment", zap.Error(err))
		if addErr := p.tracker.AddComment(ticketKey, body); addErr != nil {
			logger.Error("Failed to post status comment", zap.Error(addErr))
		}
		return
	}

	if existing := findStatusComment(comments); existing != nil {
		if err := p.tracker.UpdateComment(ticketKey, existing.ID, body); err != nil {
			logger.Error("Failed to update status comment", zap.Error(err))
		}
		return
	}

	if err := p.tracker.AddComment(ticketKey, body); err != nil {
		logger.Error("Failed to post status comment", zap.Error(err))
	}
}

// prepareBranch sets up the working branch for a new-ticket pipeline run.
// When the workspace is reused and the remote branch still exists, it
// switches to it. When the remote branch was deleted (e.g., user closed
// the PR), it recreates the branch from the target branch so the AI
// starts from a clean slate. For fresh workspaces it creates a new branch.
func (p *Pipeline) prepareBranch(
	logger *zap.Logger,
	wsPath, branchName string,
	reused bool,
	settings *models.ProjectSettings,
) error {
	if !reused {
		if forkOwner := settings.ForkOwner(); forkOwner != "" {
			if err := p.git.SyncFork(forkOwner, settings.Repos[0].Repo, settings.Repos[0].BaseBranch); err != nil {
				logger.Warn("Failed to sync fork with upstream",
					zap.String("fork", forkOwner+"/"+settings.Repos[0].Repo),
					zap.Error(err))
			}
		}
		if err := p.git.CreateBranch(wsPath, branchName, settings.Repos[0].BaseBranch); err != nil {
			return fmt.Errorf("create branch: %w", err)
		}
		return nil
	}

	remoteExists, err := p.git.RemoteBranchExists(
		settings.CommitOwner(), settings.Repos[0].Repo, branchName)
	if err != nil {
		return fmt.Errorf("check remote branch: %w", err)
	}

	if remoteExists {
		if err := p.git.SwitchBranch(wsPath, branchName); err != nil {
			return fmt.Errorf("switch to branch: %w", err)
		}
		return nil
	}

	// Remote branch was deleted — start fresh from the target branch.
	logger.Info("Remote branch deleted, recreating from target branch",
		zap.String("branch", branchName))
	if err := p.git.CreateBranch(wsPath, branchName, settings.Repos[0].BaseBranch); err != nil {
		return fmt.Errorf("recreate branch: %w", err)
	}
	return nil
}

// assigneesFromSettings returns the PR assignee list from resolved
// project settings. Returns nil when no GitHub username is configured.
func assigneesFromSettings(settings *models.ProjectSettings) []string {
	if settings.GitHubUsername == "" {
		return nil
	}
	return []string{settings.GitHubUsername}
}

// executeMultiRepoNewTicket handles new-ticket execution for workspaces
// with multiple repositories. It clones all repos, runs one AI session
// against the workspace root, then fans out commit/PR creation per repo.
func (p *Pipeline) executeMultiRepoNewTicket(
	ctx context.Context,
	job *jobmanager.Job,
	logger *zap.Logger,
	workItem *models.WorkItem,
	settings *models.ProjectSettings,
) (result jobmanager.JobResult, retErr error) {
	var ctr *container.Container

	defer func() {
		if ctr != nil {
			if stopErr := p.containers.Stop(context.Background(), ctr); stopErr != nil {
				logger.Warn("Failed to stop container", zap.Error(stopErr))
			}
		}
	}()

	// --- Step 4: Prepare multi-repo workspace ---
	wsPath, reused, err := p.prepareMultiRepoWorkspace(logger, job.TicketKey, settings)
	if err != nil {
		return result, err
	}

	// --- Step 5: Create or switch to branch per repo ---
	branchName := fmt.Sprintf("%s/%s", p.cfg.BotUsername, job.TicketKey)
	for _, repo := range settings.Repos {
		repoDir := filepath.Join(wsPath, repo.Name)
		if err := p.prepareBranchForRepo(logger, repoDir, branchName, reused, settings, repo); err != nil {
			return result, err
		}
	}

	// --- Step 6: Load repo config per repo ---
	repoConfigs := make([]*repoconfig.Config, len(settings.Repos))
	for i, repo := range settings.Repos {
		repoDir := filepath.Join(wsPath, repo.Name)
		cfg, err := repoconfig.Load(repoDir)
		if err != nil {
			logger.Warn("Failed to load repo config, using defaults",
				zap.String("repo", repo.Name), zap.Error(err))
			cfg = repoconfig.Default()
		}
		repoConfigs[i] = cfg
	}

	// --- Step 7: Clone merged imports ---
	mergedImports := mergeMultiRepoImports(settings, repoConfigs)
	if err := p.cloneImportEntries(logger, wsPath, mergedImports); err != nil {
		return result, err
	}

	// --- Step 8: Download attachments, write issue and task files ---
	downloaded, err := p.downloadAttachments(logger, *workItem, wsPath)
	if err != nil {
		return result, fmt.Errorf("download attachments: %w", err)
	}
	comments := p.fetchTicketComments(logger, workItem.Key)
	if err := p.taskWriter.WriteIssue(*workItem, wsPath, downloaded, comments); err != nil {
		return result, fmt.Errorf("write issue file: %w", err)
	}

	repoContexts := make([]taskfile.RepoContext, len(settings.Repos))
	for i, repo := range settings.Repos {
		repoContexts[i] = taskfile.RepoContext{
			Name:                      repo.Name,
			Dir:                       filepath.Join(wsPath, repo.Name),
			OverrideInstructions:      repo.Instructions,
			OverrideNewTicketWorkflow: repo.NewTicketWorkflow,
		}
	}
	if err := p.taskWriter.WriteMultiRepoNewTicketTask(*workItem, wsPath, repoContexts); err != nil {
		return result, fmt.Errorf("write task file: %w", err)
	}

	// --- Step 9: Determine AI provider ---
	provider := p.resolveProvider(settings)
	logger.Info("AI provider selected", zap.String("provider", provider))

	// --- Step 10: Build AI command (use first repo's config for AI settings) ---
	sp := buildScriptParams(provider, p.cfg.DefaultClaudeModel, p.cfg.DefaultGeminiModel, repoConfigs[0])
	execCommand := buildExecCommand(sp)

	// --- Step 11: Resolve and start container ---
	ctr, err = p.startContainer(ctx, wsPath, job.TicketKey, provider, settings)
	if err != nil {
		return result, fmt.Errorf("start container: %w", err)
	}

	if err := p.runImportInstalls(ctx, logger, ctr, mergedImports); err != nil {
		return result, fmt.Errorf("import install: %w", err)
	}

	// --- Step 11b: Strip remote auth per repo ---
	for _, repo := range settings.Repos {
		repoDir := filepath.Join(wsPath, repo.Name)
		if err := p.git.StripRemoteAuth(repoDir); err != nil {
			return result, fmt.Errorf("strip remote auth for %s: %w", repo.Name, err)
		}
	}
	authStripped := true
	defer func() {
		if authStripped {
			for _, repo := range settings.Repos {
				repoDir := filepath.Join(wsPath, repo.Name)
				if restoreErr := p.git.RestoreRemoteAuth(
					repoDir, settings.CommitOwnerFor(repo), repo.Repo); restoreErr != nil {
					logger.Warn("Failed to restore remote auth",
						zap.String("repo", repo.Name), zap.Error(restoreErr))
				}
			}
		}
	}()

	// --- Step 12: Execute AI agent ---
	execCtx := ctx
	if p.cfg.SessionTimeout > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, p.cfg.SessionTimeout)
		defer cancel()
	}

	_, exitCode, execErr := p.containers.Exec(execCtx, ctr, execCommand)
	if execErr != nil {
		if ctx.Err() != nil {
			return result, fmt.Errorf("job cancelled: %w", ctx.Err())
		}
		logger.Warn("AI agent exec failed", zap.Error(execErr))
	}

	session := readSessionOutput(wsPath)
	p.applyCostEstimate(&session)
	logger.Info("AI session completed",
		zap.Int("exit_code", exitCode),
		zap.Float64("cost_usd", session.CostUSD),
		zap.Any("validation_passed", session.ValidationPassed),
		zap.String("summary", session.Summary))
	result.CostUSD = session.CostUSD
	p.recordTicketCost(logger, wsPath, settings.MaxTicketCostUSD, result.CostUSD)

	// --- Step 12a: Restore remote auth per repo ---
	for _, repo := range settings.Repos {
		repoDir := filepath.Join(wsPath, repo.Name)
		if err := p.git.RestoreRemoteAuth(
			repoDir, settings.CommitOwnerFor(repo), repo.Repo); err != nil {
			return result, fmt.Errorf("restore remote auth for %s: %w", repo.Name, err)
		}
	}
	authStripped = false

	if execErr != nil {
		if execCtx.Err() != nil {
			return result, fmt.Errorf("session timeout exceeded: %w", execErr)
		}
		return result, fmt.Errorf("AI session failed: %w", execErr)
	}

	// --- Step 13–16: Per-repo fan-out (changes → commit → PR) ---
	importExcludes := collectExcludes(mergedImports)
	aiPR := readPRDescription(wsPath)
	vlTarget := validationLabel(session, exitCode, settings.PRValidationLabels)

	prs, err := p.fanOutCommitAndPR(logger, fanOutParams{
		settings:    settings,
		workItem:    workItem,
		wsPath:      wsPath,
		branchName:  branchName,
		ticketKey:   job.TicketKey,
		repoConfigs: repoConfigs,
		excludes:    importExcludes,
		aiPR:        aiPR,
		vlTarget:    vlTarget,
	})
	if err != nil {
		return result, err
	}
	if len(prs) == 0 {
		return result, fmt.Errorf("AI produced no changes in any repository (exit code: %d)", exitCode)
	}

	// --- Step 17: Update ticket with all PR URLs ---
	p.setMultiRepoPRURLs(logger, job.TicketKey, settings, prs)
	p.cleanupStatusComment(logger, job.TicketKey)
	p.clearFailureLabels(logger, job.TicketKey, settings.FailureLabels)

	// Post cost on the first PR only to avoid double-counting.
	p.postOrUpdateCostComment(logger,
		prs[0].owner, prs[0].repo,
		prs[0].number, result.CostUSD, "New ticket", 0)
	p.postCostCrossReference(logger, costCrossRefFromRepoPRs(prs))

	result.PRURL = prs[0].url
	result.PRNumber = prs[0].number
	result.Draft = prs[0].draft
	result.ValidationPassed = validationPassed(session, exitCode)

	allLabels := models.AllPipelineLabels(settings.FailureLabels, settings.LifecycleLabels)
	p.setPipelineLabel(logger, job.TicketKey, allLabels, settings.LifecycleLabels.Review)
	if err := p.tracker.TransitionStatus(job.TicketKey, settings.InReviewStatus); err != nil {
		logger.Warn("Failed to transition to in-review", zap.Error(err))
	}

	return result, nil
}

// fetchTicketComments fetches comments from the tracker and filters
// out bot-authored and trivially short comments. Errors are logged
// and result in an empty slice — missing comments should not block
// ticket processing.
func (p *Pipeline) fetchTicketComments(logger *zap.Logger, ticketKey string) []models.Comment {
	all, err := p.tracker.GetComments(ticketKey)
	if err != nil {
		logger.Warn("Failed to fetch ticket comments", zap.String("ticket", ticketKey), zap.Error(err))
		return []models.Comment{}
	}
	return FilterTicketComments(all, p.cfg.JiraUsername, p.cfg.MinCommentLength)
}

// FilterTicketComments removes comments authored by the bot and
// comments shorter than minLen characters.
func FilterTicketComments(comments []models.Comment, jiraUsername string, minLen int) []models.Comment {
	filtered := make([]models.Comment, 0, len(comments))
	lower := strings.ToLower(jiraUsername)
	for _, c := range comments {
		if lower != "" && strings.ToLower(c.AuthorEmail) == lower {
			continue
		}
		if minLen > 0 && len(strings.TrimSpace(c.Body)) < minLen {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// writeNewTicketFiles downloads attachments and writes the issue and
// task files for a single-repo new-ticket pipeline run.
func (p *Pipeline) writeNewTicketFiles(
	logger *zap.Logger,
	workItem models.WorkItem,
	wsPath string,
	settings *models.ProjectSettings,
) error {
	downloaded, err := p.downloadAttachments(logger, workItem, wsPath)
	if err != nil {
		return fmt.Errorf("download attachments: %w", err)
	}
	comments := p.fetchTicketComments(logger, workItem.Key)
	if err := p.taskWriter.WriteIssue(workItem, wsPath, downloaded, comments); err != nil {
		return fmt.Errorf("write issue file: %w", err)
	}
	if err := p.taskWriter.WriteNewTicketTask(
		workItem, wsPath, settings.Repos[0].Instructions, settings.Repos[0].NewTicketWorkflow,
	); err != nil {
		return fmt.Errorf("write task file: %w", err)
	}
	return nil
}

type fanOutParams struct {
	settings    *models.ProjectSettings
	workItem    *models.WorkItem
	wsPath      string
	branchName  string
	ticketKey   string
	repoConfigs []*repoconfig.Config
	excludes    []string
	aiPR        *PRDescription
	vlTarget    string
}

type repoPR struct {
	owner  string
	repo   string
	url    string
	number int
	draft  bool
}

// fanOutCommitAndPR iterates each repo, commits changes via the GitHub
// API, syncs the workspace, and creates a PR. Repos without changes are
// skipped. Returns the list of created PRs (may be empty).
func (p *Pipeline) fanOutCommitAndPR(
	logger *zap.Logger,
	params fanOutParams,
) ([]repoPR, error) {
	var prs []repoPR

	for i, repo := range params.settings.Repos {
		repoDir := filepath.Join(params.wsPath, repo.Name)

		hasChanges, err := p.git.HasChanges(repoDir, repo.BaseBranch)
		if err != nil {
			return nil, fmt.Errorf("check changes for %s: %w", repo.Name, err)
		}
		if !hasChanges {
			logger.Info("No changes in repo, skipping", zap.String("repo", repo.Name))
			continue
		}

		commitMsg := fmt.Sprintf("%s: %s", params.ticketKey, params.workItem.Summary)
		_, err = p.git.CommitChanges(
			repo.Owner, params.settings.CommitOwnerFor(repo), repo.Repo, params.branchName,
			commitMsg, repoDir, repo.BaseBranch, params.workItem.Assignee, params.excludes,
		)
		if errors.Is(err, services.ErrNoChanges) {
			logger.Info("No committable changes in repo", zap.String("repo", repo.Name))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("commit changes for %s: %w", repo.Name, err)
		}

		if err := p.git.SyncWithRemote(repoDir, params.branchName, params.excludes); err != nil {
			return nil, fmt.Errorf("sync with remote for %s: %w", repo.Name, err)
		}

		prTitle, prBody := buildPRContent(
			params.workItem, params.ticketKey, params.repoConfigs[i].PR.TitlePrefix, params.aiPR)

		pr, err := p.git.CreatePR(models.PRParams{
			Owner:     repo.Owner,
			Repo:      repo.Repo,
			Title:     prTitle,
			Body:      prBody,
			Head:      params.settings.PRHead(params.branchName),
			Base:      repo.BaseBranch,
			Draft:     params.repoConfigs[i].PR.Draft,
			Labels:    params.repoConfigs[i].PR.Labels,
			Assignees: assigneesFromSettings(params.settings),
		})
		if err != nil {
			return nil, fmt.Errorf("create PR for %s: %w", repo.Name, err)
		}

		if params.vlTarget != "" {
			p.setPRValidationLabel(logger, repo.Owner, repo.Repo,
				pr.Number, params.settings.PRValidationLabels, params.vlTarget)
		}

		prs = append(prs, repoPR{owner: repo.Owner, repo: repo.Repo, url: pr.URL, number: pr.Number, draft: params.repoConfigs[i].PR.Draft})
		logger.Info("PR created",
			zap.String("repo", repo.Name),
			zap.String("url", pr.URL),
			zap.Int("number", pr.Number),
			zap.Bool("draft", params.repoConfigs[i].PR.Draft))
	}

	return prs, nil
}

// prepareBranchForRepo sets up the working branch for a single repo
// within a multi-repo workspace. Same logic as prepareBranch but
// operates on the specific repo's directory and owner.
func (p *Pipeline) prepareBranchForRepo(
	logger *zap.Logger,
	repoDir, branchName string,
	reused bool,
	settings *models.ProjectSettings,
	repo models.RepoSettings,
) error {
	commitOwner := settings.CommitOwnerFor(repo)

	if !reused {
		if forkOwner := settings.ForkOwner(); forkOwner != "" {
			if err := p.git.SyncFork(forkOwner, repo.Repo, repo.BaseBranch); err != nil {
				logger.Warn("Failed to sync fork with upstream",
					zap.String("fork", forkOwner+"/"+repo.Repo),
					zap.Error(err))
			}
		}
		if err := p.git.CreateBranch(repoDir, branchName, repo.BaseBranch); err != nil {
			return fmt.Errorf("create branch in %s: %w", repo.Name, err)
		}
		return nil
	}

	remoteExists, err := p.git.RemoteBranchExists(commitOwner, repo.Repo, branchName)
	if err != nil {
		return fmt.Errorf("check remote branch for %s: %w", repo.Name, err)
	}

	if remoteExists {
		if err := p.git.SwitchBranch(repoDir, branchName); err != nil {
			return fmt.Errorf("switch to branch in %s: %w", repo.Name, err)
		}
		return nil
	}

	logger.Info("Remote branch deleted, recreating from target branch",
		zap.String("repo", repo.Name),
		zap.String("branch", branchName))
	if err := p.git.CreateBranch(repoDir, branchName, repo.BaseBranch); err != nil {
		return fmt.Errorf("recreate branch in %s: %w", repo.Name, err)
	}
	return nil
}

// cleanRetryState deletes remote branches and the local workspace to
// ensure a clean slate for a retry. Errors are logged but not
// propagated — cleanup is best-effort so the retry proceeds even if
// partial cleanup fails.
func (p *Pipeline) cleanRetryState(
	logger *zap.Logger,
	ticketKey string,
	settings *models.ProjectSettings,
) {
	branchName := fmt.Sprintf("%s/%s", p.cfg.BotUsername, ticketKey)

	for _, repo := range settings.Repos {
		owner := settings.CommitOwnerFor(repo)
		if err := p.git.DeleteRemoteBranch(owner, repo.Repo, branchName); err != nil {
			logger.Warn("Clean retry: failed to delete remote branch",
				zap.String("repo", owner+"/"+repo.Repo),
				zap.String("branch", branchName),
				zap.Error(err))
		} else {
			logger.Info("Clean retry: deleted remote branch",
				zap.String("repo", owner+"/"+repo.Repo),
				zap.String("branch", branchName))
		}
	}

	if err := p.workspaces.Cleanup(ticketKey); err != nil {
		logger.Warn("Clean retry: failed to delete workspace",
			zap.String("ticket", ticketKey),
			zap.Error(err))
	} else {
		logger.Info("Clean retry: deleted workspace",
			zap.String("ticket", ticketKey))
	}
}

// cloneImportEntries clones the given imports into the workspace,
// skipping directories that already exist (workspace reuse).
func (p *Pipeline) cloneImportEntries(
	logger *zap.Logger,
	wsPath string,
	imports []importEntry,
) error {
	for _, imp := range imports {
		destDir := filepath.Join(wsPath, imp.Path)

		if _, err := os.Stat(destDir); err == nil {
			logger.Debug("Import already exists, skipping",
				zap.String("path", imp.Path))
			continue
		}

		logger.Info("Cloning import",
			zap.String("repo", imp.Repo),
			zap.String("path", imp.Path),
			zap.String("ref", imp.Ref))

		if err := p.git.CloneImport(imp.Repo, destDir, imp.Ref); err != nil {
			return fmt.Errorf("clone import %s into %s: %w", imp.Repo, imp.Path, err)
		}
	}
	return nil
}

// mergeMultiRepoImports combines imports from all repos' profiles and
// repo-level configs. Within each repo, repo-level imports override
// profile imports on path conflict. Across repos, later repos override
// earlier repos on path conflict. Result is sorted by path.
func mergeMultiRepoImports(
	settings *models.ProjectSettings,
	repoConfigs []*repoconfig.Config,
) []importEntry {
	byPath := make(map[string]importEntry)

	for i, repo := range settings.Repos {
		for _, imp := range repo.Imports {
			p := filepath.Clean(imp.Path)
			byPath[p] = importEntry{
				Repo: imp.Repo, Path: p, Ref: imp.Ref,
				Install: imp.Install, Excludes: imp.Excludes,
			}
		}

		if i < len(repoConfigs) && repoConfigs[i] != nil {
			for _, imp := range repoConfigs[i].Imports {
				p := filepath.Clean(imp.Path)
				byPath[p] = importEntry{
					Repo: imp.Repo, Path: p, Ref: imp.Ref,
					Install: imp.Install, Excludes: imp.Excludes,
				}
			}
		}
	}

	result := make([]importEntry, 0, len(byPath))
	for _, e := range byPath {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Path < result[j].Path
	})
	return result
}

// validationLabel returns the PR validation label to apply based on
// the AI session's output. Returns empty string when validation passed
// and exit code is zero. ValidationFailed takes precedence over
// NonzeroExit because it is the more specific signal.
func validationLabel(session SessionOutput, exitCode int, vl models.PRValidationLabels) string {
	if session.ValidationPassed != nil && !*session.ValidationPassed {
		if vl.ValidationFailed != "" {
			return vl.ValidationFailed
		}
	}
	if exitCode != 0 {
		return vl.NonzeroExit
	}
	return ""
}

// validationPassed reports whether the AI session completed
// successfully: validation was not explicitly failed and the container
// exited with code zero.
func validationPassed(session SessionOutput, exitCode int) bool {
	if session.ValidationPassed != nil && !*session.ValidationPassed {
		return false
	}
	return exitCode == 0
}

// buildPRContent generates the PR title and body from the work item.
// Security-level tickets get redacted content.
func buildPRContent(workItem *models.WorkItem, ticketKey, titlePrefix string, aiPR *PRDescription) (title, body string) {
	if workItem.HasSecurityLevel() {
		// Security-level tickets always use redacted content —
		// the AI might leak vulnerability details in its PR description.
		title = fmt.Sprintf("%s: Security fix", ticketKey)
		body = fmt.Sprintf("Security fix for %s.\n\nDetails redacted due to security level.", ticketKey)
	} else if aiPR != nil && aiPR.Title != "" {
		// AI-generated PR description takes precedence over Jira-derived.
		// Strip the ticket key prefix if the AI already included it.
		aiTitle := aiPR.Title
		if cut, ok := strings.CutPrefix(aiTitle, ticketKey+": "); ok {
			aiTitle = cut
		} else if cut, ok := strings.CutPrefix(aiTitle, ticketKey+" "); ok {
			aiTitle = cut
		} else if cut, ok := strings.CutPrefix(aiTitle, "["+ticketKey+"]: "); ok {
			aiTitle = cut
		} else if cut, ok := strings.CutPrefix(aiTitle, "["+ticketKey+"] "); ok {
			aiTitle = cut
		}
		title = fmt.Sprintf("%s: %s", ticketKey, aiTitle)
		body = fmt.Sprintf("Resolves %s", ticketKey)
		if aiPR.Body != "" {
			body += "\n\n" + aiPR.Body
		}
	} else if aiPR != nil && aiPR.Body != "" {
		// AI wrote a body but no usable title — use Jira summary as
		// title but keep the AI-generated body.
		title = fmt.Sprintf("%s: %s", ticketKey, workItem.Summary)
		body = fmt.Sprintf("Resolves %s\n\n%s", ticketKey, aiPR.Body)
	} else {
		title = fmt.Sprintf("%s: %s", ticketKey, workItem.Summary)
		body = fmt.Sprintf("Resolves %s\n\n## Summary\n%s", ticketKey, workItem.Summary)
		if workItem.Description != "" {
			body += fmt.Sprintf("\n\n## Description\n%s", workItem.Description)
		}
	}

	if titlePrefix != "" {
		title = titlePrefix + " " + title
	}

	return title, body
}

// buildScriptParams extracts provider-specific script configuration
// from the repo config, falling back to the pipeline's default model.
func buildScriptParams(provider, defaultClaudeModel, defaultGeminiModel string, repoCfg *repoconfig.Config) scriptParams {
	params := scriptParams{Provider: provider}

	// Apply bot-level defaults first.
	switch provider {
	case "claude":
		params.Model = defaultClaudeModel
	case "gemini":
		params.Model = defaultGeminiModel
	}

	if repoCfg == nil {
		return params
	}

	// Repo-level overrides.
	if repoCfg.AI.Claude != nil {
		params.AllowedTools = repoCfg.AI.Claude.AllowedTools
		if repoCfg.AI.Claude.Model != "" {
			params.Model = repoCfg.AI.Claude.Model
		}
	}
	if repoCfg.AI.Gemini != nil && repoCfg.AI.Gemini.Model != "" {
		params.Model = repoCfg.AI.Gemini.Model
	}
	return params
}

// prepareMultiRepoWorkspace creates or reuses a multi-repo workspace
// and filters settings.Repos to only repos whose directories exist
// on disk. Repos added to the config after a workspace was created
// won't be present yet; filtering them prevents downstream loops
// from crashing on missing paths.
func (p *Pipeline) prepareMultiRepoWorkspace(
	logger *zap.Logger,
	ticketKey string,
	settings *models.ProjectSettings,
) (string, bool, error) {
	repoEntries := make([]workspace.RepoEntry, len(settings.Repos))
	for i, r := range settings.Repos {
		repoEntries[i] = workspace.RepoEntry{Name: r.Name, URL: r.CloneURL}
	}
	wsPath, reused, err := p.workspaces.FindOrCreateMultiRepo(ticketKey, repoEntries, settings.RootRepoURL)
	if err != nil {
		return "", false, fmt.Errorf("prepare workspace: %w", err)
	}
	logger.Info("Multi-repo workspace ready",
		zap.String("path", wsPath),
		zap.Bool("reused", reused),
		zap.Int("repos", len(settings.Repos)))

	settings.Repos, err = filterPresentRepos(logger, ticketKey, wsPath, settings.Repos)
	if err != nil {
		return "", false, err
	}
	if len(settings.Repos) == 0 {
		return "", false, fmt.Errorf("no repo directories found in workspace %s", wsPath)
	}
	return wsPath, reused, nil
}

// filterPresentRepos returns only the repos whose directories exist
// in the workspace.
func filterPresentRepos(logger *zap.Logger, ticketKey, wsPath string, repos []models.RepoSettings) ([]models.RepoSettings, error) {
	present := make([]models.RepoSettings, 0, len(repos))
	for _, repo := range repos {
		repoDir := filepath.Join(wsPath, repo.Name)
		_, err := os.Stat(repoDir)
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn("Repo directory not found in workspace, skipping",
				zap.String("ticket", ticketKey),
				zap.String("repo", repo.Name),
				zap.String("expected", repoDir))
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("stat repo directory %s: %w", repoDir, err)
		}
		present = append(present, repo)
	}
	if len(present) < len(repos) {
		logger.Info("Filtered workspace repos",
			zap.String("ticket", ticketKey),
			zap.Int("present", len(present)),
			zap.Int("missing", len(repos)-len(present)))
	}
	return present, nil
}
