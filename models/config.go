package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// LogLevel represents the logging level
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// LogFormat represents the logging format
type LogFormat string

const (
	LogFormatConsole LogFormat = "console"
	LogFormatJSON    LogFormat = "json"
)

// String returns the string representation of LogLevel
func (l LogLevel) String() string {
	return string(l)
}

// String returns the string representation of LogFormat
func (f LogFormat) String() string {
	return string(f)
}

// IsValid checks if the LogLevel is valid
func (l LogLevel) IsValid() bool {
	switch l {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		return true
	default:
		return false
	}
}

// IsValid checks if the LogFormat is valid
func (f LogFormat) IsValid() bool {
	switch f {
	case LogFormatConsole, LogFormatJSON:
		return true
	default:
		return false
	}
}

// UnmarshalYAML implements custom unmarshaling for LogLevel
func (l *LogLevel) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	level := LogLevel(strings.ToLower(str))
	if !level.IsValid() {
		return fmt.Errorf("invalid log level: %s. Valid options are: debug, info, warn, error", str)
	}

	*l = level
	return nil
}

// UnmarshalYAML implements custom unmarshaling for LogFormat
func (f *LogFormat) UnmarshalYAML(value *yaml.Node) error {
	var str string
	if err := value.Decode(&str); err != nil {
		return err
	}

	format := LogFormat(strings.ToLower(str))
	if !format.IsValid() {
		return fmt.Errorf("invalid log format: %s. Valid options are: console, json", str)
	}

	*f = format
	return nil
}

// Profile bundles container, imports, instructions, and workflow
// settings for a group of components. Multiple components can reference
// the same profile to avoid config duplication. Operator-defined
// profiles take precedence over repo-level config on conflicts.
type Profile struct {
	Container         ContainerSettings `yaml:"container" mapstructure:"container"`
	Imports           []ImportConfig    `yaml:"imports" mapstructure:"imports"`
	Instructions      string            `yaml:"instructions" mapstructure:"instructions"`
	NewTicketWorkflow string            `yaml:"new_ticket_workflow" mapstructure:"new_ticket_workflow"`
	FeedbackWorkflow  string            `yaml:"feedback_workflow" mapstructure:"feedback_workflow"`
}

// WorkspaceConfig defines a named workspace — a group of one or more
// repositories that constitute a working environment for AI sessions.
// Single-repo projects are just workspaces with one entry.
type WorkspaceConfig struct {
	// Hosting selects the code hosting provider for this workspace.
	// Valid values: "github" (default), "gitlab". All repos in the
	// workspace must reside on the same hosting provider.
	Hosting string `yaml:"hosting" mapstructure:"hosting"`

	// Container holds workspace-level container settings. Required for
	// multi-repo workspaces (the "fat container" with all toolchains).
	// For single-repo workspaces, this may be empty — the repo's
	// profile container is used instead.
	Container ContainerSettings `yaml:"container" mapstructure:"container"`

	// RootRepo is an optional clone URL for a scaffold repository that
	// is cloned as the workspace root before child repos are cloned
	// into subdirectories. The scaffold provides context files
	// (e.g., CLAUDE.md, AI skills) but is never branched, committed
	// to, or PR'd.
	RootRepo string `yaml:"root_repo" mapstructure:"root_repo"`

	// Repos lists the repositories in this workspace.
	Repos []RepoEntry `yaml:"repos" mapstructure:"repos"`
}

// RepoEntry associates a repository with a profile within a workspace.
type RepoEntry struct {
	// Name is a short identifier used as the subdirectory name in
	// multi-repo workspaces (e.g., "fulfillment-service").
	Name string `yaml:"name" mapstructure:"name"`

	// URL is the clone URL (e.g., "https://github.com/org/repo").
	URL string `yaml:"url" mapstructure:"url"`

	// Profile references a named profile in the project's Profiles
	// map. The profile provides fallback container, imports,
	// instructions, and workflow settings for this repo.
	Profile string `yaml:"profile" mapstructure:"profile"`

	// TargetBranch is the base branch for pull requests (e.g.,
	// "main", "master"). Defaults to "main" when empty.
	TargetBranch string `yaml:"target_branch" mapstructure:"target_branch"`
}

// ComponentConfig maps a Jira component to a workspace.
type ComponentConfig struct {
	Workspace string `yaml:"workspace" mapstructure:"workspace"`
}

// ComponentMap preserves the case of component names when parsing YAML.
// Viper lowercases map keys by default; this custom unmarshaler bypasses
// that to keep the original casing (component names are matched
// case-insensitively at lookup time).
type ComponentMap map[string]ComponentConfig

// UnmarshalYAML implements custom unmarshaling to preserve component name case.
func (c *ComponentMap) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("expected mapping node for ComponentMap, got %v", value.Kind)
	}

	result := make(map[string]ComponentConfig)
	for i := 0; i < len(value.Content); i += 2 {
		if i+1 >= len(value.Content) {
			break
		}
		keyNode := value.Content[i]
		valueNode := value.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			continue
		}

		var key string
		if err := keyNode.Decode(&key); err != nil {
			continue
		}

		var val ComponentConfig
		if err := valueNode.Decode(&val); err != nil {
			return fmt.Errorf("decode component %q: %w", key, err)
		}

		result[key] = val
	}

	*c = result
	return nil
}

// ProjectKeys is a custom type for parsing project_keys from environment variables and YAML
type ProjectKeys []string

// UnmarshalText implements encoding.TextUnmarshaler for parsing from environment variables
func (p *ProjectKeys) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*p = make([]string, 0)
		return nil
	}

	str := string(text)
	keys := strings.Split(str, ",")
	result := make([]string, 0, len(keys))

	for _, key := range keys {
		trimmedKey := strings.TrimSpace(key)
		if trimmedKey != "" {
			result = append(result, trimmedKey)
		}
	}

	*p = result
	return nil
}

// StatusTransitions represents the status transitions for different ticket types
type StatusTransitions struct {
	Todo       string `yaml:"todo" mapstructure:"todo" default:"To Do"`
	InProgress string `yaml:"in_progress" mapstructure:"in_progress" default:"In Progress"`
	InReview   string `yaml:"in_review" mapstructure:"in_review" default:"In Review"`
	Merged     string `yaml:"merged" mapstructure:"merged"`
}

// TicketTypeStatusTransitions maps ticket types to their specific status transitions
type TicketTypeStatusTransitions map[string]StatusTransitions

// UnmarshalYAML implements custom unmarshaling for TicketTypeStatusTransitions
func (t *TicketTypeStatusTransitions) UnmarshalYAML(value *yaml.Node) error {
	*t = make(TicketTypeStatusTransitions)

	// Handle the case where status_transitions is a map of ticket types
	return value.Decode((*map[string]StatusTransitions)(t))
}

// GetStatusTransitions returns the status transitions for a specific ticket type
func (t TicketTypeStatusTransitions) GetStatusTransitions(ticketType string) StatusTransitions {
	// Try exact match first
	if transitions, exists := t[ticketType]; exists {
		return transitions
	}
	// Try lowercase match (since Viper converts YAML keys to lowercase)
	if transitions, exists := t[strings.ToLower(ticketType)]; exists {
		return transitions
	}
	// Return empty transitions if ticket type is not configured
	return StatusTransitions{}
}

