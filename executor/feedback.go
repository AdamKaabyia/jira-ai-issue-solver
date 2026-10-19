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

	"jira-ai-issue-solver/commentfilter"
	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/repoconfig"
	"jira-ai-issue-solver/services"
	"jira-ai-issue-solver/taskfile"
)

func (p *Pipeline) executeFeedback(ctx context.Context, job *jobmanager.Job) (result jobmanager.JobResult, retErr error) {
	logger := p.logger.With(
		zap.String("ticket", job.TicketKey),
		zap.String("job_id", job.ID),
		zap.Int("attempt", job.AttemptNum),
	)
	logger.Info("Starting feedback pipeline")

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
		logger.Info("Per-ticket cost cap exceeded, skipping feedback",
			zap.String("ticket", job.TicketKey),
			zap.Float64("cap_usd", settings.MaxTicketCostUSD))
		p.applyCostCapPRLabel(logger, job.TicketKey, settings, true)
		return result, errTicketCostCapExceeded
	}
	p.applyCostCapPRLabel(logger, job.TicketKey, settings, false)

	defer func() {
		// On failure post error comment (but do NOT revert status --
		// the ticket stays "in review").
		if retErr != nil {
			p.handleFeedbackFailure(logger, job.TicketKey, settings, retErr)
		}
	}()

	if settings.IsMultiRepo() {
		return p.executeMultiRepoFeedback(ctx, job, logger, workItem, settings)
	}

	// Track container for cleanup.
	var ctr *container.Container

	defer func() {
		if ctr != nil {
			if stopErr := p.containers.Stop(context.Background(), ctr); stopErr != nil {
				logger.Warn("Failed to stop container", zap.Error(stopErr))
			}
		}
	}()

	// --- Step 3: Find PR by branch ---
	branchName := fmt.Sprintf("%s/%s", p.cfg.BotUsername, job.TicketKey)
	prDetails, err := p.findPRByHeads(settings.Repos[0].Owner, settings.Repos[0].Repo, settings.PRHeads(branchName))
	if err != nil {
		return result, err
	}

	// --- Step 4: Find or create workspace (self-healing) ---
	wsPath, reused, err := p.workspaces.FindOrCreate(job.TicketKey, settings.Repos[0].CloneURL)
	if err != nil {
		return result, fmt.Errorf("prepare workspace: %w", err)
	}
	logger.Info("Workspace ready",
		zap.String("path", wsPath),
		zap.Bool("reused", reused))

	// --- Step 4a: Set origin to fork and fetch ---
	if err := p.ensureForkRemote(wsPath, settings); err != nil {
		return result, err
	}

	// --- Step 5: Switch to branch and sync with remote ---
	if err := p.git.SwitchBranch(wsPath, branchName); err != nil {
		return result, fmt.Errorf("switch to branch: %w", err)
	}
	if err := p.git.SyncWithRemote(wsPath, branchName, nil); err != nil {
		return result, fmt.Errorf("sync with remote: %w", err)
	}

	// --- Step 6: Fetch and categorize comments + CI analysis ---
	owner := settings.Repos[0].Owner
	repo := settings.Repos[0].Repo
	newComments, addressedComments, ciFailures, err := p.fetchFeedbackContext(
		logger, owner, repo, prDetails)
	if err != nil {
		return result, err
	}
	if len(newComments) == 0 && len(ciFailures) == 0 {
		logger.Info("No new comments or CI failures to address")
		return result, nil
	}

	p.reactToComments(logger, owner, repo, newComments)

	// --- Step 7: Load repo config ---
	repoCfg, err := repoconfig.Load(wsPath)
	if err != nil {
		logger.Warn("Failed to load repo config, using defaults", zap.Error(err))
		repoCfg = repoconfig.Default()
	}

	// --- Step 8: Clone imports ---
	mergedImports, err := p.cloneImports(logger, wsPath, settings, repoCfg)
	if err != nil {
		return result, err
	}

	// --- Step 9: Download attachments, write issue and feedback task files ---
	if err := p.writeFeedbackFiles(
		logger, *workItem, *prDetails, newComments, addressedComments, ciFailures, wsPath, settings,
	); err != nil {
		return result, err
	}

	// --- Step 9a: Remove stale AI outputs from prior session ---
	cleanAIOutputs(logger, wsPath)

	// --- Step 10: Determine AI provider ---
	provider := p.resolveProvider(settings)
	logger.Info("AI provider selected", zap.String("provider", provider))

	// --- Step 11: Build AI command ---
	sp := buildScriptParams(provider, p.cfg.DefaultClaudeModel, p.cfg.DefaultGeminiModel, repoCfg)
	execCommand := buildExecCommand(sp)

	// --- Step 12: Resolve and start container ---
	ctr, err = p.startContainer(ctx, wsPath, job.TicketKey, provider, settings)
	if err != nil {
		return result, fmt.Errorf("start container: %w", err)
	}

	// --- Step 12a: Run import install commands inside container ---
	if err := p.runImportInstalls(ctx, logger, ctr, mergedImports); err != nil {
		return result, fmt.Errorf("import install: %w", err)
	}

	// --- Step 12b: Strip remote auth before AI execution ---
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

	// --- Step 13: Execute AI agent ---
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

	// --- Step 13a: Restore remote auth ---
	// In fork mode, origin is set to the fork so that SyncWithRemote
	// fetches from the fork (where the API commit was created).
	if err := p.git.RestoreRemoteAuth(wsPath, settings.CommitOwner(), settings.Repos[0].Repo); err != nil {
		return result, fmt.Errorf("restore remote auth: %w", err)
	}
	authStripped = false

	if execErr != nil {
		if execCtx.Err() != nil {
			return result, fmt.Errorf("session timeout exceeded: %w", execErr)
		}
		return result, fmt.Errorf("AI session failed: %w", execErr)
	}

	// --- Step 14: Check for changes ---
	hasChanges, err := p.git.HasChanges(wsPath, settings.Repos[0].BaseBranch)
	if err != nil {
		return result, fmt.Errorf("check changes: %w", err)
	}
	if !hasChanges {
		return p.handleNoChanges(logger, settings, prDetails, newComments, ciFailures, wsPath, result, exitCode, job.AttemptNum)
	}

	// --- Step 15: Commit via GitHub API ---
	importExcludes := collectExcludes(mergedImports)
	commitMsg := fmt.Sprintf("%s: address PR feedback", job.TicketKey)
	sha, err := p.git.CommitChanges(
		settings.Repos[0].Owner, settings.CommitOwner(), settings.Repos[0].Repo, branchName,
		commitMsg, wsPath, settings.Repos[0].BaseBranch, workItem.Assignee, importExcludes,
	)
	if errors.Is(err, services.ErrNoChanges) {
		return p.handleErrNoChanges(logger, settings, prDetails, newComments, result, exitCode, job.AttemptNum)
	}
	if err != nil {
		return result, fmt.Errorf("commit changes: %w", err)
	}

	// --- Step 16: Post-commit sync ---
	if err := p.git.SyncWithRemote(wsPath, branchName, importExcludes); err != nil {
		return result, fmt.Errorf("sync with remote: %w", err)
	}

	// --- Step 17: Clear failure labels and reply to addressed comments ---
	p.clearFailureLabels(logger, job.TicketKey, settings.FailureLabels)
	aiResponses := readCommentResponses(wsPath)
	p.replyToComments(logger, settings, prDetails, newComments, sha, aiResponses) // best-effort: commit is the primary outcome

	// --- Step 17a: Post CI fix attempt marker ---
	p.postCIFixMarker(logger, owner, repo, prDetails.Number, ciFailures, sha)

	p.postOrUpdateCostComment(logger,
		settings.Repos[0].Owner, settings.Repos[0].Repo,
		prDetails.Number, result.CostUSD, "Feedback", job.AttemptNum)

	// --- Step 17b: Apply or clear PR validation labels ---
	vlTarget := validationLabel(session, exitCode, settings.PRValidationLabels)
	if vlTarget != "" {
		p.setPRValidationLabel(logger, owner, repo, prDetails.Number,
			settings.PRValidationLabels, vlTarget)
	} else {
		p.clearPRValidationLabels(logger, owner, repo, prDetails.Number,
			settings.PRValidationLabels)
	}

	result.PRURL = prDetails.URL
	result.PRNumber = prDetails.Number
	result.ValidationPassed = validationPassed(session, exitCode)

	logger.Info("Feedback processed",
		zap.String("url", prDetails.URL),
		zap.Int("number", prDetails.Number),
		zap.Int("new_comments_addressed", len(newComments)))

	return result, nil
}

