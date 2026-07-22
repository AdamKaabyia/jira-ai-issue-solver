package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/costtracker"
	"jira-ai-issue-solver/executor"
	"jira-ai-issue-solver/jobmanager"
	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/projectresolver"
	"jira-ai-issue-solver/recovery"
	"jira-ai-issue-solver/scanner"
	"jira-ai-issue-solver/services"
	"jira-ai-issue-solver/services/hosting"
	"jira-ai-issue-solver/taskfile"
	"jira-ai-issue-solver/tracker/jira"
	"jira-ai-issue-solver/workspace"
)

func main() {
	configPath := flag.String("config", "", "Path to configuration file (optional, uses environment variables by default)")
	flag.Parse()

	config, err := models.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load configuration: %v\n", err)
		os.Exit(1)
	}

	logger := initLogger(config)
	defer func() { _ = logger.Sync() }()

	// --- Infrastructure ---

	jiraService := services.NewJiraService(config, logger)

	// Build the VCS hosting router from per-project workspace config.
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

	issueTracker, err := jira.NewAdapter(jiraService, logger)
	if err != nil {
		logger.Fatal("Failed to create issue tracker", zap.Error(err))
	}

	resolver, err := projectresolver.NewConfigResolver(config)
	if err != nil {
		logger.Fatal("Failed to create project resolver", zap.Error(err))
	}

	wsMgr, err := workspace.NewFSManager(config.Workspaces.BaseDir, gitService, logger)
	if err != nil {
		logger.Fatal("Failed to create workspace manager", zap.Error(err))
	}

	// Container runtime detection and manager.
	detected, err := container.DetectRuntime(
		container.Runtime(config.Container.Runtime), nil)
	if err != nil {
		logger.Fatal("Failed to detect container runtime", zap.Error(err))
	}
	logger.Info("Container runtime detected",
		zap.String("runtime", string(detected.Runtime)),
		zap.String("path", detected.Path))

	containerRunner := container.NewCLIRunner(detected)

	containerResolver, err := container.NewResolver(
		container.ResolverDefaults{
			DisableSELinux: config.Container.DisableSELinux,
			UserNS:         config.Container.UserNS,
		},
		logger)
	if err != nil {
		logger.Fatal("Failed to create container resolver", zap.Error(err))
	}

	containerMgr, err := container.NewRuntimeManager(
		containerRunner,
		containerResolver,
		container.RuntimeManagerConfig{
			NamePrefix: config.GitHub.BotUsername,
		},
		logger)
	if err != nil {
		logger.Fatal("Failed to create container manager", zap.Error(err))
	}

	// --- Cost tracking ---

	if err := os.MkdirAll(config.Workspaces.BaseDir, 0o750); err != nil {
		logger.Fatal("Failed to create workspace base directory", zap.Error(err))
	}

	costFile := filepath.Join(config.Workspaces.BaseDir, "daily-cost.json")
	costs, err := costtracker.NewFileTracker(
		costFile,
		config.Guardrails.MaxDailyCostUSD,
		logger)
	if err != nil {
		logger.Fatal("Failed to create cost tracker", zap.Error(err))
	}

	// --- Executor pipeline ---

	aiAPIKeys := make(map[string]string)
	if config.Claude.APIKey != "" {
		aiAPIKeys["claude"] = config.Claude.APIKey
	}
	if config.Gemini.APIKey != "" {
		aiAPIKeys["gemini"] = config.Gemini.APIKey
	}

	var claudeVertex *executor.ClaudeVertexConfig
	if config.Claude.VertexProjectID != "" {
		claudeVertex = &executor.ClaudeVertexConfig{
			ProjectID:       config.Claude.VertexProjectID,
			Region:          config.Claude.VertexRegion,
			CredentialsFile: config.Claude.VertexCredentialsFile,
		}
	}

	pipeline, err := executor.NewPipeline(
		executor.Config{
			BotUsername:        config.GitHub.BotUsername,
			DefaultProvider:    config.AIProvider,
			AIAPIKeys:          aiAPIKeys,
			ClaudeVertex:       claudeVertex,
			SessionTimeout:     time.Duration(config.Guardrails.MaxContainerRuntimeMinutes) * time.Minute,
			IgnoredUsernames:   config.GitHub.IgnoredUsernames,
			KnownBotUsernames:  config.GitHub.KnownBotUsernames,
			MaxThreadDepth:     config.GitHub.MaxThreadDepth,
			DefaultClaudeModel: config.Claude.Model,
			DefaultGeminiModel: config.Gemini.Model,
			MaxRetries:         config.Guardrails.MaxRetries,
			IgnoredCheckNames:  config.GitHub.IgnoredCheckNames,
			MaxCIFixAttempts:   config.Guardrails.MaxCIFixAttempts,
			RetryLabel:         config.Guardrails.RetryLabel,
			JiraUsername:       config.Jira.Username,
			MinCommentLength:   config.Guardrails.MinCommentLength,
			GeminiPricing: executor.GeminiPricing{
				InputPerMTok:  config.Gemini.InputPricePerMTok,
				OutputPerMTok: config.Gemini.OutputPricePerMTok,
				CachedPerMTok: config.Gemini.CachedPricePerMTok,
			},
		},
		issueTracker,
		gitService,
		containerMgr,
		wsMgr,
		taskfile.NewMarkdownWriter(),
		resolver,
		logger,
	)
	if err != nil {
		logger.Fatal("Failed to create executor pipeline", zap.Error(err))
	}

	// --- Job manager ---

	coordinator, err := jobmanager.NewCoordinator(
		jobmanager.Config{
			MaxConcurrent:           config.Guardrails.MaxConcurrentJobs,
			MaxRetries:              config.Guardrails.MaxRetries,
			CircuitBreakerThreshold: config.Guardrails.CircuitBreakerThreshold,
			CircuitBreakerWindow:    time.Duration(config.Guardrails.CircuitBreakerWindowMinutes) * time.Minute,
			CircuitBreakerCooldown:  time.Duration(config.Guardrails.CircuitBreakerCooldownMinutes) * time.Minute,
			CostRecorder:            costs,
		},
		pipeline.Execute,
		logger,
	)
	if err != nil {
		logger.Fatal("Failed to create job coordinator", zap.Error(err))
	}

	// --- Crash recovery ---

	todoCriteria, inReviewCriteria, activeStatuses := buildScanCriteria(config)

	startupRunner, err := recovery.NewStartupRunner(
		recovery.Config{
			ContainerPrefix:    config.GitHub.BotUsername,
			WorkspaceTTL:       time.Duration(config.Workspaces.TTLDays) * 24 * time.Hour,
			BotUsername:        config.GitHub.BotUsername,
			InProgressCriteria: buildInProgressCriteria(config),
			ActiveStatuses:     activeStatuses,
		},
		issueTracker,
		gitService,
		wsMgr,
		containerMgr,
		coordinator,
		resolver,
		logger,
	)
	if err != nil {
		logger.Fatal("Failed to create startup runner", zap.Error(err))
	}

	// Run crash recovery before starting scanners.
	if err := startupRunner.Run(context.Background()); err != nil {
		logger.Warn("Crash recovery returned error", zap.Error(err))
	}

	// --- Scanners ---

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticketScanner, err := scanner.NewWorkItemScanner(
		issueTracker,
		coordinator,
		coordinator,
		issueTracker,
		config.Guardrails.RetryLabel,
		scanner.WorkItemScannerConfig{
			Criteria:     todoCriteria,
			PollInterval: time.Duration(config.Jira.IntervalSeconds) * time.Second,
		},
		logger,
	)
	if err != nil {
		logger.Fatal("Failed to create work item scanner", zap.Error(err))
	}

	feedbackScanner, err := scanner.NewFeedbackScanner(
		issueTracker,
		coordinator,
		gitService,
		resolver,
		gitService,
		scanner.FeedbackScannerConfig{
			Criteria:          inReviewCriteria,
			PollInterval:      time.Duration(config.Jira.IntervalSeconds) * time.Second,
			BotUsername:       config.GitHub.BotUsername,
			IgnoredUsernames:  config.GitHub.IgnoredUsernames,
			KnownBotUsernames: config.GitHub.KnownBotUsernames,
			MaxThreadDepth:    config.GitHub.MaxThreadDepth,
			IgnoredCheckNames: config.GitHub.IgnoredCheckNames,
			MaxCIFixAttempts:  config.Guardrails.MaxCIFixAttempts,
			SkipPRLabel:       config.GitHub.SkipPRLabel,
		},
		logger,
		scanner.WithLabelManager(issueTracker, resolver),
		scanner.WithLifecycleLabelManager(resolver, resolver, issueTracker),
		scanner.WithPRLabeler(gitService),
	)
	if err != nil {
		logger.Fatal("Failed to create feedback scanner", zap.Error(err))
	}

	cleanupScanner, err := scanner.NewWorkspaceCleanupScanner(
		wsMgr,
		issueTracker,
		scanner.WorkspaceCleanupConfig{
			PollInterval:   time.Duration(config.Jira.IntervalSeconds) * time.Second,
			ActiveStatuses: activeStatuses,
		},
		logger,
	)
	if err != nil {
		logger.Fatal("Failed to create workspace cleanup scanner", zap.Error(err))
	}

	mergeScanner, err := scanner.NewMergeScanner(
		issueTracker,
		coordinator,
		gitService,
		resolver,
		gitService,
		gitService,
		scanner.MergeScannerConfig{
			Criteria:          inReviewCriteria,
			PollInterval:      time.Duration(config.Jira.IntervalSeconds) * time.Second,
			BotUsername:       config.GitHub.BotUsername,
			IdleDays:          config.Merge.IdleDays,
			IdleLabel:         config.Merge.IdleLabel,
			IgnoredUsernames:  config.GitHub.IgnoredUsernames,
			KnownBotUsernames: config.GitHub.KnownBotUsernames,
			SkipPRLabel:       config.GitHub.SkipPRLabel,
		},
		logger,
		scanner.WithMergeCommands(),
	)
	if err != nil {
		logger.Fatal("Failed to create merge scanner", zap.Error(err))
	}

	var triageScanner *scanner.TriageLabelScanner
	if triageCriteria, ok := buildTriageCriteria(config); ok {
		triageScanner, err = scanner.NewTriageLabelScanner(
			issueTracker,
			issueTracker,
			resolver,
			scanner.TriageLabelScannerConfig{
				Criteria:     triageCriteria,
				PollInterval: time.Duration(config.Jira.IntervalSeconds) * time.Second,
				BotUsername:  config.Jira.Username,
			},
			logger,
		)
		if err != nil {
			logger.Fatal("Failed to create triage label scanner", zap.Error(err))
		}
	}

	if err := ticketScanner.Start(ctx); err != nil {
		logger.Fatal("Failed to start work item scanner", zap.Error(err))
	}
	if err := feedbackScanner.Start(ctx); err != nil {
		logger.Fatal("Failed to start feedback scanner", zap.Error(err))
	}
	if err := cleanupScanner.Start(ctx); err != nil {
		logger.Fatal("Failed to start workspace cleanup scanner", zap.Error(err))
	}
	if err := mergeScanner.Start(ctx); err != nil {
		logger.Fatal("Failed to start merge scanner", zap.Error(err))
	}
	if triageScanner != nil {
		if err := triageScanner.Start(ctx); err != nil {
			logger.Fatal("Failed to start triage label scanner", zap.Error(err))
		}
	}

	logger.Info("Scanners started")

	// --- HTTP server ---

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "OK")
	})

	port := config.Server.Port
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			port = p
		}
	}

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	go func() {
		logger.Info("Starting server", zap.Int("port", port))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Server error", zap.Error(err))
			stop <- syscall.SIGTERM
		}
	}()

	// --- Graceful shutdown ---

	<-stop

	logger.Info("Shutdown signal received")

	// Stop accepting new work.
	cancel()
	ticketScanner.Stop()
	feedbackScanner.Stop()
	cleanupScanner.Stop()
	mergeScanner.Stop()
	if triageScanner != nil {
		triageScanner.Stop()
	}

	// Drain running jobs.
	coordinator.Shutdown()

	// Shut down HTTP server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server shutdown error", zap.Error(err))
	}

	logger.Info("Shutdown complete")
}