// UnmarshalMapstructure implements custom mapstructure decoding for handling different data types
func (t *TicketTypeStatusTransitions) UnmarshalMapstructure(data interface{}) error {
	*t = make(TicketTypeStatusTransitions)

	// Handle string data (from environment variables as JSON)
	if jsonStr, ok := data.(string); ok {
		// Try to parse as JSON
		var jsonData map[string]interface{}
		if err := json.Unmarshal([]byte(jsonStr), &jsonData); err != nil {
			return fmt.Errorf("failed to parse status transitions JSON: %w", err)
		}

		// Convert JSON data to TicketTypeStatusTransitions
		for ticketType, transitionData := range jsonData {
			if transitionMap, ok := transitionData.(map[string]interface{}); ok {
				transitions := StatusTransitions{}
				if todo, ok := transitionMap["todo"].(string); ok {
					transitions.Todo = todo
				}
				if inProgress, ok := transitionMap["in_progress"].(string); ok {
					transitions.InProgress = inProgress
				}
				if inReview, ok := transitionMap["in_review"].(string); ok {
					transitions.InReview = inReview
				}
				if merged, ok := transitionMap["merged"].(string); ok {
					transitions.Merged = merged
				}
				(*t)[ticketType] = transitions
			}
		}
		return nil
	}

	// Handle the case where data is a map[string]interface{} (from mapstructure)
	if mapData, ok := data.(map[string]interface{}); ok {

		// New format - convert map[string]interface{} to map[string]StatusTransitions
		for ticketType, transitionData := range mapData {
			if transitionMap, ok := transitionData.(map[string]interface{}); ok {
				transitions := StatusTransitions{}
				if todo, ok := transitionMap["todo"].(string); ok {
					transitions.Todo = todo
				}
				if inProgress, ok := transitionMap["in_progress"].(string); ok {
					transitions.InProgress = inProgress
				}
				if inReview, ok := transitionMap["in_review"].(string); ok {
					transitions.InReview = inReview
				}
				if merged, ok := transitionMap["merged"].(string); ok {
					transitions.Merged = merged
				}
				(*t)[ticketType] = transitions
			}
		}
		return nil
	}

	return fmt.Errorf("unsupported data type for TicketTypeStatusTransitions: %T", data)
}

// ProjectConfig represents configuration for a specific project or group of projects.
type ProjectConfig struct {
	ProjectKeys             ProjectKeys                 `yaml:"project_keys" mapstructure:"project_keys"`
	StatusTransitions       TicketTypeStatusTransitions `yaml:"status_transitions" mapstructure:"status_transitions"`
	GitPullRequestFieldName string                      `yaml:"git_pull_request_field_name" mapstructure:"git_pull_request_field_name"`
	DisableErrorComments    bool                        `yaml:"disable_error_comments" mapstructure:"disable_error_comments" default:"false"`

	// ForkMode when true requires fork-based contributions for
	// this project. Commits are pushed to the assignee's fork
	// (looked up via jira.assignee_to_github_username) and PRs
	// are created as cross-repo PRs. When false, commits go
	// directly to the upstream repo.
	ForkMode bool `yaml:"fork_mode" mapstructure:"fork_mode" default:"false"`

	// DefaultWorkspace is the workspace used for tickets that have
	// no Jira component or whose component has no mapping in
	// Components. Projects that require explicit component mapping
	// leave this empty.
	DefaultWorkspace string `yaml:"default_workspace" mapstructure:"default_workspace"`

	// Workspaces maps workspace names to their definitions. Each
	// workspace groups one or more repositories that form a working
	// environment for AI sessions.
	Workspaces map[string]WorkspaceConfig `yaml:"workspaces" mapstructure:"workspaces"`

	// Profiles maps profile names to their settings. Each profile
	// bundles container settings, imports, instructions, and
	// workflow for a repo within a workspace. Multiple repos can
	// reference the same profile to avoid duplication.
	Profiles map[string]Profile `yaml:"profiles" mapstructure:"profiles"`

	// Components maps Jira component names to workspace names.
	// Component names are matched case-insensitively at lookup time.
	Components ComponentMap `yaml:"components" mapstructure:"components"`

	// FailureLabels configures optional Jira labels applied to
	// tickets in failure states. Empty strings disable the
	// corresponding label.
	FailureLabels FailureLabels `yaml:"failure_labels" mapstructure:"failure_labels"`

	// LifecycleLabels configures optional Jira labels that track
	// ticket progression through the autofix pipeline. Empty strings
	// disable the corresponding label.
	LifecycleLabels LifecycleLabels `yaml:"lifecycle_labels" mapstructure:"lifecycle_labels"`

	// PRValidationLabels configures GitHub PR labels applied when the
	// AI session reports validation failure or exits with a non-zero
	// code. Labels are mutually exclusive: at most one is set on a PR.
	// Empty strings disable the corresponding label. Suggested values:
	// "ai-validation-failed" and "ai-nonzero-exit".
	PRValidationLabels PRValidationLabels `yaml:"pr_validation_labels" mapstructure:"pr_validation_labels"`

	// TriageLabels configures triage label cleanup. When a ticket
	// leaves NewStatus and is assigned to a human, active triage
	// labels are replaced with the stale label.
	TriageLabels TriageLabels `yaml:"triage_labels" mapstructure:"triage_labels"`

	// MaxTicketCostUSD overrides the global
	// guardrails.max_ticket_cost_usd for this project. Nil means use
	// the global default; an explicit negative value disables per-ticket
	// cost capping for this project.
	MaxTicketCostUSD *float64 `yaml:"max_ticket_cost_usd,omitempty" mapstructure:"max_ticket_cost_usd"`
}

// FailureLabels holds optional Jira label names applied to tickets in
// failure states. Each label is independent: an empty string disables
// that label. Labels are mutually exclusive by lifecycle.
type FailureLabels struct {
	// CIFailing is applied when the bot's PR exists but CI checks
	// are failing. Removed when CI passes.
	CIFailing string `yaml:"ci_failing" mapstructure:"ci_failing"`

	// Rejected is applied when a human reviewer closes the PR
	// without merging.
	Rejected string `yaml:"rejected" mapstructure:"rejected"`

	// Blocked is applied when the bot cannot proceed on a ticket
	// (workspace errors, infra failures, retry exhaustion).
	Blocked string `yaml:"blocked" mapstructure:"blocked"`

	// ForkUserMissing is applied when a fork-mode project cannot
	// resolve the ticket assignee's GitHub username from the
	// jira.assignee_to_github_username mapping.
	ForkUserMissing string `yaml:"fork_user_missing" mapstructure:"fork_user_missing"`
}

// All returns the configured label strings in a fixed order. Empty
// strings (disabled labels) are included; callers should skip them.
func (fl FailureLabels) All() []string {
	return []string{fl.CIFailing, fl.Rejected, fl.Blocked, fl.ForkUserMissing}
}

// LifecycleLabels holds optional Jira label names that track ticket
// progression through the autofix pipeline. Labels are mutually
// exclusive: setting one removes the others. Empty strings disable
// the corresponding label.
type LifecycleLabels struct {
	// Queued is set externally (e.g., by a triage bot) to indicate
	// the ticket is waiting for the autofix bot. This bot never
	// sets it, but removes it when applying Review.
	Queued string `yaml:"queued" mapstructure:"queued"`

	// Review is applied when the bot creates a PR and the ticket
	// moves to "in review" status.
	Review string `yaml:"review" mapstructure:"review"`

	// Merged is applied when all PRs for the ticket are merged.
	Merged string `yaml:"merged" mapstructure:"merged"`
}

// All returns the configured label strings in a fixed order. Empty
// strings (disabled labels) are included; callers should skip them.
func (ll LifecycleLabels) All() []string {
	return []string{ll.Queued, ll.Review, ll.Merged}
}

// DefaultCostCapLabel is the default GitHub PR label applied when the
// per-ticket AI session cost cap is exceeded.
const DefaultCostCapLabel = "ai-budget-exceeded"

// PRValidationLabels holds configurable GitHub PR labels applied when
// the AI session's validation or exit code indicates a problem. Labels
// are mutually exclusive: at most one is set on a given PR. Empty
// strings disable the corresponding label.
type PRValidationLabels struct {
	// ValidationFailed is applied when the AI session explicitly
	// reports validation_passed: false.
	ValidationFailed string `yaml:"validation_failed" mapstructure:"validation_failed"`

	// NonzeroExit is applied when the AI container exits with a
	// non-zero code (and validation was not explicitly reported
	// as failed).
	NonzeroExit string `yaml:"nonzero_exit" mapstructure:"nonzero_exit"`

	// CostCapExceeded is applied when the per-ticket AI session
	// cost cap has been reached. Defaults to DefaultCostCapLabel
	// when not configured (nil); set to "" to disable.
	CostCapExceeded *string `yaml:"cost_cap_exceeded,omitempty" mapstructure:"cost_cap_exceeded"`
}