// repoPRInfo groups a repo's PR details and categorized comments for
// multi-repo feedback processing.
type repoPRInfo struct {
	repo       models.RepoSettings
	pr         *models.PRDetails
	rawCmts    []models.PRComment
	newCmts    []models.PRComment
	addrCmts   []models.PRComment
	ciFailures []models.CheckRunFailure
}

// executeMultiRepoFeedback handles feedback for workspaces with
// multiple repositories. It finds PRs across all repos, aggregates
// comments, runs one AI session, then fans out commits and replies.
func (p *Pipeline) executeMultiRepoFeedback(
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

	// --- Step 3: Prepare multi-repo workspace ---
	wsPath, _, err := p.prepareMultiRepoWorkspace(logger, job.TicketKey, settings)
	if err != nil {
		return result, err
	}

	// --- Step 4: Find PRs across all repos ---
	branchName := fmt.Sprintf("%s/%s", p.cfg.BotUsername, job.TicketKey)
	heads := settings.PRHeads(branchName)
	var repoInfos []repoPRInfo
	for _, repo := range settings.Repos {
		pr, err := p.findPRByHeads(repo.Owner, repo.Repo, heads)
		if err != nil {
			logger.Debug("No PR found for repo",
				zap.String("repo", repo.Name))
			continue
		}
		repoInfos = append(repoInfos, repoPRInfo{repo: repo, pr: pr})
	}
	if len(repoInfos) == 0 {
		return result, fmt.Errorf("no PRs found for branch %s in any repository", branchName)
	}

	// --- Step 5: Per-repo branch setup (only repos with PRs) ---
	if err := p.syncMultiRepoBranches(wsPath, branchName, settings, repoInfos); err != nil {
		return result, err
	}

	// --- Step 6: Fetch and categorize comments per repo ---
	allNew, allAddressed, err := p.fetchMultiRepoComments(repoInfos)
	if err != nil {
		return result, err
	}

	// --- Step 6a: CI failure analysis per repo ---
	allCIFailures := p.analyzeMultiRepoCIFailures(logger, repoInfos)

	if len(allNew) == 0 && len(allCIFailures) == 0 {
		logger.Info("No new comments or CI failures to address across any repo")
		return result, nil
	}

	for _, ri := range repoInfos {
		p.reactToComments(logger, ri.repo.Owner, ri.repo.Repo, ri.newCmts)
	}

	// --- Step 7: Load repo configs and merge imports ---
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

	mergedImports := mergeMultiRepoImports(settings, repoConfigs)
	if err := p.cloneImportEntries(logger, wsPath, mergedImports); err != nil {
		return result, err
	}

	// --- Step 8: Write issue and feedback task files ---
	if err := p.writeMultiRepoFeedbackFiles(
		logger, *workItem, repoInfos[0].pr, allNew, allAddressed, allCIFailures, wsPath, settings,
	); err != nil {
		return result, err
	}

	// --- Step 8a: Remove stale AI outputs from prior session ---
	cleanAIOutputs(logger, wsPath)

	// --- Step 9: Provider, command, container ---
	provider := p.resolveProvider(settings)
	sp := buildScriptParams(provider, p.cfg.DefaultClaudeModel, p.cfg.DefaultGeminiModel, repoConfigs[0])
	execCommand := buildExecCommand(sp)

	ctr, err = p.startContainer(ctx, wsPath, job.TicketKey, provider, settings)
	if err != nil {
		return result, fmt.Errorf("start container: %w", err)
	}

	if err := p.runImportInstalls(ctx, logger, ctr, mergedImports); err != nil {
		return result, fmt.Errorf("import install: %w", err)
	}

	// --- Step 10: Strip remote auth per repo ---
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

	// --- Step 11: Execute AI agent ---
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

	// --- Step 11a: Restore remote auth per repo ---
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

	// --- Steps 12-14: Check changes, commit, reply ---
	importExcludes := collectExcludes(mergedImports)
	repoSHAs, err := p.commitMultiRepoFeedback(logger, commitMultiRepoParams{
		settings:     settings,
		workItem:     workItem,
		wsPath:       wsPath,
		branchName:   branchName,
		ticketKey:    job.TicketKey,
		excludes:     importExcludes,
		exitCode:     exitCode,
		finalAttempt: p.isFinalAttempt(job.AttemptNum),
		repoInfos:    repoInfos,
	})

	// Record CI fix attempt per-repo using the actual commit SHA.
	for _, ri := range repoInfos {
		sha := repoSHAs[ri.repo.Name]
		if sha == "" {
			sha = "no-changes"
		}
		p.postCIFixMarker(logger,
			ri.repo.Owner, ri.repo.Repo,
			ri.pr.Number, ri.ciFailures, sha)
	}

	costLabel := feedbackCostLabel(err, len(repoSHAs), p.isFinalAttempt(job.AttemptNum))
	p.postOrUpdateCostComment(logger,
		repoInfos[0].repo.Owner, repoInfos[0].repo.Repo,
		repoInfos[0].pr.Number, result.CostUSD, costLabel, job.AttemptNum)
	p.postCostCrossReference(logger, costCrossRefFromRepoInfos(repoInfos))

	if err != nil {
		return result, err
	}
	if len(repoSHAs) == 0 {
		return result, nil
	}

	p.clearFailureLabels(logger, job.TicketKey, settings.FailureLabels)

	// Apply or clear PR validation labels only on repos that received a commit.
	vlTarget := validationLabel(session, exitCode, settings.PRValidationLabels)
	for _, ri := range repoInfos {
		if repoSHAs[ri.repo.Name] == "" {
			continue
		}
		if vlTarget != "" {
			p.setPRValidationLabel(logger, ri.repo.Owner, ri.repo.Repo,
				ri.pr.Number, settings.PRValidationLabels, vlTarget)
		} else {
			p.clearPRValidationLabels(logger, ri.repo.Owner, ri.repo.Repo,
				ri.pr.Number, settings.PRValidationLabels)
		}
	}

	result.PRURL = repoInfos[0].pr.URL
	result.PRNumber = repoInfos[0].pr.Number
	result.ValidationPassed = validationPassed(session, exitCode)

	logger.Info("Multi-repo feedback processed",
		zap.Int("repos_with_prs", len(repoInfos)),
		zap.Int("new_comments_addressed", len(allNew)))

	return result, nil
}