// initLogger creates a structured logger from the application config.
func initLogger(config *models.Config) *zap.Logger {
	level := getLogLevel(config.Logging.Level)

	var encoderConfig zapcore.EncoderConfig
	if config.Logging.Format == models.LogFormatJSON {
		encoderConfig = zap.NewProductionEncoderConfig()
		encoderConfig.TimeKey = "timestamp"
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	} else {
		encoderConfig = zap.NewDevelopmentEncoderConfig()
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}

	var encoder zapcore.Encoder
	if config.Logging.Format == models.LogFormatJSON {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	core := zapcore.NewCore(encoder, zapcore.AddSync(os.Stdout), level)
	return zap.New(core)
}

func getLogLevel(level models.LogLevel) zapcore.Level {
	switch level {
	case models.LogLevelDebug:
		return zapcore.DebugLevel
	case models.LogLevelInfo:
		return zapcore.InfoLevel
	case models.LogLevelWarn:
		return zapcore.WarnLevel
	case models.LogLevelError:
		return zapcore.ErrorLevel
	default:
		return zapcore.InfoLevel
	}
}

// buildScanCriteria constructs the search criteria for both scanners
// and the set of active statuses for workspace cleanup, derived from
// the multi-project configuration.
func buildScanCriteria(config *models.Config) (todo, inReview models.SearchCriteria, activeStatuses map[string]bool) {
	todoByType := make(map[string][]string)
	inReviewByType := make(map[string][]string)
	activeStatuses = make(map[string]bool)
	var projectKeys []string

	for _, project := range config.Jira.Projects {
		projectKeys = append(projectKeys, project.ProjectKeys...)

		for ticketType, transitions := range project.StatusTransitions {
			todoByType[ticketType] = appendUnique(todoByType[ticketType], transitions.Todo)
			inReviewByType[ticketType] = appendUnique(inReviewByType[ticketType], transitions.InReview)

			activeStatuses[transitions.Todo] = true
			activeStatuses[transitions.InProgress] = true
			activeStatuses[transitions.InReview] = true
		}
	}

	todo = models.SearchCriteria{
		ProjectKeys:              projectKeys,
		StatusByType:             todoByType,
		ContributorIsCurrentUser: true,
	}

	inReview = models.SearchCriteria{
		ProjectKeys:              projectKeys,
		StatusByType:             inReviewByType,
		ContributorIsCurrentUser: true,
	}

	return todo, inReview, activeStatuses
}

// buildInProgressCriteria constructs the search criteria for finding
// tickets stuck in "in progress" during crash recovery.
func buildInProgressCriteria(config *models.Config) models.SearchCriteria {
	inProgressByType := make(map[string][]string)
	var projectKeys []string

	for _, project := range config.Jira.Projects {
		projectKeys = append(projectKeys, project.ProjectKeys...)

		for ticketType, transitions := range project.StatusTransitions {
			inProgressByType[ticketType] = appendUnique(
				inProgressByType[ticketType], transitions.InProgress)
		}
	}

	return models.SearchCriteria{
		ProjectKeys:              projectKeys,
		StatusByType:             inProgressByType,
		ContributorIsCurrentUser: true,
	}
}

// buildTriageCriteria constructs search criteria for the triage label
// cleanup scanner. Returns false if no project has triage labels
// configured.
func buildTriageCriteria(config *models.Config) (models.SearchCriteria, bool) {
	var projectKeys []string
	var labels []string

	for _, project := range config.Jira.Projects {
		if len(project.TriageLabels.Active) == 0 {
			continue
		}
		projectKeys = append(projectKeys, project.ProjectKeys...)
		for _, label := range project.TriageLabels.Active {
			labels = appendUnique(labels, label)
		}
	}

	if len(projectKeys) == 0 || len(labels) == 0 {
		return models.SearchCriteria{
			ProjectKeys: []string{},
			Labels:      []string{},
		}, false
	}

	return models.SearchCriteria{
		ProjectKeys: projectKeys,
		Labels:      labels,
		OrderBy:     "updated DESC",
	}, true
}

// appendUnique appends value to slice only if not already present.
func appendUnique(slice []string, value string) []string {
	for _, v := range slice {
		if v == value {
			return slice
		}
	}
	return append(slice, value)
}

// buildRepoProviderMap iterates all configured projects/workspaces and
// maps each repo URL's owner/repo to the correct hosting provider.
func buildRepoProviderMap(
	config *models.Config,
	ghService *services.GitHubServiceImpl,
	glService *services.GitLabServiceImpl,
) map[string]hosting.Provider {
	providers := make(map[string]hosting.Provider)

	for _, project := range config.Jira.Projects {
		for _, ws := range project.Workspaces {
			var provider hosting.Provider
			switch ws.HostingProvider() {
			case models.HostingGitLab:
				provider = glService
			default:
				provider = ghService
			}
			for _, repo := range ws.Repos {
				owner, repoName := extractOwnerRepoFromURL(repo.URL)
				if owner != "" && repoName != "" {
					key := owner + "/" + repoName
					providers[key] = provider
				}
			}
		}
	}

	return providers
}

// extractOwnerRepoFromURL parses owner and repo from a clone URL.
func extractOwnerRepoFromURL(repoURL string) (string, string) {
	repoURL = strings.TrimSuffix(repoURL, ".git")
	parts := strings.Split(repoURL, "/")
	if len(parts) >= 5 {
		repo := parts[len(parts)-1]
		owner := strings.Join(parts[3:len(parts)-1], "/")
		return owner, repo
	}
	return "", ""
}