// CostCapLabel returns the raw cost cap exceeded label value.
// Returns empty string when the pointer is nil or points to "".
// The default (DefaultCostCapLabel) is applied by the project
// resolver, not here; callers outside the resolver should only
// see post-resolution values.
func (vl PRValidationLabels) CostCapLabel() string {
	if vl.CostCapExceeded == nil {
		return ""
	}
	return *vl.CostCapExceeded
}

// All returns the configured label strings in a fixed order. Empty
// strings (disabled labels) are included; callers should skip them.
func (vl PRValidationLabels) All() []string {
	return []string{vl.ValidationFailed, vl.NonzeroExit, vl.CostCapLabel()}
}

// AllPipelineLabels returns all configured failure and lifecycle labels
// in a single slice. Used by setPipelineLabel / applyPipelineLabel to
// enforce mutual exclusivity across both groups.
func AllPipelineLabels(fl FailureLabels, ll LifecycleLabels) []string {
	return append(fl.All(), ll.All()...)
}

// TriageLabels configures triage label cleanup for tickets that leave
// the triage phase. Active lists the labels applied by the triage bot
// during assessment. When a ticket leaves NewStatus and is assigned to
// a human, active labels are replaced with Stale.
type TriageLabels struct {
	// Active lists triage labels to clean up (e.g., "jira-triage-missing-info").
	Active []string `yaml:"active" mapstructure:"active"`

	// Stale is the replacement label (e.g., "jira-triage-stale").
	Stale string `yaml:"stale" mapstructure:"stale"`

	// NewStatus is the ticket status meaning "still in triage" (e.g., "New").
	NewStatus string `yaml:"new_status" mapstructure:"new_status"`
}

// ImportConfig declares an auxiliary repository to clone into the workspace.
type ImportConfig struct {
	// Repo is the clone URL (e.g., "https://github.com/org/repo").
	Repo string `yaml:"repo" mapstructure:"repo"`

	// Path is the destination directory relative to the workspace
	// root (e.g., ".ai-workflows"). Required.
	Path string `yaml:"path" mapstructure:"path"`

	// Ref is the branch, tag, or commit to check out. Empty means
	// the remote's default branch.
	Ref string `yaml:"ref" mapstructure:"ref"`

	// Install is a shell command to run inside the container after
	// cloning. The command runs from the workspace root (/workspace)
	// and has access to the container's toolchain. Empty means no
	// install step. Example: ".ai-workflows/install.sh".
	Install string `yaml:"install" mapstructure:"install"`

	// Excludes lists directories (relative to workspace root) that
	// this import's tools may create as output. These directories
	// are excluded from commits and preserved across workspace syncs.
	// Example: [".artifacts/"] for an AI workflow import that writes
	// intermediate artifacts there.
	Excludes []string `yaml:"excludes" mapstructure:"excludes"`
}

type JiraConfig struct {
	BaseURL                  string            `yaml:"base_url" mapstructure:"base_url"`
	Username                 string            `yaml:"username" mapstructure:"username"`
	APIToken                 string            `yaml:"api_token" mapstructure:"api_token"`
	IntervalSeconds          int               `yaml:"interval_seconds" mapstructure:"interval_seconds" default:"300"`
	AssigneeToGitHubUsername map[string]string `yaml:"assignee_to_github_username" mapstructure:"assignee_to_github_username"`
	Projects                 []ProjectConfig   `yaml:"projects" mapstructure:"projects"`
}

// Config represents the application configuration
type Config struct {
	// Server configuration
	Server struct {
		Port int `yaml:"port" mapstructure:"port" default:"8080"`
	} `yaml:"server" mapstructure:"server"`

	// Logging configuration
	Logging struct {
		Level  LogLevel  `yaml:"level" mapstructure:"level" default:"info"`
		Format LogFormat `yaml:"format" mapstructure:"format" default:"console"`
	} `yaml:"logging" mapstructure:"logging"`

	// Jira configuration
	Jira JiraConfig `yaml:"jira" mapstructure:"jira"`

	// GitHub configuration
	GitHub struct {
		// GitHub App authentication
		AppID          int64  `yaml:"app_id" mapstructure:"app_id"`
		PrivateKeyPath string `yaml:"private_key_path" mapstructure:"private_key_path"`

		// Common fields
		BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
		BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"` // Optional: auto-constructed for GitHub App mode
		PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
		SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`                         // Path to SSH private key for commit signing
		MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`     // Maximum number of bot replies allowed in a comment thread (e.g., 5 = bot can reply up to 5 times)
		KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`           // List of known bot usernames to prevent loops
		IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`               // List of usernames whose PR comments are completely ignored
		IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`           // Check run names excluded from CI failure detection
		SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"` // GitHub label that tells the bot to skip a PR
	} `yaml:"github" mapstructure:"github"`

	// GitLab configuration (optional — required only when workspaces
	// use hosting: gitlab). Uses Personal Access Token or Project
	// Access Token for authentication.
	GitLab struct {
		BaseURL      string `yaml:"base_url" mapstructure:"base_url"`           // e.g., "https://gitlab.com" or "https://gitlab.cee.redhat.com"
		AccessToken  string `yaml:"access_token" mapstructure:"access_token"`   // PAT or project/group access token
		BotUsername  string `yaml:"bot_username" mapstructure:"bot_username"`    // Git commit author name
		BotEmail     string `yaml:"bot_email" mapstructure:"bot_email"`          // Git commit author email
		MRLabel      string `yaml:"mr_label" mapstructure:"mr_label"`           // Default label applied to merge requests
		SSHKeyPath   string `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`   // Optional: SSH key for commit signing
		SkipMRLabel  string `yaml:"skip_mr_label" mapstructure:"skip_mr_label"` // GitLab label that tells the bot to skip an MR
		MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth"`
		KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
		IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
	} `yaml:"gitlab" mapstructure:"gitlab"`

	// AI Provider selection
	AIProvider string `yaml:"ai_provider" mapstructure:"ai_provider" default:"claude"` // "claude" or "gemini"

	// Claude configuration — authentication is needed at the bot level;
	// CLI path, timeout, and tool settings are configured per-repo via
	// .ai-bot/config.yaml or container environment.
	//
	// Three authentication modes are supported (mutually exclusive):
	//   1. Direct API key: set api_key (JIRA_AI_CLAUDE_API_KEY)
	//   2. Vertex AI: set vertex_project_id, vertex_region, and
	//      vertex_credentials_file (JIRA_AI_CLAUDE_VERTEX_*)
	//   3. API Proxy: set proxy_url and proxy_auth_token
	//      (JIRA_AI_CLAUDE_PROXY_URL, JIRA_AI_CLAUDE_PROXY_AUTH_TOKEN)
	Claude struct {
		APIKey string `yaml:"api_key" mapstructure:"api_key"`

		// Model overrides the default Claude model (e.g.,
		// "claude-sonnet-4-6"). Repo-level .ai-bot/config.yaml
		// can further override this. Empty means Claude Code's
		// built-in default.
		Model string `yaml:"model" mapstructure:"model"`

		// Vertex AI authentication fields. When set, Claude Code
		// uses Anthropic's Vertex AI integration instead of the
		// direct API. All three fields must be set together.
		VertexProjectID       string `yaml:"vertex_project_id" mapstructure:"vertex_project_id"`
		VertexRegion          string `yaml:"vertex_region" mapstructure:"vertex_region"`
		VertexCredentialsFile string `yaml:"vertex_credentials_file" mapstructure:"vertex_credentials_file"`

		// API Proxy authentication fields. When set, Claude Code
		// routes requests through a custom LLM gateway or proxy.
		// Mutually exclusive with api_key and vertex_credentials_file.
		//
		// When proxy_vertex_format is false (default), the container
		// gets ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN (Anthropic
		// Messages API format).
		//
		// When proxy_vertex_format is true, the container gets
		// Vertex-format proxy env vars (ANTHROPIC_VERTEX_BASE_URL,
		// CLAUDE_CODE_SKIP_VERTEX_AUTH, etc.) and vertex_project_id
		// + vertex_region must also be set.
		ProxyURL          string `yaml:"proxy_url" mapstructure:"proxy_url"`
		ProxyAuthToken    string `yaml:"proxy_auth_token" mapstructure:"proxy_auth_token"`
		ProxyVertexFormat bool   `yaml:"proxy_vertex_format" mapstructure:"proxy_vertex_format"`
	} `yaml:"claude" mapstructure:"claude"`

	// Gemini configuration.
	Gemini struct {
		APIKey string `yaml:"api_key" mapstructure:"api_key"`
		Model  string `yaml:"model" mapstructure:"model"`

		// Token pricing (USD per million tokens) for cost estimation.
		// Defaults match gemini-2.5-flash rates as of 2026-04.
		InputPricePerMTok  float64 `yaml:"input_price_per_mtok" mapstructure:"input_price_per_mtok"`
		OutputPricePerMTok float64 `yaml:"output_price_per_mtok" mapstructure:"output_price_per_mtok"`
		CachedPricePerMTok float64 `yaml:"cached_price_per_mtok" mapstructure:"cached_price_per_mtok"`
	} `yaml:"gemini" mapstructure:"gemini"`

	// Workspaces configuration for ticket-scoped workspace lifecycle
	Workspaces WorkspacesConfig `yaml:"workspaces" mapstructure:"workspaces"`

	// Container configuration for dev container management
	Container ContainerCfg `yaml:"container" mapstructure:"container"`

	// Guardrails configuration for safety and resource limits
	Guardrails GuardrailsConfig `yaml:"guardrails" mapstructure:"guardrails"`

	// Merge configuration for auto-merge scanner behavior
	Merge MergeConfig `yaml:"merge" mapstructure:"merge"`
}