// syncMultiRepoBranches sets up fork remotes, switches to the branch,
// and syncs with the remote for repos that have PRs.
func (p *Pipeline) syncMultiRepoBranches(
	wsPath, branchName string,
	settings *models.ProjectSettings,
	repoInfos []repoPRInfo,
) error {
	for _, ri := range repoInfos {
		repoDir := filepath.Join(wsPath, ri.repo.Name)
		if err := p.ensureForkRemoteForRepo(repoDir, settings, ri.repo); err != nil {
			return err
		}
		if err := p.git.SwitchBranch(repoDir, branchName); err != nil {
			return fmt.Errorf("switch to branch in %s: %w", ri.repo.Name, err)
		}
		if err := p.git.SyncWithRemote(repoDir, branchName, nil); err != nil {
			return fmt.Errorf("sync with remote for %s: %w", ri.repo.Name, err)
		}
	}
	return nil
}

// fetchMultiRepoComments fetches and categorizes PR comments across
// all repos with PRs. Updates each repoPRInfo with its categorized
// comments and returns the aggregated new and addressed lists.
func (p *Pipeline) fetchMultiRepoComments(
	repoInfos []repoPRInfo,
) (allNew, allAddressed []models.PRComment, err error) {
	for i := range repoInfos {
		ri := &repoInfos[i]
		comments, err := p.git.GetPRComments(
			ri.repo.Owner, ri.repo.Repo, ri.pr.Number, time.Time{})
		if err != nil {
			return nil, nil, fmt.Errorf("get PR comments for %s: %w", ri.repo.Name, err)
		}
		ri.rawCmts = comments
		filtered := commentfilter.Filter(comments, p.commentFilterConfig())
		ri.newCmts, ri.addrCmts = CategorizeComments(filtered, p.cfg.BotUsername)
		allNew = append(allNew, ri.newCmts...)
		allAddressed = append(allAddressed, ri.addrCmts...)
	}
	return allNew, allAddressed, nil
}