// MergeConfig holds settings for the auto-merge scanner that keeps
// PR branches current with the target branch.
type MergeConfig struct {
	// IdleDays is the number of days without human PR comment
	// activity before a PR is considered idle. Idle unmergeable
	// PRs are labeled instead of merged. Zero disables idle
	// detection (all unmergeable PRs are merged).
	IdleDays int `yaml:"idle_days" mapstructure:"idle_days"`

	// IdleLabel is the GitHub label applied to idle unmergeable
	// PRs. Removing the label re-activates auto-merging.
	IdleLabel string `yaml:"idle_label" mapstructure:"idle_label"`
}

func (m *MergeConfig) validate() error {
	if m.IdleDays < 0 {
		return errors.New("merge.idle_days must be non-negative")
	}
	if m.IdleDays > 0 && strings.TrimSpace(m.IdleLabel) == "" {
		return errors.New("merge.idle_label is required when merge.idle_days is positive")
	}
	return nil
}

// ContainerCfg holds bot-level container configuration: host-level
// runtime policy applied to all spawned containers.
type ContainerCfg struct {
	// Runtime specifies the container runtime preference. Valid values
	// are "auto" (detect, preferring podman), "podman", or "docker".
	Runtime string `yaml:"runtime" mapstructure:"runtime" default:"auto"`

	// DisableSELinux disables SELinux confinement for all spawned
	// containers (--security-opt=label=disable). This is host-level
	// policy and applies regardless of profile container config.
	DisableSELinux bool `yaml:"disable_selinux" mapstructure:"disable_selinux"`

	// UserNS sets the user namespace mode for all spawned containers
	// (e.g., "keep-id", "keep-id:uid=1000,gid=1000"). This is
	// host-level policy. Empty means the container runtime's default.
	UserNS string `yaml:"userns" mapstructure:"userns"`
}

// ContainerSettings holds per-environment container settings. This
// type is shared between the global fallback (container.fallback) and
// per-project overrides (jira.projects[].container). Both sit below
// repo-level config (.ai-bot/container.json) in the resolution chain.
type ContainerSettings struct {
	// Image is the container image reference (e.g., "my-org/dev:latest").
	Image string `yaml:"image" mapstructure:"image"`

	// ResourceLimits constrain the container's resource usage.
	ResourceLimits ContainerResourceLimits `yaml:"resource_limits" mapstructure:"resource_limits"`

	// Env holds static environment variables injected into the
	// container. These are merged additively through the resolution
	// chain: higher-priority sources override keys from lower-priority
	// sources, but keys not present in the higher source are preserved.
	// These are separate from runtime env vars (API keys, etc.) passed
	// by the executor at start time.
	Env map[string]string `yaml:"env" mapstructure:"env"`

	// Tmpfs specifies tmpfs mounts. Each entry uses the standard
	// runtime format (e.g., "/tmp:size=4g").
	Tmpfs []string `yaml:"tmpfs" mapstructure:"tmpfs"`

	// ExtraMounts specifies additional volume mounts beyond the
	// workspace mount. Useful for persistent caches (Go module
	// cache, build cache) that survive across container restarts.
	ExtraMounts []ExtraMountCfg `yaml:"extra_mounts" mapstructure:"extra_mounts"`
}

// ExtraMountCfg represents an additional volume mount for containers.
type ExtraMountCfg struct {
	Source  string `yaml:"source" mapstructure:"source"`
	Target  string `yaml:"target" mapstructure:"target"`
	Options string `yaml:"options" mapstructure:"options"`
}

// ContainerResourceLimits holds default resource limits for containers.
type ContainerResourceLimits struct {
	// Memory limit in container runtime format (e.g., "8g", "512m").
	Memory string `yaml:"memory" mapstructure:"memory"`

	// CPUs limit in container runtime format (e.g., "4", "0.5").
	CPUs string `yaml:"cpus" mapstructure:"cpus"`
}

// WorkspacesConfig holds configuration for ticket-scoped workspace management.
type WorkspacesConfig struct {
	// BaseDir is the root directory under which per-ticket workspaces are created.
	// Each workspace is a subdirectory named after the ticket key (e.g., PROJ-123/).
	BaseDir string `yaml:"base_dir" mapstructure:"base_dir"`

	// TTLDays is the maximum age (in days) before a workspace is eligible
	// for cleanup, regardless of ticket status. Must be positive.
	//
	// If a workspace is cleaned up while its ticket still has an active PR,
	// the feedback pipeline will self-heal by re-cloning the repository.
	// However, AI-generated artifacts from prior sessions will be lost.
	// Set this high enough to cover typical PR review turnaround times.
	TTLDays int `yaml:"ttl_days" mapstructure:"ttl_days" default:"7"`
}

// GuardrailsConfig holds safety and resource limit settings.
type GuardrailsConfig struct {
	// MaxConcurrentJobs is the maximum number of jobs that can run
	// simultaneously. Must be positive.
	MaxConcurrentJobs int `yaml:"max_concurrent_jobs" mapstructure:"max_concurrent_jobs" default:"10"`

	// MaxRetries is the maximum number of times a ticket can fail
	// before further submissions are rejected. Zero means no retries
	// (one attempt total). Negative disables the retry limit.
	MaxRetries int `yaml:"max_retries" mapstructure:"max_retries" default:"3"`

	// MaxDailyCostUSD is the maximum daily AI session cost in USD.
	// Job creation is paused when this budget is exceeded. Zero or
	// negative disables cost-based limiting.
	MaxDailyCostUSD float64 `yaml:"max_daily_cost_usd" mapstructure:"max_daily_cost_usd"`

	// MaxTicketCostUSD is the default per-ticket AI session cost cap
	// in USD. No new AI sessions are started for a ticket once its
	// cumulative cost reaches or exceeds this value. Can be overridden
	// per-project in ProjectConfig. Zero or negative disables
	// per-ticket cost capping.
	MaxTicketCostUSD float64 `yaml:"max_ticket_cost_usd" mapstructure:"max_ticket_cost_usd"`

	// MaxContainerRuntimeMinutes is the maximum duration (in minutes)
	// for an AI session inside a container. Zero means no timeout.
	MaxContainerRuntimeMinutes int `yaml:"max_container_runtime_minutes" mapstructure:"max_container_runtime_minutes" default:"60"`

	// CircuitBreakerThreshold is the number of consecutive failures
	// within CircuitBreakerWindow that trips the breaker. Zero
	// disables the circuit breaker.
	CircuitBreakerThreshold int `yaml:"circuit_breaker_threshold" mapstructure:"circuit_breaker_threshold" default:"5"`

	// CircuitBreakerWindowMinutes is the time window (in minutes) for
	// counting consecutive failures. Failures outside this window are
	// pruned.
	CircuitBreakerWindowMinutes int `yaml:"circuit_breaker_window_minutes" mapstructure:"circuit_breaker_window_minutes" default:"10"`

	// CircuitBreakerCooldownMinutes is how long (in minutes) the circuit
	// breaker stays open before automatically resetting.
	CircuitBreakerCooldownMinutes int `yaml:"circuit_breaker_cooldown_minutes" mapstructure:"circuit_breaker_cooldown_minutes" default:"5"`

	// MaxCIFixAttempts limits how many times the bot will attempt to
	// fix CI failures on a single PR. Zero disables CI failure
	// detection entirely. Negative means unlimited attempts.
	MaxCIFixAttempts int `yaml:"max_ci_fix_attempts" mapstructure:"max_ci_fix_attempts" default:"3"`

	// RetryLabel is the Jira label that users can add to a ticket to
	// trigger a retry after the bot has exhausted its retry limit.
	// The scanner detects the label, resets the retry count, removes
	// the label, and resubmits the ticket. Empty disables the feature.
	RetryLabel string `yaml:"retry_label" mapstructure:"retry_label" default:"ai-retry"`

	// MinCommentLength is the minimum character length for Jira ticket
	// comments to be included in AI task files. Comments shorter than
	// this are filtered as noise. Zero disables length filtering.
	MinCommentLength int `yaml:"min_comment_length" mapstructure:"min_comment_length" default:"20"`

	// MaxCommitFiles is the maximum number of files allowed in a single
	// commit created via the GitHub API. Prevents the bot from pushing
	// oversized commits when the AI modifies more files than intended.
	// Zero disables the configurable limit (a hard-coded 1000-file
	// safety cap still applies).
	MaxCommitFiles int `yaml:"max_commit_files" mapstructure:"max_commit_files" default:"100"`
}

// GetProjectConfigForTicket returns the project configuration for a given ticket key
func (c *Config) GetProjectConfigForTicket(ticketKey string) *ProjectConfig {
	projectKey := strings.Split(ticketKey, "-")[0]

	for _, project := range c.Jira.Projects {
		for _, key := range project.ProjectKeys {
			if strings.EqualFold(key, projectKey) {
				return &project
			}
		}
	}

	// Return the first project if no specific match found (fallback)
	if len(c.Jira.Projects) > 0 {
		return &c.Jira.Projects[0]
	}

	return nil
}

// GetAllProjectKeys returns all project keys from all project configurations
func (c *Config) GetAllProjectKeys() []string {
	var allKeys []string
	for _, project := range c.Jira.Projects {
		allKeys = append(allKeys, project.ProjectKeys...)
	}
	return allKeys
}

// GetBotEmail returns the bot email, constructing it from app_id and bot_username for GitHub App mode if not explicitly set
// For GitHub App: {app_id}+{bot_username}[bot]@users.noreply.github.com
// For PAT mode: uses the explicitly configured bot_email
func (c *Config) GetBotEmail() string {
	// If bot_email is explicitly set, use it (PAT mode or manual override)
	if c.GitHub.BotEmail != "" {
		return c.GitHub.BotEmail
	}

	// For GitHub App mode, construct from app_id and bot_username
	if c.GitHub.AppID > 0 {
		return fmt.Sprintf("%d+%s[bot]@users.noreply.github.com", c.GitHub.AppID, c.GitHub.BotUsername)
	}

	// Fallback: return empty string (will be caught by validation)
	return ""
}

// HostingProvider constants.
const (
	HostingGitHub  = "github"
	HostingGitLab  = "gitlab"
)

// HostingProvider returns the normalized hosting provider for the workspace.
// Defaults to "github" when the field is empty.
func (w *WorkspaceConfig) HostingProvider() string {
	switch strings.ToLower(w.Hosting) {
	case HostingGitLab:
		return HostingGitLab
	default:
		return HostingGitHub
	}
}

// RequiredHostingProviders scans all project workspaces and returns which
// hosting providers are in use. GitHub is always needed if any workspace
// uses it (or has no explicit hosting field, since github is the default).
func (c *Config) RequiredHostingProviders() (needsGitHub, needsGitLab bool) {
	for _, project := range c.Jira.Projects {
		for _, ws := range project.Workspaces {
			switch ws.HostingProvider() {
			case HostingGitLab:
				needsGitLab = true
			default:
				needsGitHub = true
			}
		}
	}
	// If no workspaces are configured at all, default to needing GitHub
	// (backward compatibility).
	if !needsGitHub && !needsGitLab {
		needsGitHub = true
	}
	return
}