// writeMultiRepoFeedbackFiles downloads attachments and writes the
// issue and feedback task files for a multi-repo workspace.
func (p *Pipeline) writeMultiRepoFeedbackFiles(
	logger *zap.Logger,
	workItem models.WorkItem,
	pr *models.PRDetails,
	newComments, addressedComments []models.PRComment,
	ciFailures []models.CheckRunFailure,
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

	repoContexts := make([]taskfile.RepoContext, len(settings.Repos))
	for i, repo := range settings.Repos {
		repoContexts[i] = taskfile.RepoContext{
			Name:                     repo.Name,
			Dir:                      filepath.Join(wsPath, repo.Name),
			OverrideInstructions:     repo.Instructions,
			OverrideFeedbackWorkflow: repo.FeedbackWorkflow,
		}
	}
	if err := p.taskWriter.WriteMultiRepoFeedbackTask(
		*pr, newComments, addressedComments, ciFailures, wsPath, repoContexts,
	); err != nil {
		return fmt.Errorf("write task file: %w", err)
	}
	return nil
}

// writeFeedbackFiles downloads attachments and writes the issue and
// feedback task files for a single-repo feedback pipeline run.
func (p *Pipeline) writeFeedbackFiles(
	logger *zap.Logger,
	workItem models.WorkItem,
	prDetails models.PRDetails,
	newComments, addressedComments []models.PRComment,
	ciFailures []models.CheckRunFailure,
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
	if err := p.taskWriter.WriteFeedbackTask(
		prDetails, newComments, addressedComments, ciFailures, wsPath,
		settings.Repos[0].Instructions, settings.Repos[0].FeedbackWorkflow,
	); err != nil {
		return fmt.Errorf("write task file: %w", err)
	}
	return nil
}

type commitMultiRepoParams struct {
	settings     *models.ProjectSettings
	workItem     *models.WorkItem
	wsPath       string
	branchName   string
	ticketKey    string
	excludes     []string
	exitCode     int
	finalAttempt bool
	repoInfos    []repoPRInfo
}

// commitMultiRepoFeedback checks for changes across repos, commits
// them, syncs with remotes, and replies to PR comments. Returns a
// per-repo map of commit SHAs (nil when no commits were made;
// final-attempt replies are posted and nil error returned in that case).
// On mid-loop errors the partial map is returned alongside the error
// so callers can post accurate CI fix markers for repos that committed
// before the failure.
func (p *Pipeline) commitMultiRepoFeedback(
	logger *zap.Logger,
	params commitMultiRepoParams,
) (map[string]string, error) {
	// Check for any changes across repos that have PRs.
	repoHasChanges := make([]bool, len(params.repoInfos))
	anyChanges := false
	for i, ri := range params.repoInfos {
		repoDir := filepath.Join(params.wsPath, ri.repo.Name)
		has, err := p.git.HasChanges(repoDir, ri.repo.BaseBranch)
		if err != nil {
			return nil, fmt.Errorf("check changes for %s: %w", ri.repo.Name, err)
		}
		repoHasChanges[i] = has
		if has {
			anyChanges = true
		}
	}
	if !anyChanges {
		aiResponses := readCommentResponses(params.wsPath)
		if aiResponses != nil {
			logger.Info("AI produced no code changes but provided comment responses")
			totalPosted := 0
			for _, ri := range params.repoInfos {
				totalPosted += p.replyToCommentsOnRepo(logger, ri.repo.Owner, ri.repo.Repo,
					ri.pr, ri.newCmts, "", aiResponses)
			}
			totalComments := 0
			for _, ri := range params.repoInfos {
				totalComments += len(ri.newCmts)
			}
			if totalPosted == 0 && totalComments > 0 {
				return nil, fmt.Errorf("AI provided comment responses but failed to post any replies")
			}
			return nil, nil
		}
		if params.finalAttempt {
			logger.Info("Final attempt produced no changes, posting unable-to-address replies")
			totalPosted := 0
			totalComments := 0
			for _, ri := range params.repoInfos {
				totalPosted += p.replyToCommentsOnRepo(logger, ri.repo.Owner, ri.repo.Repo,
					ri.pr, ri.newCmts, "unable")
				totalComments += len(ri.newCmts)
			}
			if totalPosted == 0 && totalComments > 0 {
				return nil, fmt.Errorf("final attempt: failed to post unable-to-address replies")
			}
			return nil, nil
		}
		return nil, fmt.Errorf("AI produced no changes (exit code: %d)", params.exitCode)
	}

	// Commit per repo.
	commitMsg := fmt.Sprintf("%s: address PR feedback", params.ticketKey)
	repoSHAs := make(map[string]string)

	for i, ri := range params.repoInfos {
		if !repoHasChanges[i] {
			continue
		}
		repoDir := filepath.Join(params.wsPath, ri.repo.Name)

		sha, err := p.git.CommitChanges(
			ri.repo.Owner, params.settings.CommitOwnerFor(ri.repo), ri.repo.Repo, params.branchName,
			commitMsg, repoDir, ri.repo.BaseBranch, params.workItem.Assignee, params.excludes,
		)
		if errors.Is(err, services.ErrNoChanges) {
			continue
		}
		if err != nil {
			return repoSHAs, fmt.Errorf("commit changes for %s: %w", ri.repo.Name, err)
		}
		repoSHAs[ri.repo.Name] = sha

		if err := p.git.SyncWithRemote(repoDir, params.branchName, params.excludes); err != nil {
			return repoSHAs, fmt.Errorf("sync with remote for %s: %w", ri.repo.Name, err)
		}
	}

	if len(repoSHAs) == 0 {
		if params.finalAttempt {
			logger.Info("Final attempt produced no committable changes, posting unable-to-address replies")
			totalPosted := 0
			totalComments := 0
			for _, ri := range params.repoInfos {
				totalPosted += p.replyToCommentsOnRepo(logger, ri.repo.Owner, ri.repo.Repo,
					ri.pr, ri.newCmts, "unable")
				totalComments += len(ri.newCmts)
			}
			if totalPosted == 0 && totalComments > 0 {
				return nil, fmt.Errorf("final attempt: failed to post unable-to-address replies")
			}
			return nil, nil
		}
		return nil, fmt.Errorf("AI produced no committable changes (exit code: %d)", params.exitCode)
	}

	// Reply to comments using the first committed SHA as the reference.
	aiResponses := readCommentResponses(params.wsPath)
	var firstSHA string
	for _, ri := range params.repoInfos {
		if sha, ok := repoSHAs[ri.repo.Name]; ok {
			firstSHA = sha
			break
		}
	}
	shortSHA := firstSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	for _, ri := range params.repoInfos {
		p.replyToCommentsOnRepo(logger, ri.repo.Owner, ri.repo.Repo,
			ri.pr, ri.newCmts, shortSHA, aiResponses)
	}

	return repoSHAs, nil
}