// LoadConfig loads configuration from multiple sources with Viper
// Priority order: Environment variables > Config file > .env file > Defaults
func LoadConfig(configPath string) (*Config, error) {
	v := viper.New()

	// Set defaults first
	setDefaults(v)

	// Enable environment variable support
	v.SetEnvPrefix("JIRA_AI")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Note: Automatic environment variable mapping is enabled
	// Environment variables should use the format: JIRA_AI_<SECTION>_<FIELD>
	// Examples:
	// - JIRA_AI_JIRA_BASE_URL → jira.base_url
	// - JIRA_AI_JIRA_USERNAME → jira.username
	// - JIRA_AI_JIRA_GIT_PULL_REQUEST_FIELD_NAME → jira.git_pull_request_field_name

	// Helper function to bind environment variables with error checking
	// Panics on error since all keys are static strings and should never fail
	bindEnv := func(key string) {
		if err := v.BindEnv(key); err != nil {
			panic(fmt.Sprintf("failed to bind environment variable %s: %v", key, err))
		}
	}

	// Explicit bindings for all environment variables (automatic mapping doesn't work with nested structs)
	// Jira configuration
	bindEnv("jira.base_url")
	bindEnv("jira.username")
	bindEnv("jira.api_token")
	bindEnv("jira.interval_seconds")
	bindEnv("jira.assignee_to_github_username")
	bindEnv("jira.disable_error_comments")
	bindEnv("jira.git_pull_request_field_name")
	bindEnv("jira.status_transitions")
	bindEnv("jira.project_keys")

	// GitHub configuration
	bindEnv("github.app_id")
	bindEnv("github.private_key_path")
	bindEnv("github.bot_username")
	bindEnv("github.bot_email")
	bindEnv("github.pr_label")
	bindEnv("github.ssh_key_path")
	bindEnv("github.max_thread_depth")
	bindEnv("github.known_bot_usernames")
	bindEnv("github.ignored_usernames")
	bindEnv("github.ignored_check_names")
	bindEnv("github.skip_pr_label")

	// GitLab configuration
	bindEnv("gitlab.base_url")
	bindEnv("gitlab.access_token")
	bindEnv("gitlab.bot_username")
	bindEnv("gitlab.bot_email")
	bindEnv("gitlab.mr_label")
	bindEnv("gitlab.ssh_key_path")
	bindEnv("gitlab.skip_mr_label")
	bindEnv("gitlab.max_thread_depth")
	bindEnv("gitlab.known_bot_usernames")
	bindEnv("gitlab.ignored_usernames")

	// AI configuration
	bindEnv("ai_provider")

	// AI API key configuration
	bindEnv("claude.api_key")
	bindEnv("claude.model")
	bindEnv("claude.vertex_project_id")
	bindEnv("claude.vertex_region")
	bindEnv("claude.vertex_credentials_file")
	bindEnv("claude.proxy_url")
	bindEnv("claude.proxy_auth_token")
	bindEnv("claude.proxy_vertex_format")
	bindEnv("gemini.api_key")
	bindEnv("gemini.model")
	bindEnv("gemini.input_price_per_mtok")
	bindEnv("gemini.output_price_per_mtok")
	bindEnv("gemini.cached_price_per_mtok")

	// Server configuration
	bindEnv("server.port")
	bindEnv("PORT")

	// Logging configuration
	bindEnv("logging.level")
	bindEnv("logging.format")

	// Workspaces configuration
	bindEnv("workspaces.base_dir")
	bindEnv("workspaces.ttl_days")

	// Container configuration
	bindEnv("container.runtime")
	bindEnv("container.disable_selinux")
	bindEnv("container.userns")

	// Guardrails configuration
	bindEnv("guardrails.max_concurrent_jobs")
	bindEnv("guardrails.max_retries")
	bindEnv("guardrails.max_daily_cost_usd")
	bindEnv("guardrails.max_ticket_cost_usd")
	bindEnv("guardrails.max_container_runtime_minutes")
	bindEnv("guardrails.circuit_breaker_threshold")
	bindEnv("guardrails.circuit_breaker_window_minutes")
	bindEnv("guardrails.circuit_breaker_cooldown_minutes")
	bindEnv("guardrails.max_ci_fix_attempts")
	bindEnv("guardrails.min_comment_length")
	bindEnv("guardrails.retry_label")
	bindEnv("guardrails.max_commit_files")

	// Merge configuration
	bindEnv("merge.idle_days")
	bindEnv("merge.idle_label")

	// Note: component_to_repo has custom unmarshaling logic, so we don't bind it explicitly

	// Load main config file if provided
	if configPath != "" {
		v.SetConfigFile(configPath)

		if err := v.ReadInConfig(); err != nil {
			if _, ok := err.(viper.ConfigFileNotFoundError); ok {
				// Config file not found; this is okay, we'll use env vars and defaults
				fmt.Printf("Warning: Config file %s not found, using environment variables and defaults\n", configPath)
			} else {
				// Config file was found but another error was produced
				return nil, fmt.Errorf("error reading config file: %w", err)
			}
		} else {
			fmt.Printf("Config file found and successfully parsed: %s\n", configPath)
		}
	}

	// Load .env file if it exists (after main config to allow overrides)
	v.SetConfigName(".env")
	v.SetConfigType("env")
	v.AddConfigPath(".")
	if err := v.ReadInConfig(); err != nil {
		// It's okay if .env file doesn't exist
		_ = err
	}

	// Unmarshal into struct
	var config Config
	if err := v.Unmarshal(&config, viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
		func(f reflect.Type, t reflect.Type, data any) (any, error) {
			if t == reflect.TypeOf(TicketTypeStatusTransitions{}) {
				var result TicketTypeStatusTransitions
				if err := result.UnmarshalMapstructure(data); err != nil {
					return nil, err
				}
				return result, nil
			}
			return data, nil
		},
	))); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Fallback to environment variable parsing for projects if still empty
	if len(config.Jira.Projects) == 0 {
		// Create a default project from environment variables
		defaultProject := ProjectConfig{}

		// Handle component_to_repo from environment. Creates a
		// "default" profile, one workspace per component, and maps
		// each component to its workspace. Operators who need
		// multi-repo workspaces must use a YAML config file.
		componentToRepoStr := v.GetString("component_to_repo")
		if componentToRepoStr != "" {
			pairs := strings.Split(componentToRepoStr, ",")
			components := make(ComponentMap)
			workspaces := make(map[string]WorkspaceConfig)

			for _, pair := range pairs {
				parts := strings.SplitN(pair, "=", 2)
				if len(parts) == 2 {
					compName := parts[0]
					repoURL := parts[1]
					repoName := repoNameFromURL(repoURL)

					components[compName] = ComponentConfig{
						Workspace: compName,
					}
					workspaces[compName] = WorkspaceConfig{
						Repos: []RepoEntry{{Name: repoName, URL: repoURL, Profile: "default"}},
					}
				}
			}

			defaultProject.Components = components
			defaultProject.Workspaces = workspaces
			defaultProject.Profiles = map[string]Profile{
				"default": {},
			}
		}

		// Handle status transitions from environment
		defaultProject.StatusTransitions = reconstructStatusTransitionsFromEnv(v)

		// Handle project keys from environment (if set)
		projectKeysStr := v.GetString("jira.project_keys")
		if projectKeysStr != "" {
			keys := strings.Split(projectKeysStr, ",")
			var projectKeys []string
			for _, key := range keys {
				if trimmed := strings.TrimSpace(key); trimmed != "" {
					projectKeys = append(projectKeys, trimmed)
				}
			}
			defaultProject.ProjectKeys = ProjectKeys(projectKeys)
		}

		// Handle git PR field name from environment
		gitFieldName := v.GetString("jira.git_pull_request_field_name")
		if gitFieldName != "" {
			defaultProject.GitPullRequestFieldName = gitFieldName
		}

		// Handle disable error comments from environment
		defaultProject.DisableErrorComments = v.GetBool("jira.disable_error_comments")

		// Only add the default project if it has some configuration
		if len(defaultProject.Components) > 0 || len(defaultProject.StatusTransitions) > 0 || len(defaultProject.ProjectKeys) > 0 {
			config.Jira.Projects = []ProjectConfig{defaultProject}
		}
	}

	// Validate configuration (after all fallbacks have been applied)
	if err := config.validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

// reconstructStatusTransitionsFromEnv reconstructs status transitions from individual environment variables
// This is used as a fallback when the JSON approach doesn't work in deployment
func reconstructStatusTransitionsFromEnv(v *viper.Viper) TicketTypeStatusTransitions {
	result := make(TicketTypeStatusTransitions)

	// Check for Bug ticket type
	bugTransitions := StatusTransitions{}
	if todo := v.GetString("jira.status_transitions_bug_todo"); todo != "" {
		bugTransitions.Todo = todo
	}
	if inProgress := v.GetString("jira.status_transitions_bug_in_progress"); inProgress != "" {
		bugTransitions.InProgress = inProgress
	}
	if inReview := v.GetString("jira.status_transitions_bug_in_review"); inReview != "" {
		bugTransitions.InReview = inReview
	}
	if bugTransitions.Todo != "" || bugTransitions.InProgress != "" || bugTransitions.InReview != "" {
		result["Bug"] = bugTransitions
	}

	// Check for Story ticket type
	storyTransitions := StatusTransitions{}
	if todo := v.GetString("jira.status_transitions_story_todo"); todo != "" {
		storyTransitions.Todo = todo
	}
	if inProgress := v.GetString("jira.status_transitions_story_in_progress"); inProgress != "" {
		storyTransitions.InProgress = inProgress
	}
	if inReview := v.GetString("jira.status_transitions_story_in_review"); inReview != "" {
		storyTransitions.InReview = inReview
	}
	if storyTransitions.Todo != "" || storyTransitions.InProgress != "" || storyTransitions.InReview != "" {
		result["Story"] = storyTransitions
	}

	// Check for Task ticket type
	taskTransitions := StatusTransitions{}
	if todo := v.GetString("jira.status_transitions_task_todo"); todo != "" {
		taskTransitions.Todo = todo
	}
	if inProgress := v.GetString("jira.status_transitions_task_in_progress"); inProgress != "" {
		taskTransitions.InProgress = inProgress
	}
	if inReview := v.GetString("jira.status_transitions_task_in_review"); inReview != "" {
		taskTransitions.InReview = inReview
	}
	if taskTransitions.Todo != "" || taskTransitions.InProgress != "" || taskTransitions.InReview != "" {
		result["Task"] = taskTransitions
	}

	return result
}

// setDefaults sets all configuration defaults
func setDefaults(v *viper.Viper) {
	// Server defaults
	v.SetDefault("server.port", 8080)

	// Logging defaults
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "console")

	// Jira defaults
	v.SetDefault("jira.interval_seconds", 300)
	v.SetDefault("jira.disable_error_comments", false)

	// GitHub defaults
	v.SetDefault("github.pr_label", "ai-pr")
	v.SetDefault("github.skip_pr_label", "ai-bot-skip")
	v.SetDefault("github.max_thread_depth", 5)
	v.SetDefault("github.known_bot_usernames", []string{
		"github-actions",
		"dependabot",
		"renovate",
		"coderabbitai",
		"sourcery-ai",
		"copilot",
		"deepsource-io",
		"codefactor-io",
		"codeclimate",
	})

	// GitLab defaults
	v.SetDefault("gitlab.mr_label", "ai-mr")
	v.SetDefault("gitlab.skip_mr_label", "ai-bot-skip")
	v.SetDefault("gitlab.max_thread_depth", 5)

	// AI Provider defaults
	v.SetDefault("ai_provider", "claude")

	// Gemini pricing defaults (USD per million tokens, gemini-2.5-flash rates)
	v.SetDefault("gemini.input_price_per_mtok", 0.15)
	v.SetDefault("gemini.output_price_per_mtok", 0.60)
	v.SetDefault("gemini.cached_price_per_mtok", 0.0375)

	// Workspace defaults
	v.SetDefault("workspaces.ttl_days", 7)

	// Container defaults
	v.SetDefault("container.runtime", "auto")

	// Guardrails defaults
	v.SetDefault("guardrails.max_concurrent_jobs", 10)
	v.SetDefault("guardrails.max_retries", 3)
	v.SetDefault("guardrails.max_container_runtime_minutes", 60)
	v.SetDefault("guardrails.circuit_breaker_threshold", 5)
	v.SetDefault("guardrails.circuit_breaker_window_minutes", 10)
	v.SetDefault("guardrails.circuit_breaker_cooldown_minutes", 5)
	v.SetDefault("guardrails.max_ci_fix_attempts", 3)
	v.SetDefault("guardrails.retry_label", "ai-retry")
	v.SetDefault("guardrails.min_comment_length", 20)
	v.SetDefault("guardrails.max_commit_files", 100)
	v.SetDefault("guardrails.max_ticket_cost_usd", 20.0)

	// Merge configuration defaults
	v.SetDefault("merge.idle_days", 7)
	v.SetDefault("merge.idle_label", "ai-bot/idle")
}