// ensureForkRemoteForRepo sets a repo's origin to the assignee's fork
// and fetches its refs. No-op when fork mode is inactive.
func (p *Pipeline) ensureForkRemoteForRepo(
	repoDir string,
	settings *models.ProjectSettings,
	repo models.RepoSettings,
) error {
	if settings.ForkOwner() == "" {
		return nil
	}
	if err := p.git.RestoreRemoteAuth(repoDir, settings.CommitOwnerFor(repo), repo.Repo); err != nil {
		return fmt.Errorf("set fork remote for %s: %w", repo.Name, err)
	}
	if err := p.git.FetchRemote(repoDir); err != nil {
		return fmt.Errorf("fetch fork for %s: %w", repo.Name, err)
	}
	return nil
}

// replyToCommentsOnRepo posts replies to comments on a specific
// repo's PR. When sha is "unable", posts unable-to-address replies.
// When sha is a commit SHA, posts addressed replies with optional AI
// response summaries. Returns the number of replies successfully posted.
func (p *Pipeline) replyToCommentsOnRepo(
	logger *zap.Logger,
	owner, repo string,
	pr *models.PRDetails,
	comments []models.PRComment,
	sha string,
	aiResponses ...map[int64]string,
) int {
	var responses map[int64]string
	if len(aiResponses) > 0 {
		responses = aiResponses[0]
	}

	posted := 0
	for _, c := range comments {
		var replyBody string
		switch {
		case sha == "unable":
			replyBody = "I was unable to produce code changes to address this comment after multiple attempts."
		case sha != "" && responses[c.ID] != "":
			replyBody = fmt.Sprintf("%s\n\nAddressed in %s.", responses[c.ID], sha)
		case sha != "":
			replyBody = fmt.Sprintf("Addressed in %s.", sha)
		case responses[c.ID] != "":
			replyBody = responses[c.ID]
		default:
			replyBody = "Reviewed — no code changes needed."
		}

		if c.IsReviewComment {
			if err := p.git.ReplyToComment(owner, repo, pr.Number, c.ID, replyBody); err != nil {
				logger.Warn("Failed to reply to review comment",
					zap.Int64("comment_id", c.ID),
					zap.Error(err))
			} else {
				posted++
			}
		} else {
			contextual := conversationReplyBody(c, replyBody)
			markedBody := fmt.Sprintf("%s\n%s", contextual, commentfilter.AddressedMarker(c.ID))
			if _, err := p.git.PostIssueComment(owner, repo, pr.Number, markedBody); err != nil {
				logger.Warn("Failed to reply to conversation comment",
					zap.Int64("comment_id", c.ID),
					zap.Error(err))
			} else {
				posted++
			}
		}
	}
	return posted
}

// CategorizeComments separates PR comments into new (requiring action)
// and addressed (bot has already replied). Bot's own comments are
// excluded from both lists.
//
// A review comment is "addressed" when the bot has a threaded reply
// to it (InReplyTo match). A conversation comment is "addressed"
// when the bot has posted a comment containing an addressed marker
// (<!-- addressed: ID -->) referencing it.
//
// Both returned slices are non-nil (empty slices, not nil).
func CategorizeComments(comments []models.PRComment, botUsername string) (newComments, addressed []models.PRComment) {
	normBot := normalizeUsername(botUsername)
	botRepliedTo := commentfilter.BotRepliedTo(comments, normBot)

	// Categorize non-bot comments.
	for _, c := range comments {
		if normalizeUsername(c.Author.Username) == normBot {
			continue
		}
		if botRepliedTo[c.ID] {
			addressed = append(addressed, c)
		} else {
			newComments = append(newComments, c)
		}
	}

	// Normalize nil slices.
	if newComments == nil {
		newComments = []models.PRComment{}
	}
	if addressed == nil {
		addressed = []models.PRComment{}
	}

	return newComments, addressed
}

func (p *Pipeline) commentFilterConfig() commentfilter.Config {
	return commentfilter.Config{
		BotUsername:       p.cfg.BotUsername,
		IgnoredUsernames:  p.cfg.IgnoredUsernames,
		KnownBotUsernames: p.cfg.KnownBotUsernames,
		MaxThreadDepth:    p.cfg.MaxThreadDepth,
	}
}