// validate validates the entire configuration
func (c *Config) validate() error {
	// Validate AI provider configuration
	if c.AIProvider != "claude" && c.AIProvider != "gemini" {
		return errors.New("ai_provider must be either 'claude' or 'gemini'")
	}

	if err := c.validateClaudeAuth(); err != nil {
		return err
	}

	// Validate logging configuration
	if !c.Logging.Level.IsValid() {
		return fmt.Errorf("invalid log level: %s. Valid options are: debug, info, warn, error", c.Logging.Level)
	}
	if !c.Logging.Format.IsValid() {
		return fmt.Errorf("invalid log format: %s. Valid options are: console, json", c.Logging.Format)
	}

	// Validate Jira configuration (required for this application)
	if c.Jira.BaseURL == "" {
		return errors.New("jira.base_url is required")
	}
	if c.Jira.Username == "" {
		return errors.New("jira.username is required")
	}
	if c.Jira.APIToken == "" {
		return errors.New("jira.api_token is required")
	}

	// Validate projects configuration - at least one project must be configured
	if len(c.Jira.Projects) == 0 {
		return errors.New("at least one project must be configured in jira.projects")
	}

	// Validate each project configuration
	for i, project := range c.Jira.Projects {
		if err := project.validate(i); err != nil {
			return err
		}
	}

	// Determine which hosting providers are in use.
	needsGitHub, needsGitLab := c.RequiredHostingProviders()

	// GitHub validation — required when any workspace uses github hosting.
	if needsGitHub {
		if c.GitHub.AppID <= 0 {
			return errors.New("github.app_id must be a positive integer")
		}
		if c.GitHub.PrivateKeyPath == "" {
			return errors.New("github.private_key_path must be provided")
		}
		if _, err := os.Stat(c.GitHub.PrivateKeyPath); os.IsNotExist(err) {
			return fmt.Errorf("github.private_key_path file does not exist: %s", c.GitHub.PrivateKeyPath)
		}

		if c.GitHub.BotUsername == "" {
			return errors.New("github.bot_username is required")
		}

		if c.GetBotEmail() == "" {
			return errors.New("github.bot_email is required (either set explicitly, or it will be auto-constructed from app_id)")
		}

		invalidChars := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r", "\t"}
		for _, char := range invalidChars {
			if strings.Contains(c.GitHub.BotUsername, char) {
				return fmt.Errorf("github.bot_username contains invalid character %q - bot username will be used in branch names and must be git-safe", char)
			}
		}

		for _, botUsername := range c.GitHub.KnownBotUsernames {
			for _, char := range invalidChars {
				if strings.Contains(botUsername, char) {
					return fmt.Errorf("github.known_bot_usernames contains username %q with invalid character %q", botUsername, char)
				}
			}
		}
	}

	// GitLab validation — required when any workspace uses gitlab hosting.
	if needsGitLab {
		if c.GitLab.BaseURL == "" {
			return errors.New("gitlab.base_url is required when any workspace uses hosting: gitlab")
		}
		if c.GitLab.AccessToken == "" {
			return errors.New("gitlab.access_token is required when any workspace uses hosting: gitlab")
		}
		if c.GitLab.BotUsername == "" {
			return errors.New("gitlab.bot_username is required when any workspace uses hosting: gitlab")
		}
		if c.GitLab.BotEmail == "" {
			return errors.New("gitlab.bot_email is required when any workspace uses hosting: gitlab")
		}
	}

	// Validate workspaces configuration
	if c.Workspaces.BaseDir == "" {
		return errors.New("workspaces.base_dir is required")
	}
	if c.Workspaces.TTLDays <= 0 {
		return errors.New("workspaces.ttl_days must be positive")
	}

	if err := c.Container.validate(); err != nil {
		return err
	}

	if err := c.Guardrails.validate(); err != nil {
		return err
	}

	if err := c.Merge.validate(); err != nil {
		return err
	}

	return nil
}

// validate checks a single project configuration for required fields.
func (p *ProjectConfig) validate(index int) error {
	prefix := fmt.Sprintf("jira.projects[%d]", index)

	if len(p.ProjectKeys) == 0 {
		return fmt.Errorf("%s.project_keys: at least one project key must be configured", prefix)
	}

	for ticketType, transitions := range p.StatusTransitions {
		if transitions.Todo == "" {
			return fmt.Errorf("%s.status_transitions.%s.todo cannot be empty", prefix, ticketType)
		}
		if transitions.InProgress == "" {
			return fmt.Errorf("%s.status_transitions.%s.in_progress cannot be empty", prefix, ticketType)
		}
		if transitions.InReview == "" {
			return fmt.Errorf("%s.status_transitions.%s.in_review cannot be empty", prefix, ticketType)
		}
	}

	if len(p.StatusTransitions) == 0 {
		return fmt.Errorf("%s.status_transitions: at least one ticket type must be configured", prefix)
	}

	if len(p.Workspaces) == 0 {
		return fmt.Errorf("%s.workspaces: at least one workspace must be configured", prefix)
	}

	if len(p.Profiles) == 0 {
		return fmt.Errorf("%s.profiles: at least one profile must be configured", prefix)
	}

	// Validate each workspace.
	for wsName, ws := range p.Workspaces {
		if h := strings.ToLower(ws.Hosting); h != "" && h != HostingGitHub && h != HostingGitLab {
			return fmt.Errorf("%s.workspaces.%s.hosting: invalid value %q (must be %q or %q)", prefix, wsName, ws.Hosting, HostingGitHub, HostingGitLab)
		}
		if len(ws.Repos) == 0 {
			return fmt.Errorf("%s.workspaces.%s: at least one repo is required", prefix, wsName)
		}
		seenRepoNames := make(map[string]struct{}, len(ws.Repos))
		for i, repo := range ws.Repos {
			if repo.Name == "" {
				return fmt.Errorf("%s.workspaces.%s.repos[%d].name is required", prefix, wsName, i)
			}
			if _, exists := seenRepoNames[repo.Name]; exists {
				return fmt.Errorf("%s.workspaces.%s.repos[%d].name: duplicate repo name %q", prefix, wsName, i, repo.Name)
			}
			seenRepoNames[repo.Name] = struct{}{}
			if repo.URL == "" {
				return fmt.Errorf("%s.workspaces.%s.repos[%d].url is required", prefix, wsName, i)
			}
			if repo.Profile != "" {
				if _, ok := p.Profiles[repo.Profile]; !ok {
					if !profileExistsCaseInsensitive(p.Profiles, repo.Profile) {
						return fmt.Errorf("%s.workspaces.%s.repos[%d].profile: profile %q does not exist", prefix, wsName, i, repo.Profile)
					}
				}
			}
		}
	}

	// Either components or default_workspace must be configured.
	if len(p.Components) == 0 && p.DefaultWorkspace == "" {
		return fmt.Errorf("%s: either components or default_workspace must be configured", prefix)
	}

	// Validate default_workspace references a valid workspace.
	if p.DefaultWorkspace != "" {
		if _, ok := p.Workspaces[p.DefaultWorkspace]; !ok {
			if !workspaceExistsCaseInsensitive(p.Workspaces, p.DefaultWorkspace) {
				return fmt.Errorf("%s.default_workspace: workspace %q does not exist", prefix, p.DefaultWorkspace)
			}
		}
	}

	// Verify every component references a valid workspace.
	for name, comp := range p.Components {
		if comp.Workspace == "" {
			return fmt.Errorf("%s.components.%s.workspace is required", prefix, name)
		}
		if _, ok := p.Workspaces[comp.Workspace]; !ok {
			if !workspaceExistsCaseInsensitive(p.Workspaces, comp.Workspace) {
				return fmt.Errorf("%s.components.%s.workspace: workspace %q does not exist", prefix, name, comp.Workspace)
			}
		}
	}

	if p.MaxTicketCostUSD != nil && (math.IsNaN(*p.MaxTicketCostUSD) || math.IsInf(*p.MaxTicketCostUSD, 0)) {
		return fmt.Errorf("%s.max_ticket_cost_usd must be a finite number", prefix)
	}

	if len(p.TriageLabels.Active) > 0 {
		for _, label := range p.TriageLabels.Active {
			if label == "" {
				return fmt.Errorf("%s.triage_labels.active must not contain empty strings", prefix)
			}
		}
		if p.TriageLabels.NewStatus == "" {
			return fmt.Errorf("%s.triage_labels.new_status is required when active labels are configured", prefix)
		}
		if p.TriageLabels.Stale == "" {
			return fmt.Errorf("%s.triage_labels.stale is required when active labels are configured", prefix)
		}
		for _, label := range p.TriageLabels.Active {
			if strings.EqualFold(p.TriageLabels.Stale, label) {
				return fmt.Errorf("%s.triage_labels.stale %q must not overlap an active label", prefix, p.TriageLabels.Stale)
			}
		}
	}

	return nil
}