// ensureForkRemote sets the workspace origin to the assignee's fork
// and fetches its refs so the PR branch is available locally. This is
// a no-op when fork mode is not active (GitHubUsername is empty).
func (p *Pipeline) ensureForkRemote(wsPath string, settings *models.ProjectSettings) error {
	if settings.ForkOwner() == "" {
		return nil
	}
	if err := p.git.RestoreRemoteAuth(wsPath, settings.CommitOwner(), settings.Repos[0].Repo); err != nil {
		return fmt.Errorf("set fork remote: %w", err)
	}
	if err := p.git.FetchRemote(wsPath); err != nil {
		return fmt.Errorf("fetch fork: %w", err)
	}
	return nil
}

// handleFeedbackFailure posts an error comment on feedback failure.
// Unlike [Pipeline.handleFailure] for new tickets, feedback failures
// do not revert the ticket status (it stays "in review").
func (p *Pipeline) handleFeedbackFailure(
	logger *zap.Logger,
	ticketKey string,
	settings *models.ProjectSettings,
	jobErr error,
) {
	allLabels := models.AllPipelineLabels(settings.FailureLabels, settings.LifecycleLabels)
	p.setPipelineLabel(logger, ticketKey, allLabels, settings.FailureLabels.Blocked)

	if settings.DisableErrorComments {
		return
	}

	comment := fmt.Sprintf("AI feedback processing failed: %s", jobErr.Error())
	if err := p.tracker.AddComment(ticketKey, comment); err != nil {
		logger.Error("Failed to post error comment", zap.Error(err))
	}
}

// reactToComments adds an eyes emoji reaction to each comment to signal
// that the bot has noticed the feedback and is working on it. Failures
// are logged but not fatal — reactions are best-effort.
func (p *Pipeline) reactToComments(logger *zap.Logger, owner, repo string, comments []models.PRComment) {
	for _, c := range comments {
		if err := p.git.AddCommentReaction(owner, repo, c, "eyes"); err != nil {
			logger.Warn("Failed to add reaction to comment",
				zap.Int64("comment_id", c.ID),
				zap.Error(err))
		}
	}
}

// replyToComments posts a reply to each comment that was processed.
//
// When the AI provides a per-comment response summary (via
// comment-responses.json), the reply includes that summary alongside
// the commit reference. Otherwise, a generic "Addressed in <sha>"
// reply is used.
//
// Review comments are replied to via the threaded review comment API.
// Conversation comments are replied to via a new issue comment that
// includes a machine-readable marker for deduplication.
//
// Failures are logged but not fatal.
func (p *Pipeline) replyToComments(
	logger *zap.Logger,
	settings *models.ProjectSettings,
	prDetails *models.PRDetails,
	comments []models.PRComment,
	commitSHA string,
	aiResponses map[int64]string,
) int {
	shortSHA := commitSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	posted := 0
	for _, c := range comments {
		var replyBody string
		if summary, ok := aiResponses[c.ID]; ok {
			if shortSHA != "" {
				replyBody = fmt.Sprintf("%s\n\nAddressed in %s.", summary, shortSHA)
			} else {
				replyBody = summary
			}
		} else if shortSHA != "" {
			replyBody = fmt.Sprintf("Addressed in %s.", shortSHA)
		} else {
			replyBody = "Reviewed — no code changes needed."
		}

		if c.IsReviewComment {
			if err := p.git.ReplyToComment(
				settings.Repos[0].Owner, settings.Repos[0].Repo, prDetails.Number, c.ID, replyBody); err != nil {
				logger.Warn("Failed to reply to review comment",
					zap.Int64("comment_id", c.ID),
					zap.Error(err))
			} else {
				posted++
			}
		} else {
			contextual := conversationReplyBody(c, replyBody)
			markedBody := fmt.Sprintf("%s\n%s", contextual, commentfilter.AddressedMarker(c.ID))
			if _, err := p.git.PostIssueComment(
				settings.Repos[0].Owner, settings.Repos[0].Repo, prDetails.Number, markedBody); err != nil {
				logger.Warn("Failed to reply to conversation comment",
					zap.Int64("comment_id", c.ID),
					zap.Error(err))
			} else {
				posted++
			}
		}
	}
	return posted
}

// isFinalAttempt returns true when the current attempt is the last
// one before the job manager stops retrying. This accounts for the
// coordinator's check (failureCounts > maxRetries), which allows
// maxRetries+1 total attempts.
func (p *Pipeline) isFinalAttempt(attemptNum int) bool {
	return p.cfg.MaxRetries >= 0 && attemptNum > p.cfg.MaxRetries
}

// replyUnableToAddress posts a reply to each comment indicating that
// the bot was unable to make changes after multiple attempts. The
// reply includes an addressed marker so the comment is not picked up
// again by future scanner cycles. Returns the number of replies
// successfully posted.
func (p *Pipeline) replyUnableToAddress(
	logger *zap.Logger,
	settings *models.ProjectSettings,
	prDetails *models.PRDetails,
	comments []models.PRComment,
) int {
	posted := 0
	for _, c := range comments {
		replyBody := "I was unable to produce code changes to address this comment after multiple attempts."

		if c.IsReviewComment {
			if err := p.git.ReplyToComment(
				settings.Repos[0].Owner, settings.Repos[0].Repo, prDetails.Number, c.ID, replyBody); err != nil {
				logger.Warn("Failed to reply to review comment",
					zap.Int64("comment_id", c.ID),
					zap.Error(err))
			} else {
				posted++
			}
		} else {
			contextual := conversationReplyBody(c, replyBody)
			markedBody := fmt.Sprintf("%s\n%s", contextual, commentfilter.AddressedMarker(c.ID))
			if _, err := p.git.PostIssueComment(
				settings.Repos[0].Owner, settings.Repos[0].Repo, prDetails.Number, markedBody); err != nil {
				logger.Warn("Failed to reply to conversation comment",
					zap.Int64("comment_id", c.ID),
					zap.Error(err))
			} else {
				posted++
			}
		}
	}
	return posted
}

// conversationReplyBody builds a reply to a conversation comment with
// context linking back to the original. Review comments don't need
// this because GitHub threads them automatically.
func conversationReplyBody(c models.PRComment, response string) string {
	var b strings.Builder

	if c.URL != "" {
		fmt.Fprintf(&b, "In [comment](%s), @%s said:\n\n", c.URL, c.Author.Username)
	} else {
		fmt.Fprintf(&b, "@%s said:\n\n", c.Author.Username)
	}

	for _, line := range strings.Split(strings.TrimRight(c.Body, "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", line)
	}

	fmt.Fprintf(&b, "\n%s", response)
	return b.String()
}

// normalizeUsername strips the GitHub [bot] suffix and lowercases
// for case-insensitive comparison. Matches the normalization used
// by [commentfilter.Filter].
func normalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSuffix(s, "[bot]"))
}

func (p *Pipeline) analyzeMultiRepoCIFailures(
	logger *zap.Logger, repoInfos []repoPRInfo,
) []models.CheckRunFailure {
	var all []models.CheckRunFailure
	for i := range repoInfos {
		ri := &repoInfos[i]
		ri.ciFailures = p.analyzeCIFailures(logger, ri.repo.Owner, ri.repo.Repo, ri.pr, ri.rawCmts)
		all = append(all, ri.ciFailures...)
	}
	return all
}

func feedbackCostLabel(commitErr error, commitCount int, finalAttempt bool) string {
	switch {
	case commitErr != nil && (strings.Contains(commitErr.Error(), "no changes") ||
		strings.Contains(commitErr.Error(), "no committable changes")):
		return "Feedback (no changes)"
	case commitErr != nil:
		return "Feedback (error)"
	case commitCount == 0 && finalAttempt:
		return "Feedback (unable)"
	case commitCount == 0:
		return "Feedback (no changes)"
	default:
		return "Feedback"
	}
}

func (p *Pipeline) handleErrNoChanges(
	logger *zap.Logger,
	settings *models.ProjectSettings,
	prDetails *models.PRDetails,
	newComments []models.PRComment,
	result jobmanager.JobResult,
	exitCode int,
	attemptNum int,
) (jobmanager.JobResult, error) {
	owner := settings.Repos[0].Owner
	repo := settings.Repos[0].Repo

	if p.isFinalAttempt(attemptNum) {
		logger.Info("Final attempt produced no changes, posting unable-to-address replies")
		posted := p.replyUnableToAddress(logger, settings, prDetails, newComments)
		p.postOrUpdateCostComment(logger, owner, repo, prDetails.Number, result.CostUSD, "Feedback (unable)", attemptNum)
		if posted == 0 && len(newComments) > 0 {
			return result, fmt.Errorf("final attempt: failed to post unable-to-address replies")
		}
		return result, nil
	}
	p.postOrUpdateCostComment(logger, owner, repo, prDetails.Number, result.CostUSD, "Feedback (no changes)", attemptNum)
	return result, fmt.Errorf("AI produced no committable changes (exit code: %d)", exitCode)
}

// cleanAIOutputs removes AI-generated output files from the workspace
// to prevent stale data from a prior session being read as current.
// SessionContextPath is intentionally preserved — it carries design
// context from the original session that helps the AI address feedback.
func cleanAIOutputs(logger *zap.Logger, wsPath string) {
	for _, rel := range []string{
		taskfile.CommentResponsesPath,
		taskfile.PRDescriptionPath,
		sessionOutputPath,
		cliOutputPath,
	} {
		if err := os.Remove(filepath.Join(wsPath, rel)); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Debug("Failed to clean AI output file",
				zap.String("path", rel), zap.Error(err))
		}
	}
}

func (p *Pipeline) handleNoChanges(
	logger *zap.Logger,
	settings *models.ProjectSettings,
	prDetails *models.PRDetails,
	newComments []models.PRComment,
	ciFailures []models.CheckRunFailure,
	wsPath string,
	result jobmanager.JobResult,
	exitCode int,
	attemptNum int,
) (jobmanager.JobResult, error) {
	owner := settings.Repos[0].Owner
	repo := settings.Repos[0].Repo

	// Record CI fix attempt even when no code changes were produced,
	// to prevent infinite retry loops.
	p.postCIFixMarker(logger, owner, repo, prDetails.Number, ciFailures, "no-changes")

	aiResponses := readCommentResponses(wsPath)
	if aiResponses != nil {
		logger.Info("AI produced no code changes but provided comment responses")
		posted := p.replyToComments(logger, settings, prDetails, newComments, "", aiResponses)
		p.postOrUpdateCostComment(logger, owner, repo, prDetails.Number, result.CostUSD, "Feedback (no changes)", attemptNum)
		if posted == 0 && len(newComments) > 0 {
			return result, fmt.Errorf("AI provided comment responses but failed to post any replies")
		}
		return result, nil
	}
	if p.isFinalAttempt(attemptNum) {
		logger.Info("Final attempt produced no changes, posting unable-to-address replies")
		posted := p.replyUnableToAddress(logger, settings, prDetails, newComments)
		p.postOrUpdateCostComment(logger, owner, repo, prDetails.Number, result.CostUSD, "Feedback (unable)", attemptNum)
		if posted == 0 && len(newComments) > 0 {
			return result, fmt.Errorf("final attempt: failed to post unable-to-address replies")
		}
		return result, nil
	}
	p.postOrUpdateCostComment(logger, owner, repo, prDetails.Number, result.CostUSD, "Feedback (no changes)", attemptNum)
	return result, fmt.Errorf("AI produced no changes (exit code: %d)", exitCode)
}