func profileExistsCaseInsensitive(profiles map[string]Profile, name string) bool {
	lower := strings.ToLower(name)
	for key := range profiles {
		if strings.ToLower(key) == lower {
			return true
		}
	}
	return false
}

func workspaceExistsCaseInsensitive(workspaces map[string]WorkspaceConfig, name string) bool {
	lower := strings.ToLower(name)
	for key := range workspaces {
		if strings.ToLower(key) == lower {
			return true
		}
	}
	return false
}

// repoNameFromURL extracts a short repo name from a clone URL.
// e.g., "https://github.com/org/backend.git" -> "backend"
func repoNameFromURL(rawURL string) string {
	trimmed := strings.TrimRight(rawURL, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 {
		return rawURL
	}
	return strings.TrimSuffix(parts[len(parts)-1], ".git")
}

// validate checks guardrails configuration values.
func (g *GuardrailsConfig) validate() error {
	if math.IsNaN(g.MaxDailyCostUSD) || math.IsInf(g.MaxDailyCostUSD, 0) {
		return errors.New("guardrails.max_daily_cost_usd must be a finite number")
	}
	if math.IsNaN(g.MaxTicketCostUSD) || math.IsInf(g.MaxTicketCostUSD, 0) {
		return errors.New("guardrails.max_ticket_cost_usd must be a finite number")
	}
	if g.MaxConcurrentJobs <= 0 {
		return errors.New("guardrails.max_concurrent_jobs must be positive")
	}
	if g.MaxContainerRuntimeMinutes < 0 {
		return errors.New("guardrails.max_container_runtime_minutes must be non-negative")
	}
	if g.CircuitBreakerThreshold < 0 {
		return errors.New("guardrails.circuit_breaker_threshold must be non-negative")
	}
	if g.CircuitBreakerWindowMinutes < 0 {
		return errors.New("guardrails.circuit_breaker_window_minutes must be non-negative")
	}
	if g.CircuitBreakerCooldownMinutes < 0 {
		return errors.New("guardrails.circuit_breaker_cooldown_minutes must be non-negative")
	}
	if g.MaxCommitFiles < 0 {
		return errors.New("guardrails.max_commit_files must be non-negative")
	}
	return nil
}

// validate checks container configuration values.
func (cc *ContainerCfg) validate() error {
	// Empty runtime is treated as "auto" (the default).
	// Valid values must match container.RuntimeAuto/RuntimePodman/RuntimeDocker constants.
	if cc.Runtime != "" {
		validRuntimes := map[string]bool{"auto": true, "podman": true, "docker": true}
		if !validRuntimes[cc.Runtime] {
			return fmt.Errorf("container.runtime must be \"auto\", \"podman\", or \"docker\", got %q", cc.Runtime)
		}
	}
	return nil
}

// validateClaudeAuth checks that Claude authentication is configured
// correctly: exactly one of direct API key, Vertex AI credentials,
// or API proxy — never more than one.
func (c *Config) validateClaudeAuth() error {
	hasAPIKey := c.Claude.APIKey != ""
	hasVertexProject := c.Claude.VertexProjectID != ""
	hasVertexRegion := c.Claude.VertexRegion != ""
	hasVertexCreds := c.Claude.VertexCredentialsFile != ""
	hasAnyVertex := hasVertexProject || hasVertexRegion || hasVertexCreds
	hasAllVertex := hasVertexProject && hasVertexRegion && hasVertexCreds

	hasProxyURL := c.Claude.ProxyURL != ""
	hasProxyToken := c.Claude.ProxyAuthToken != ""
	hasAnyProxy := hasProxyURL || hasProxyToken
	hasAllProxy := hasProxyURL && hasProxyToken

	// Proxy fields must be set together.
	if hasAnyProxy && !hasAllProxy {
		if !hasProxyURL {
			return errors.New("claude: proxy_auth_token is set but proxy_url is missing")
		}
		return errors.New("claude: proxy_url is set but proxy_auth_token is missing")
	}

	// Count active modes for mutual-exclusivity check.
	modes := 0
	if hasAPIKey {
		modes++
	}
	if hasVertexCreds {
		modes++
	}
	if hasAllProxy {
		modes++
	}
	if modes > 1 {
		return errors.New("claude: api_key, vertex_credentials_file, and proxy_url are mutually exclusive — configure exactly one authentication mode")
	}

	// Vertex (non-proxy) requires all three fields together.
	if hasVertexCreds {
		if hasAnyVertex && !hasAllVertex {
			var missing []string
			if !hasVertexProject {
				missing = append(missing, "vertex_project_id")
			}
			if !hasVertexRegion {
				missing = append(missing, "vertex_region")
			}
			if !hasVertexCreds {
				missing = append(missing, "vertex_credentials_file")
			}
			return fmt.Errorf("claude: incomplete Vertex AI configuration — missing: %s", strings.Join(missing, ", "))
		}
	}

	// Vertex fields without vertex_credentials_file are only valid
	// alongside proxy_vertex_format.
	if !hasVertexCreds && !hasAllProxy && hasAnyVertex {
		var missing []string
		if !hasVertexProject {
			missing = append(missing, "vertex_project_id")
		}
		if !hasVertexRegion {
			missing = append(missing, "vertex_region")
		}
		missing = append(missing, "vertex_credentials_file")
		return fmt.Errorf("claude: incomplete Vertex AI configuration — missing: %s", strings.Join(missing, ", "))
	}

	// Proxy with vertex format requires vertex_project_id and vertex_region.
	if hasAllProxy && c.Claude.ProxyVertexFormat {
		if !hasVertexProject || !hasVertexRegion {
			var missing []string
			if !hasVertexProject {
				missing = append(missing, "vertex_project_id")
			}
			if !hasVertexRegion {
				missing = append(missing, "vertex_region")
			}
			return fmt.Errorf("claude: proxy_vertex_format requires %s", strings.Join(missing, " and "))
		}
	}

	// When ai_provider is claude, at least one auth mode must be set.
	if c.AIProvider == "claude" && !hasAPIKey && !hasAllVertex && !hasAllProxy {
		return errors.New("claude: ai_provider is \"claude\" but no authentication configured — set api_key, all vertex_* fields, or proxy_url + proxy_auth_token")
	}

	return nil
}