func (p *Pipeline) fetchFeedbackContext(
	logger *zap.Logger,
	owner, repo string,
	prDetails *models.PRDetails,
) (newComments, addressedComments []models.PRComment, ciFailures []models.CheckRunFailure, err error) {
	allComments, err := p.git.GetPRComments(owner, repo, prDetails.Number, time.Time{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get PR comments: %w", err)
	}

	filtered := commentfilter.Filter(allComments, p.commentFilterConfig())
	newComments, addressedComments = CategorizeComments(filtered, p.cfg.BotUsername)
	ciFailures = p.analyzeCIFailures(logger, owner, repo, prDetails, allComments)

	return newComments, addressedComments, ciFailures, nil
}

func (p *Pipeline) postCIFixMarker(
	logger *zap.Logger,
	owner, repo string,
	prNumber int,
	ciFailures []models.CheckRunFailure,
	commitSHA string,
) {
	if len(ciFailures) == 0 {
		return
	}
	marker := commentfilter.CIFixAttemptMarker(ciFailures, commitSHA)
	if _, err := p.git.PostIssueComment(owner, repo, prNumber, marker); err != nil {
		logger.Warn("Failed to post CI fix attempt marker", zap.Error(err))
	}
}

const maxBytesPerStep = 4096

// analyzeCIFailures checks the PR's head commit for actionable CI
// failures. Returns an empty slice if CI checking is disabled, checks
// are pending, failures are ignored, or fix attempts are exhausted.
func (p *Pipeline) analyzeCIFailures(
	logger *zap.Logger,
	owner, repo string,
	pr *models.PRDetails,
	comments []models.PRComment,
) []models.CheckRunFailure {
	if p.cfg.MaxCIFixAttempts == 0 || pr.HeadSHA == "" {
		return nil
	}

	failures, allCompleted, err := p.git.ListCheckRunsForRef(owner, repo, pr.HeadSHA)
	if err != nil {
		logger.Warn("Failed to check CI status", zap.Error(err))
		return nil
	}
	if !allCompleted {
		logger.Debug("CI checks still running, skipping CI analysis")
		return nil
	}

	filtered := filterIgnoredChecks(failures, p.cfg.IgnoredCheckNames)
	if len(filtered) == 0 {
		return nil
	}

	// Filter pre-existing failures (also failing on base branch).
	baseFailures, _, baseErr := p.git.ListCheckRunsForRef(owner, repo, pr.BaseBranch)
	if baseErr == nil {
		filtered = filterPreExistingFailures(filtered, baseFailures)
	}
	if len(filtered) == 0 {
		return nil
	}

	// Check CI fix attempt limit.
	if p.cfg.MaxCIFixAttempts > 0 {
		attempts := commentfilter.CountCIFixAttempts(comments, p.cfg.BotUsername)
		if attempts >= p.cfg.MaxCIFixAttempts {
			logger.Debug("CI fix attempts exhausted",
				zap.Int("attempts", attempts),
				zap.Int("max", p.cfg.MaxCIFixAttempts))
			return nil
		}
	}

	// Enrich failures with annotations and step logs.
	for i := range filtered {
		annotations, err := p.git.ListCheckRunAnnotations(owner, repo, filtered[i].ID)
		if err != nil {
			logger.Warn("Failed to fetch annotations",
				zap.String("check", filtered[i].Name),
				zap.Error(err))
			continue
		}
		filtered[i].Annotations = annotations
	}

	jobLogs, err := p.git.GetFailedJobLogs(owner, repo, pr.HeadSHA, maxBytesPerStep)
	if err != nil {
		logger.Warn("Failed to fetch job logs", zap.Error(err))
	} else {
		for i := range filtered {
			if steps, ok := jobLogs[filtered[i].Name]; ok {
				filtered[i].FailedSteps = steps
			}
		}
	}

	sortCheckRunFailures(filtered)

	logger.Info("Found CI failures to address",
		zap.Int("count", len(filtered)))
	return filtered
}

// filterIgnoredChecks removes check runs whose names match the ignore
// list (case-insensitive).
func filterIgnoredChecks(
	failures []models.CheckRunFailure, ignored []string,
) []models.CheckRunFailure {
	if len(ignored) == 0 {
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

func sortCheckRunFailures(failures []models.CheckRunFailure) {
	sort.Slice(failures, func(i, j int) bool {
		return failures[i].Name < failures[j].Name
	})
}

// filterPreExistingFailures removes failures whose check name also
// appears in base branch failures, since those existed before the PR.
func filterPreExistingFailures(
	prFailures, baseFailures []models.CheckRunFailure,
) []models.CheckRunFailure {
	if len(baseFailures) == 0 {
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

// findPRByHeads tries each candidate head ref and returns the first
// matching PR. Returns an error if no PR is found under any head.
func (p *Pipeline) findPRByHeads(owner, repo string, heads []string) (*models.PRDetails, error) {
	pr, err := p.findPRByHeadsOptional(owner, repo, heads)
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, fmt.Errorf("no open PR found for heads %v", heads)
	}
	return pr, nil
}

// findPRByHeadsOptional tries each candidate head ref and returns the
// first matching PR, or (nil, nil) when no PR matches any head.
func (p *Pipeline) findPRByHeadsOptional(owner, repo string, heads []string) (*models.PRDetails, error) {
	if len(heads) == 0 {
		return nil, errors.New("no candidate heads provided")
	}
	var lastErr error
	for _, head := range heads {
		pr, err := p.git.GetPRForBranch(owner, repo, head)
		if err != nil {
			lastErr = err
			continue
		}
		if pr != nil {
			return pr, nil
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, nil
}
