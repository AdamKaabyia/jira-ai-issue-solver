package models

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// createTempKeyFile creates a temporary key file for testing
func createTempKeyFile(t *testing.T) string {
	tmpKeyFile, err := os.CreateTemp("", "test_key_*.pem")
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpKeyFile.Close(); err != nil {
		t.Fatal(err)
	}
	return tmpKeyFile.Name()
}

// getValidGitHubConfig returns a valid GitHub configuration for testing
func getValidGitHubConfig() struct {
	AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
	PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
	BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
	BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
	PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
	SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
	MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
	KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
	IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
	IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
	SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
} {
	return struct {
		AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
		PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
		BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
		BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
		PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
		SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
		MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
		KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
		IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
		IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
		SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
	}{
		AppID:          123456,
		PrivateKeyPath: "/tmp/test_key.pem",
		BotEmail:       "test@example.com",
		BotUsername:    "test-bot",
	}
}

func TestConfig_validateStatusTransitions(t *testing.T) {
	// Create a temporary private key file for tests
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{
			name: "valid status transitions",
			config: Config{
				AIProvider: "claude",
				Logging: struct {
					Level  LogLevel  `yaml:"level" mapstructure:"level" default:"info"`
					Format LogFormat `yaml:"format" mapstructure:"format" default:"console"`
				}{
					Level:  LogLevelInfo,
					Format: LogFormatConsole,
				},
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					AssigneeToGitHubUsername: map[string]string{
						"alice@example.com": "alice",
					},
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: struct {
					AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
					PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
					BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
					BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
					PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
					SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
					MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
					KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
					IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
					IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
					SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
				}{
					AppID:          123456,
					PrivateKeyPath: tmpKeyPath,
					BotUsername:    "test-bot",
				},
				Workspaces: WorkspacesConfig{
					BaseDir: "/tmp/test-workspaces",
					TTLDays: 7,
				},
				Guardrails: GuardrailsConfig{
					MaxConcurrentJobs: 10,
				},
			},
			wantErr: false,
		},
		{
			name: "empty todo status",
			config: Config{
				AIProvider: "claude",
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: getValidGitHubConfig(),
			},
			wantErr: true,
		},
		{
			name: "empty in_progress status",
			config: Config{
				AIProvider: "claude",
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: getValidGitHubConfig(),
			},
			wantErr: true,
		},
		{
			name: "empty in_review status",
			config: Config{
				AIProvider: "claude",
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: getValidGitHubConfig(),
			},
			wantErr: true,
		},
		{
			name: "no ticket types configured",
			config: Config{
				AIProvider: "claude",
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys:       ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: getValidGitHubConfig(),
			},
			wantErr: true,
		},
		{
			name: "no project keys configured",
			config: Config{
				AIProvider: "claude",
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: getValidGitHubConfig(),
			},
			wantErr: true,
		},
		{
			name: "one ticket type valid, one invalid",
			config: Config{
				AIProvider: "claude",
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "Open",
									InProgress: "In Progress",
									InReview:   "Code Review",
								},
								"Story": StatusTransitions{
									Todo:       "Backlog",
									InProgress: "", // Invalid - missing in_progress
									InReview:   "Testing",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: getValidGitHubConfig(),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.AIProvider == "claude" {
				tt.config.Claude.APIKey = "sk-test"
			}
			err := tt.config.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Config.validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfig_WithStatusTransitions(t *testing.T) {
	// Create a temporary private key file
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	// Create a temporary config file
	configContent := fmt.Sprintf(`
logging:
  level: info
  format: console
ai_provider: "claude"
claude:
  api_key: sk-test
jira:
  base_url: "https://example.com"
  username: "testuser"
  api_token: "testtoken"
  assignee_to_github_username:
    alice@example.com: alice
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: https://github.com/test/repo.git
              profile: default
      components:
        test:
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "%s"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`, tmpKeyPath)
	tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpfile.Name()) }()

	if _, err := tmpfile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	// Load the config
	config, err := LoadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify status transitions
	projectConfig := config.GetProjectConfigForTicket("PROJ1-123")
	if projectConfig == nil {
		t.Fatal("Project config not found")
		return
	}
	bugTransitions := projectConfig.StatusTransitions.GetStatusTransitions("bug")
	if bugTransitions.Todo != "To Do" {
		t.Errorf("Expected todo status 'To Do', got '%s'", bugTransitions.Todo)
	}
	if bugTransitions.InProgress != "In Progress" {
		t.Errorf("Expected in_progress status 'In Progress', got '%s'", bugTransitions.InProgress)
	}
	if bugTransitions.InReview != "In Review" {
		t.Errorf("Expected in_review status 'In Review', got '%s'", bugTransitions.InReview)
	}
}

func TestLoadConfig_ComponentToWorkspaceCaseSensitivity(t *testing.T) {
	// Create a temporary private key file
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	// Create a temporary config file with mixed case component names
	configContent := fmt.Sprintf(`
logging:
  level: info
  format: console
ai_provider: "claude"
claude:
  api_key: sk-test
jira:
  base_url: "https://example.com"
  username: "testuser"
  api_token: "testtoken"
  assignee_to_github_username:
    alice@example.com: alice
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        flightctl:
          repos:
            - name: flightctl
              url: https://github.com/your-org/flightctl.git
              profile: default
        backend-ws:
          repos:
            - name: backend
              url: https://github.com/your-org/backend.git
              profile: default
      components:
        FlightCtl:
          workspace: flightctl
        Backend:
          workspace: backend-ws
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "%s"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`, tmpKeyPath)
	tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpfile.Name()) }()

	if _, err := tmpfile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	// Load the config
	config, err := LoadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Get project config for testing
	projectConfig := config.GetProjectConfigForTicket("PROJ1-123")
	if projectConfig == nil {
		t.Fatal("Project config not found")
		return
	}

	// Viper lowercases YAML map keys, so component names are lowercased
	if projectConfig.Components["flightctl"].Workspace != "flightctl" {
		t.Errorf("Expected flightctl component to map to workspace 'flightctl', got '%s'", projectConfig.Components["flightctl"].Workspace)
	}
	if projectConfig.Components["backend"].Workspace != "backend-ws" {
		t.Errorf("Expected backend component to map to workspace 'backend-ws', got '%s'", projectConfig.Components["backend"].Workspace)
	}
}

func TestLoadConfig_WithTicketTypeSpecificStatusTransitions(t *testing.T) {
	// Create a temporary private key file
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	// Create a temporary config file with ticket-type-specific status transitions
	configContent := fmt.Sprintf(`
logging:
  level: info
  format: console
ai_provider: "claude"
claude:
  api_key: sk-test
jira:
  base_url: "https://example.com"
  username: "testuser"
  api_token: "testtoken"
  assignee_to_github_username:
    alice@example.com: alice
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "Open"
          in_progress: "In Progress"
          in_review: "Code Review"
        story:
          todo: "Backlog"
          in_progress: "Development"
          in_review: "Testing"
      workspaces:
        default:
          repos:
            - name: repo
              url: https://github.com/test/repo.git
              profile: default
      components:
        test:
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "%s"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`, tmpKeyPath)
	tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpfile.Name()) }()

	if _, err := tmpfile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	// Load the config
	config, err := LoadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify bug-specific status transitions
	projectConfig := config.GetProjectConfigForTicket("PROJ1-123")
	if projectConfig == nil {
		t.Fatal("Project config not found")
		return
	}
	bugTransitions := projectConfig.StatusTransitions.GetStatusTransitions("bug")
	if bugTransitions.Todo != "Open" {
		t.Errorf("Expected bug todo status 'Open', got '%s'", bugTransitions.Todo)
	}
	if bugTransitions.InProgress != "In Progress" {
		t.Errorf("Expected bug in_progress status 'In Progress', got '%s'", bugTransitions.InProgress)
	}
	if bugTransitions.InReview != "Code Review" {
		t.Errorf("Expected bug in_review status 'Code Review', got '%s'", bugTransitions.InReview)
	}

	// Verify story-specific status transitions
	storyTransitions := projectConfig.StatusTransitions.GetStatusTransitions("story")
	if storyTransitions.Todo != "Backlog" {
		t.Errorf("Expected story todo status 'Backlog', got '%s'", storyTransitions.Todo)
	}
	if storyTransitions.InProgress != "Development" {
		t.Errorf("Expected story in_progress status 'Development', got '%s'", storyTransitions.InProgress)
	}
	if storyTransitions.InReview != "Testing" {
		t.Errorf("Expected story in_review status 'Testing', got '%s'", storyTransitions.InReview)
	}

	// Verify that unknown ticket type returns empty transitions (no fallback)
	unknownTransitions := projectConfig.StatusTransitions.GetStatusTransitions("unknown")
	if unknownTransitions.Todo != "" {
		t.Errorf("Expected unknown ticket type to return empty todo, got '%s'", unknownTransitions.Todo)
	}
}

func TestTicketTypeStatusTransitions_GetStatusTransitions(t *testing.T) {
	tests := []struct {
		name           string
		transitions    TicketTypeStatusTransitions
		ticketType     string
		expectedTodo   string
		expectedInProg string
		expectedInRev  string
	}{
		{
			name: "specific ticket type found",
			transitions: TicketTypeStatusTransitions{
				"default": StatusTransitions{
					Todo:       "To Do",
					InProgress: "In Progress",
					InReview:   "In Review",
				},
				"Bug": StatusTransitions{
					Todo:       "Open",
					InProgress: "In Progress",
					InReview:   "Code Review",
				},
			},
			ticketType:     "Bug",
			expectedTodo:   "Open",
			expectedInProg: "In Progress",
			expectedInRev:  "Code Review",
		},
		{
			name: "ticket type not found, no fallback",
			transitions: TicketTypeStatusTransitions{
				"Bug": StatusTransitions{
					Todo:       "Open",
					InProgress: "In Progress",
					InReview:   "Code Review",
				},
			},
			ticketType:     "Story",
			expectedTodo:   "",
			expectedInProg: "",
			expectedInRev:  "",
		},
		{
			name: "no default, return empty transitions",
			transitions: TicketTypeStatusTransitions{
				"Bug": StatusTransitions{
					Todo:       "Open",
					InProgress: "In Progress",
					InReview:   "Code Review",
				},
			},
			ticketType:     "Story",
			expectedTodo:   "",
			expectedInProg: "",
			expectedInRev:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.transitions.GetStatusTransitions(tt.ticketType)

			if result.Todo != tt.expectedTodo {
				t.Errorf("Expected Todo '%s', got '%s'", tt.expectedTodo, result.Todo)
			}
			if result.InProgress != tt.expectedInProg {
				t.Errorf("Expected InProgress '%s', got '%s'", tt.expectedInProg, result.InProgress)
			}
			if result.InReview != tt.expectedInRev {
				t.Errorf("Expected InReview '%s', got '%s'", tt.expectedInRev, result.InReview)
			}
		})
	}
}

func TestLoadConfig_WithAIConfiguration(t *testing.T) {
	// Create a temporary private key file
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	// Create a temporary config file
	configContent := fmt.Sprintf(`
ai_provider: claude
claude:
  api_key: sk-test
jira:
  base_url: https://test.atlassian.net
  username: test-user
  api_token: test-token
  assignee_to_github_username:
    alice@example.com: alice
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: "https://github.com/test/repo"
              profile: default
      components:
        "test-component":
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "%s"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`, tmpKeyPath)

	tempFile, err := os.CreateTemp("", "config_test_*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer func() { _ = os.Remove(tempFile.Name()) }()

	if _, err := tempFile.WriteString(configContent); err != nil {
		t.Fatalf("Failed to write config content: %v", err)
	}
	_ = tempFile.Close()

	// Load the config
	config, err := LoadConfig(tempFile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify AI provider
	if config.AIProvider != "claude" {
		t.Errorf("Expected AI provider to be 'claude', got '%s'", config.AIProvider)
	}
}

// TestConfig_GitHubAppAuthentication tests GitHub App configuration and validation
func TestConfig_GitHubAppAuthentication(t *testing.T) {
	// Create a temporary private key file for testing
	tempKeyFile, err := os.CreateTemp("", "github-app-key-*.pem")
	if err != nil {
		t.Fatalf("Failed to create temp key file: %v", err)
	}
	defer func() { _ = os.Remove(tempKeyFile.Name()) }()

	// Write a dummy private key content
	if _, err := tempKeyFile.WriteString("-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----"); err != nil {
		t.Fatalf("Failed to write to temp key file: %v", err)
	}
	if err := tempKeyFile.Close(); err != nil {
		t.Fatalf("Failed to close temp key file: %v", err)
	}

	tests := []struct {
		name    string
		config  Config
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid GitHub App configuration",
			config: Config{
				AIProvider: "claude",
				Logging: struct {
					Level  LogLevel  `yaml:"level" mapstructure:"level" default:"info"`
					Format LogFormat `yaml:"format" mapstructure:"format" default:"console"`
				}{
					Level:  LogLevelInfo,
					Format: LogFormatConsole,
				},
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					AssigneeToGitHubUsername: map[string]string{
						"alice@example.com": "alice",
						"bob@example.com":   "bob",
					},
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: struct {
					AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
					PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
					BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
					BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
					PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
					SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
					MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
					KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
					IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
					IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
					SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
				}{
					AppID:          123456,
					PrivateKeyPath: tempKeyFile.Name(),
					BotUsername:    "test-bot[bot]",
				},
				Workspaces: WorkspacesConfig{
					BaseDir: "/tmp/test-workspaces",
					TTLDays: 7,
				},
				Guardrails: GuardrailsConfig{
					MaxConcurrentJobs: 10,
				},
			},
			wantErr: false,
		},
		{
			name: "GitHub App without app_id (should fail)",
			config: Config{
				AIProvider: "claude",
				Logging: struct {
					Level  LogLevel  `yaml:"level" mapstructure:"level" default:"info"`
					Format LogFormat `yaml:"format" mapstructure:"format" default:"console"`
				}{
					Level:  LogLevelInfo,
					Format: LogFormatConsole,
				},
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: struct {
					AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
					PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
					BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
					BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
					PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
					SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
					MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
					KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
					IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
					IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
					SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
				}{
					PrivateKeyPath: tempKeyFile.Name(),
					BotUsername:    "test-bot",
				},
			},
			wantErr: true,
			errMsg:  "github.app_id must be a positive integer",
		},
		{
			name: "GitHub App with non-existent private key file (should fail)",
			config: Config{
				AIProvider: "claude",
				Logging: struct {
					Level  LogLevel  `yaml:"level" mapstructure:"level" default:"info"`
					Format LogFormat `yaml:"format" mapstructure:"format" default:"console"`
				}{
					Level:  LogLevelInfo,
					Format: LogFormatConsole,
				},
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					AssigneeToGitHubUsername: map[string]string{
						"alice@example.com": "alice",
					},
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: struct {
					AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
					PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
					BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
					BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
					PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
					SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
					MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
					KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
					IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
					IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
					SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
				}{
					AppID:          123456,
					PrivateKeyPath: "/non/existent/path/key.pem",
					BotUsername:    "test-bot",
				},
			},
			wantErr: true,
			errMsg:  "github.private_key_path file does not exist",
		},
		{
			name: "GitHub App without assignee mapping (valid for direct-mode projects)",
			config: Config{
				AIProvider: "claude",
				Logging: struct {
					Level  LogLevel  `yaml:"level" mapstructure:"level" default:"info"`
					Format LogFormat `yaml:"format" mapstructure:"format" default:"console"`
				}{
					Level:  LogLevelInfo,
					Format: LogFormatConsole,
				},
				Jira: JiraConfig{
					BaseURL:  "https://example.com",
					Username: "testuser",
					APIToken: "testtoken",
					Projects: []ProjectConfig{
						{
							ProjectKeys: ProjectKeys{"PROJ1"},
							StatusTransitions: TicketTypeStatusTransitions{
								"Bug": StatusTransitions{
									Todo:       "To Do",
									InProgress: "In Progress",
									InReview:   "In Review",
								},
							},
							Components: ComponentMap{
								"test": ComponentConfig{Workspace: "default"},
							},
							Workspaces: map[string]WorkspaceConfig{
								"default": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/test/repo.git", Profile: "default"}}},
							},
							Profiles: map[string]Profile{
								"default": {},
							},
						},
					},
				},
				GitHub: struct {
					AppID             int64    `yaml:"app_id" mapstructure:"app_id"`
					PrivateKeyPath    string   `yaml:"private_key_path" mapstructure:"private_key_path"`
					BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
					BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
					PRLabel           string   `yaml:"pr_label" mapstructure:"pr_label" default:"ai-pr"`
					SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
					MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth" default:"5"`
					KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
					IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
					IgnoredCheckNames []string `yaml:"ignored_check_names" mapstructure:"ignored_check_names"`
					SkipPRLabel       string   `yaml:"skip_pr_label" mapstructure:"skip_pr_label" default:"ai-bot-skip"`
				}{
					AppID:          123456,
					PrivateKeyPath: tempKeyFile.Name(),
					BotUsername:    "test-bot",
				},
				Workspaces: WorkspacesConfig{BaseDir: "/tmp/ws", TTLDays: 7},
				Guardrails: GuardrailsConfig{MaxConcurrentJobs: 1, MaxRetries: 1},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.AIProvider == "claude" {
				tt.config.Claude.APIKey = "sk-test"
			}
			err := tt.config.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Config.validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("Config.validate() error = %v, want error containing %q", err, tt.errMsg)
				}
			}
		})
	}
}

// TestLoadConfig_WithAssigneeToGitHubUsername tests loading assignee_to_github_username from YAML
func TestLoadConfig_WithAssigneeToGitHubUsername(t *testing.T) {
	// Create a temporary private key file for GitHub App
	tempKeyFile, err := os.CreateTemp("", "github-app-key-*.pem")
	if err != nil {
		t.Fatalf("Failed to create temp key file: %v", err)
	}
	defer func() { _ = os.Remove(tempKeyFile.Name()) }()

	if _, err := tempKeyFile.WriteString("-----BEGIN RSA PRIVATE KEY-----\ntest\n-----END RSA PRIVATE KEY-----"); err != nil {
		t.Fatalf("Failed to write to temp key file: %v", err)
	}
	if err := tempKeyFile.Close(); err != nil {
		t.Fatalf("Failed to close temp key file: %v", err)
	}

	// Create a temporary config file with assignee_to_github_username
	configContent := `
logging:
  level: info
  format: console
ai_provider: "claude"
claude:
  api_key: sk-test
jira:
  base_url: "https://example.com"
  username: "testuser"
  api_token: "testtoken"
  assignee_to_github_username:
    "alice@example.com": alice-github
    "bob.smith@company.com": bob-smith
    "charlie+tag@domain.org": charlie
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        Bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: https://github.com/test/repo.git
              profile: default
      components:
        test:
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "` + tempKeyFile.Name() + `"
  bot_username: "test-bot[bot]"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`
	tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Remove(tmpfile.Name()) }()

	if _, err := tmpfile.Write([]byte(configContent)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	// Load the config
	config, err := LoadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	// Verify assignee_to_github_username was loaded correctly
	if config.Jira.AssigneeToGitHubUsername == nil {
		t.Fatal("AssigneeToGitHubUsername is nil")
	}

	expectedMappings := map[string]string{
		"alice@example.com":      "alice-github",
		"bob.smith@company.com":  "bob-smith",
		"charlie+tag@domain.org": "charlie",
	}

	if len(config.Jira.AssigneeToGitHubUsername) != len(expectedMappings) {
		t.Errorf("Expected %d mappings, got %d", len(expectedMappings), len(config.Jira.AssigneeToGitHubUsername))
	}

	for email, expectedUsername := range expectedMappings {
		actualUsername, exists := config.Jira.AssigneeToGitHubUsername[email]
		if !exists {
			t.Errorf("Expected mapping for %s not found", email)
			continue
		}
		if actualUsername != expectedUsername {
			t.Errorf("For email %s: expected username %s, got %s", email, expectedUsername, actualUsername)
		}
	}
}

func TestConfig_validateWorkspacesConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		baseDir       string
		ttlDays       int
		expectedError string
	}{
		{
			name:          "valid workspace config",
			baseDir:       "/var/lib/ai-bot/workspaces",
			ttlDays:       7,
			expectedError: "",
		},
		{
			name:          "missing base_dir",
			baseDir:       "",
			ttlDays:       7,
			expectedError: "workspaces.base_dir is required",
		},
		{
			name:          "zero ttl_days",
			baseDir:       "/tmp/workspaces",
			ttlDays:       0,
			expectedError: "workspaces.ttl_days must be positive",
		},
		{
			name:          "negative ttl_days",
			baseDir:       "/tmp/workspaces",
			ttlDays:       -1,
			expectedError: "workspaces.ttl_days must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyPath := createTempKeyFile(t)
			defer func() { _ = os.Remove(keyPath) }()

			config := &Config{}
			config.AIProvider = "claude"
			config.Claude.APIKey = "sk-test"
			config.Logging.Level = "info"
			config.Logging.Format = "console"
			config.Jira.BaseURL = "https://test.atlassian.net"
			config.Jira.Username = "test@example.com"
			config.Jira.APIToken = "test-token"
			config.Jira.AssigneeToGitHubUsername = map[string]string{
				"test@example.com": "test-user",
			}
			config.Jira.Projects = []ProjectConfig{
				{
					ProjectKeys: ProjectKeys{"TEST"},
					StatusTransitions: TicketTypeStatusTransitions{
						"Bug": StatusTransitions{
							Todo:       "To Do",
							InProgress: "In Progress",
							InReview:   "In Review",
						},
					},
					Components: ComponentMap{
						"component1": ComponentConfig{Workspace: "default"},
					},
					Workspaces: map[string]WorkspaceConfig{
						"default": {Repos: []RepoEntry{{Name: "repo1", URL: "https://github.com/test/repo1.git", Profile: "default"}}},
					},
					Profiles: map[string]Profile{
						"default": {},
					},
				},
			}
			config.GitHub.AppID = 123456
			config.GitHub.PrivateKeyPath = keyPath
			config.GitHub.BotUsername = "test-bot"
			config.Workspaces.BaseDir = tt.baseDir
			config.Workspaces.TTLDays = tt.ttlDays
			config.Guardrails.MaxConcurrentJobs = 10

			err := config.validate()

			if tt.expectedError == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got: %v", tt.expectedError, err)
				}
			}
		})
	}
}

func TestConfig_validateClaudeAuth(t *testing.T) {
	// validBaseConfig builds a Config that passes all validation except
	// Claude auth (which is the subject under test). Call it per
	// subtest so each gets its own temp key file.
	validBaseConfig := func(t *testing.T) *Config {
		t.Helper()
		keyPath := createTempKeyFile(t)
		t.Cleanup(func() { _ = os.Remove(keyPath) })

		config := &Config{}
		config.AIProvider = "claude"
		config.Logging.Level = "info"
		config.Logging.Format = "console"
		config.Jira.BaseURL = "https://test.atlassian.net"
		config.Jira.Username = "test@example.com"
		config.Jira.APIToken = "test-token"
		config.Jira.AssigneeToGitHubUsername = map[string]string{
			"test@example.com": "test-user",
		}
		config.Jira.Projects = []ProjectConfig{
			{
				ProjectKeys: ProjectKeys{"TEST"},
				StatusTransitions: TicketTypeStatusTransitions{
					"Bug": StatusTransitions{
						Todo:       "To Do",
						InProgress: "In Progress",
						InReview:   "In Review",
					},
				},
				Components: ComponentMap{
					"component1": ComponentConfig{Workspace: "default"},
				},
				Workspaces: map[string]WorkspaceConfig{
					"default": {Repos: []RepoEntry{{Name: "repo1", URL: "https://github.com/test/repo1.git", Profile: "default"}}},
				},
				Profiles: map[string]Profile{
					"default": {},
				},
			},
		}
		config.GitHub.AppID = 123456
		config.GitHub.PrivateKeyPath = keyPath
		config.GitHub.BotUsername = "test-bot"
		config.Workspaces.BaseDir = "/var/lib/workspaces"
		config.Workspaces.TTLDays = 7
		config.Guardrails.MaxConcurrentJobs = 10
		return config
	}

	t.Run("no claude auth is valid for non-claude provider", func(t *testing.T) {
		config := validBaseConfig(t)
		config.AIProvider = "gemini"
		config.Gemini.APIKey = "gemini-key"
		if err := config.validate(); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("api_key only is valid", func(t *testing.T) {
		config := validBaseConfig(t)
		config.Claude.APIKey = "sk-ant-test-key"
		if err := config.validate(); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("vertex complete config is valid", func(t *testing.T) {
		config := validBaseConfig(t)
		config.Claude.VertexProjectID = "my-project"
		config.Claude.VertexRegion = "us-east5"
		config.Claude.VertexCredentialsFile = "/host/path/to/sa-key.json"
		if err := config.validate(); err != nil {
			t.Errorf("expected no error, got: %v", err)
		}
	})

	t.Run("api_key and vertex are mutually exclusive", func(t *testing.T) {
		config := validBaseConfig(t)
		config.Claude.APIKey = "sk-ant-test-key"
		config.Claude.VertexProjectID = "my-project"
		err := config.validate()
		if err == nil {
			t.Fatal("expected error for mutually exclusive config")
		}
		if !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("error = %q, want 'mutually exclusive'", err.Error())
		}
	})

	t.Run("claude provider requires auth", func(t *testing.T) {
		config := validBaseConfig(t)
		config.AIProvider = "claude"
		err := config.validate()
		if err == nil {
			t.Fatal("expected error when ai_provider=claude but no auth configured")
		}
		if !strings.Contains(err.Error(), "no authentication configured") {
			t.Errorf("error = %q, want mention of missing authentication", err.Error())
		}
	})

	t.Run("non-claude provider allows no claude auth", func(t *testing.T) {
		config := validBaseConfig(t)
		config.AIProvider = "gemini"
		if err := config.validate(); err != nil {
			t.Errorf("expected no error for non-claude provider without claude auth, got: %v", err)
		}
	})

	t.Run("incomplete vertex missing region", func(t *testing.T) {
		config := validBaseConfig(t)
		config.Claude.VertexProjectID = "my-project"
		config.Claude.VertexCredentialsFile = "/host/path/to/sa-key.json"
		err := config.validate()
		if err == nil {
			t.Fatal("expected error for incomplete vertex config")
		}
		if !strings.Contains(err.Error(), "vertex_region") {
			t.Errorf("error = %q, should mention vertex_region", err.Error())
		}
	})

	t.Run("incomplete vertex missing project and creds", func(t *testing.T) {
		config := validBaseConfig(t)
		config.Claude.VertexRegion = "us-east5"
		err := config.validate()
		if err == nil {
			t.Fatal("expected error for incomplete vertex config")
		}
		if !strings.Contains(err.Error(), "vertex_project_id") {
			t.Errorf("error = %q, should mention vertex_project_id", err.Error())
		}
		if !strings.Contains(err.Error(), "vertex_credentials_file") {
			t.Errorf("error = %q, should mention vertex_credentials_file", err.Error())
		}
	})

}

func TestGuardrailsConfig_ValidateMaxCommitFiles(t *testing.T) {
	tests := []struct {
		name          string
		value         int
		expectedError string
	}{
		{name: "positive value is valid", value: 100},
		{name: "zero is valid (disables limit)", value: 0},
		{name: "negative value is invalid", value: -1, expectedError: "guardrails.max_commit_files must be non-negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GuardrailsConfig{
				MaxConcurrentJobs: 1,
				MaxCommitFiles:    tt.value,
			}
			err := g.validate()
			if tt.expectedError == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got: %v", tt.expectedError, err)
				}
			}
		})
	}
}

func TestConfig_validateContainerConfiguration(t *testing.T) {
	tests := []struct {
		name          string
		runtime       string
		expectedError string
	}{
		{
			name:          "valid runtime auto",
			runtime:       "auto",
			expectedError: "",
		},
		{
			name:          "valid runtime podman",
			runtime:       "podman",
			expectedError: "",
		},
		{
			name:          "valid runtime docker",
			runtime:       "docker",
			expectedError: "",
		},
		{
			name:          "empty runtime is valid (treated as auto)",
			runtime:       "",
			expectedError: "",
		},
		{
			name:          "invalid runtime",
			runtime:       "containerd",
			expectedError: `container.runtime must be "auto", "podman", or "docker"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyPath := createTempKeyFile(t)
			defer func() { _ = os.Remove(keyPath) }()

			config := &Config{}
			config.AIProvider = "claude"
			config.Claude.APIKey = "sk-test"
			config.Logging.Level = "info"
			config.Logging.Format = "console"
			config.Jira.BaseURL = "https://test.atlassian.net"
			config.Jira.Username = "test@example.com"
			config.Jira.APIToken = "test-token"
			config.Jira.AssigneeToGitHubUsername = map[string]string{
				"test@example.com": "test-user",
			}
			config.Jira.Projects = []ProjectConfig{
				{
					ProjectKeys: ProjectKeys{"TEST"},
					StatusTransitions: TicketTypeStatusTransitions{
						"Bug": StatusTransitions{
							Todo:       "To Do",
							InProgress: "In Progress",
							InReview:   "In Review",
						},
					},
					Components: ComponentMap{
						"component1": ComponentConfig{Workspace: "default"},
					},
					Workspaces: map[string]WorkspaceConfig{
						"default": {Repos: []RepoEntry{{Name: "repo1", URL: "https://github.com/test/repo1.git", Profile: "default"}}},
					},
					Profiles: map[string]Profile{
						"default": {},
					},
				},
			}
			config.GitHub.AppID = 123456
			config.GitHub.PrivateKeyPath = keyPath
			config.GitHub.BotUsername = "test-bot"
			config.Workspaces.BaseDir = "/var/lib/workspaces"
			config.Workspaces.TTLDays = 7
			config.Container.Runtime = tt.runtime
			config.Guardrails.MaxConcurrentJobs = 10

			err := config.validate()

			if tt.expectedError == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got: %v", tt.expectedError, err)
				}
			}
		})
	}
}

func TestConfig_validateWorkspaceConfiguration(t *testing.T) {
	keyPath := createTempKeyFile(t)
	t.Cleanup(func() { _ = os.Remove(keyPath) })

	validBase := func() *Config {
		config := &Config{}
		config.Logging.Level = "info"
		config.Logging.Format = "console"
		config.AIProvider = "claude"
		config.Claude.APIKey = "sk-test"
		config.Jira.BaseURL = "https://test.atlassian.net"
		config.Jira.Username = "test@example.com"
		config.Jira.APIToken = "test-token"
		config.Jira.AssigneeToGitHubUsername = map[string]string{
			"test@example.com": "test-user",
		}
		config.GitHub.AppID = 123456
		config.GitHub.PrivateKeyPath = keyPath
		config.GitHub.BotUsername = "test-bot"
		config.Workspaces.BaseDir = "/var/lib/workspaces"
		config.Workspaces.TTLDays = 7
		config.Guardrails.MaxConcurrentJobs = 10
		return config
	}

	baseProject := func() ProjectConfig {
		return ProjectConfig{
			ProjectKeys: ProjectKeys{"PROJ"},
			StatusTransitions: TicketTypeStatusTransitions{
				"Story": {Todo: "To Do", InProgress: "In Progress", InReview: "In Review"},
			},
			Profiles: map[string]Profile{"default": {}},
		}
	}

	tests := []struct {
		name          string
		setup         func(*Config)
		expectedError string
	}{
		{
			name: "valid single-repo workspace with component",
			setup: func(c *Config) {
				p := baseProject()
				p.Profiles = map[string]Profile{"go": {}}
				p.Workspaces = map[string]WorkspaceConfig{
					"backend": {
						Repos: []RepoEntry{{Name: "api", URL: "https://github.com/org/api", Profile: "go"}},
					},
				}
				p.Components = ComponentMap{"backend-api": {Workspace: "backend"}}
				c.Jira.Projects = []ProjectConfig{p}
			},
		},
		{
			name: "valid multi-repo workspace",
			setup: func(c *Config) {
				p := baseProject()
				p.Profiles = map[string]Profile{"node": {}, "go": {}}
				p.Workspaces = map[string]WorkspaceConfig{
					"full-stack": {
						Repos: []RepoEntry{
							{Name: "frontend", URL: "https://github.com/org/frontend", Profile: "node"},
							{Name: "backend", URL: "https://github.com/org/backend", Profile: "go"},
						},
					},
				}
				p.DefaultWorkspace = "full-stack"
				c.Jira.Projects = []ProjectConfig{p}
			},
		},
		{
			name: "valid with default_workspace and no components",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"main": {
						Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo", Profile: "default"}},
					},
				}
				p.DefaultWorkspace = "main"
				c.Jira.Projects = []ProjectConfig{p}
			},
		},
		{
			name: "no workspaces configured",
			setup: func(c *Config) {
				p := baseProject()
				p.Components = ComponentMap{"comp1": {Workspace: "backend"}}
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "at least one workspace must be configured",
		},
		{
			name: "workspace with no repos",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"empty": {Repos: []RepoEntry{}},
				}
				p.DefaultWorkspace = "empty"
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "at least one repo is required",
		},
		{
			name: "repo missing name",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{URL: "https://github.com/org/repo"}}},
				}
				p.DefaultWorkspace = "ws"
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "name is required",
		},
		{
			name: "repo missing url",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo"}}},
				}
				p.DefaultWorkspace = "ws"
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "url is required",
		},
		{
			name: "repo references nonexistent profile",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo", Profile: "nonexistent"}}},
				}
				p.DefaultWorkspace = "ws"
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "profile \"nonexistent\" does not exist",
		},
		{
			name: "component references nonexistent workspace",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo", Profile: "default"}}},
				}
				p.Components = ComponentMap{"comp1": {Workspace: "nonexistent"}}
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "workspace \"nonexistent\" does not exist",
		},
		{
			name: "component missing workspace field",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo", Profile: "default"}}},
				}
				p.Components = ComponentMap{"comp1": {}}
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "workspace is required",
		},
		{
			name: "default_workspace references nonexistent workspace",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo", Profile: "default"}}},
				}
				p.Components = ComponentMap{"comp1": {Workspace: "ws"}}
				p.DefaultWorkspace = "nonexistent"
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "workspace \"nonexistent\" does not exist",
		},
		{
			name: "neither components nor default_workspace",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo", Profile: "default"}}},
				}
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "either components or default_workspace must be configured",
		},
		{
			name: "duplicate repo names in one workspace",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {
						Repos: []RepoEntry{
							{Name: "api", URL: "https://github.com/org/api-one", Profile: "default"},
							{Name: "api", URL: "https://github.com/org/api-two", Profile: "default"},
						},
					},
				}
				p.DefaultWorkspace = "ws"
				c.Jira.Projects = []ProjectConfig{p}
			},
			expectedError: "duplicate repo name",
		},
		{
			name: "repo with empty profile is valid",
			setup: func(c *Config) {
				p := baseProject()
				p.Workspaces = map[string]WorkspaceConfig{
					"ws": {Repos: []RepoEntry{{Name: "repo", URL: "https://github.com/org/repo"}}},
				}
				p.DefaultWorkspace = "ws"
				c.Jira.Projects = []ProjectConfig{p}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := validBase()
			tt.setup(config)

			err := config.validate()

			if tt.expectedError == "" {
				if err != nil {
					t.Errorf("expected no error, got: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.expectedError)
				} else if !strings.Contains(err.Error(), tt.expectedError) {
					t.Errorf("expected error containing %q, got: %v", tt.expectedError, err)
				}
			}
		})
	}
}

func TestLoadConfig_SkipPRLabel(t *testing.T) {
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	baseConfig := `
ai_provider: claude
claude:
  api_key: sk-test
jira:
  base_url: https://test.atlassian.net
  username: test-user
  api_token: test-token
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: "https://github.com/test/repo"
              profile: default
      components:
        "comp":
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "` + tmpKeyPath + `"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`

	t.Run("default value", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(baseConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		if config.GitHub.SkipPRLabel != "ai-bot-skip" {
			t.Errorf("SkipPRLabel = %q, want %q", config.GitHub.SkipPRLabel, "ai-bot-skip")
		}
	})

	t.Run("env var override", func(t *testing.T) {
		t.Setenv("JIRA_AI_GITHUB_SKIP_PR_LABEL", "do-not-touch")

		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(baseConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		if config.GitHub.SkipPRLabel != "do-not-touch" {
			t.Errorf("SkipPRLabel = %q, want %q", config.GitHub.SkipPRLabel, "do-not-touch")
		}
	})
}

func TestLoadConfig_PRValidationLabels(t *testing.T) {
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	baseConfig := `
ai_provider: claude
claude:
  api_key: sk-test
jira:
  base_url: https://test.atlassian.net
  username: test-user
  api_token: test-token
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: "https://github.com/test/repo"
              profile: default
      components:
        "comp":
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "` + tmpKeyPath + `"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`

	t.Run("empty when not configured", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(baseConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		vl := config.Jira.Projects[0].PRValidationLabels
		if vl.ValidationFailed != "" {
			t.Errorf("ValidationFailed = %q, want empty", vl.ValidationFailed)
		}
		if vl.NonzeroExit != "" {
			t.Errorf("NonzeroExit = %q, want empty", vl.NonzeroExit)
		}
		if vl.CostCapExceeded != nil {
			t.Errorf("CostCapExceeded = %v, want nil", vl.CostCapExceeded)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		customConfig := `
ai_provider: claude
claude:
  api_key: sk-test
jira:
  base_url: https://test.atlassian.net
  username: test-user
  api_token: test-token
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: "https://github.com/test/repo"
              profile: default
      components:
        "comp":
          workspace: default
      profiles:
        default: {}
      pr_validation_labels:
        validation_failed: "custom-vf"
        nonzero_exit: "custom-nze"
        cost_cap_exceeded: "custom-cce"
github:
  app_id: 123456
  private_key_path: "` + tmpKeyPath + `"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`
		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(customConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		vl := config.Jira.Projects[0].PRValidationLabels
		if vl.ValidationFailed != "custom-vf" {
			t.Errorf("ValidationFailed = %q, want custom-vf", vl.ValidationFailed)
		}
		if vl.NonzeroExit != "custom-nze" {
			t.Errorf("NonzeroExit = %q, want custom-nze", vl.NonzeroExit)
		}
		if vl.CostCapExceeded == nil || *vl.CostCapExceeded != "custom-cce" {
			t.Errorf("CostCapExceeded = %v, want ptr to custom-cce", vl.CostCapExceeded)
		}
	})
}

func TestPRValidationLabels_CostCapLabel(t *testing.T) {
	t.Run("nil pointer returns empty", func(t *testing.T) {
		vl := PRValidationLabels{CostCapExceeded: nil}
		if got := vl.CostCapLabel(); got != "" {
			t.Errorf("CostCapLabel() = %q, want empty", got)
		}
	})

	t.Run("empty string pointer returns empty", func(t *testing.T) {
		empty := ""
		vl := PRValidationLabels{CostCapExceeded: &empty}
		if got := vl.CostCapLabel(); got != "" {
			t.Errorf("CostCapLabel() = %q, want empty", got)
		}
	})

	t.Run("non-empty pointer returns value", func(t *testing.T) {
		label := "ai-budget-exceeded"
		vl := PRValidationLabels{CostCapExceeded: &label}
		if got := vl.CostCapLabel(); got != "ai-budget-exceeded" {
			t.Errorf("CostCapLabel() = %q, want ai-budget-exceeded", got)
		}
	})
}

func TestPRValidationLabels_All_IncludesCostCap(t *testing.T) {
	t.Run("includes cost cap label", func(t *testing.T) {
		label := "ai-budget-exceeded"
		vl := PRValidationLabels{
			ValidationFailed: "vf",
			NonzeroExit:      "nze",
			CostCapExceeded:  &label,
		}
		got := vl.All()
		want := []string{"vf", "nze", "ai-budget-exceeded"}
		if len(got) != len(want) {
			t.Fatalf("All() length = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("All()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("nil cost cap returns empty in slice", func(t *testing.T) {
		vl := PRValidationLabels{
			ValidationFailed: "vf",
			NonzeroExit:      "nze",
			CostCapExceeded:  nil,
		}
		got := vl.All()
		if len(got) != 3 {
			t.Fatalf("All() length = %d, want 3", len(got))
		}
		if got[2] != "" {
			t.Errorf("All()[2] = %q, want empty", got[2])
		}
	})
}

func TestLoadConfig_MaxTicketCostUSD(t *testing.T) {
	tmpKeyPath := createTempKeyFile(t)
	defer func() { _ = os.Remove(tmpKeyPath) }()

	baseConfig := `
ai_provider: claude
claude:
  api_key: sk-test
jira:
  base_url: https://test.atlassian.net
  username: test-user
  api_token: test-token
  projects:
    - project_keys:
        - "PROJ1"
      status_transitions:
        bug:
          todo: "To Do"
          in_progress: "In Progress"
          in_review: "In Review"
      workspaces:
        default:
          repos:
            - name: repo
              url: "https://github.com/test/repo"
              profile: default
      components:
        "comp":
          workspace: default
      profiles:
        default: {}
github:
  app_id: 123456
  private_key_path: "` + tmpKeyPath + `"
  bot_username: "test-bot"
workspaces:
  base_dir: /tmp/test-workspaces
  ttl_days: 7
`

	t.Run("default value", func(t *testing.T) {
		t.Setenv("JIRA_AI_GUARDRAILS_MAX_TICKET_COST_USD", "")
		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(baseConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		if config.Guardrails.MaxTicketCostUSD != 20.0 {
			t.Errorf("MaxTicketCostUSD = %v, want 20.0 (default)", config.Guardrails.MaxTicketCostUSD)
		}
	})

	t.Run("YAML override", func(t *testing.T) {
		t.Setenv("JIRA_AI_GUARDRAILS_MAX_TICKET_COST_USD", "")
		yamlConfig := baseConfig + `
guardrails:
  max_ticket_cost_usd: 50.0
`
		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(yamlConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		if config.Guardrails.MaxTicketCostUSD != 50.0 {
			t.Errorf("MaxTicketCostUSD = %v, want 50.0", config.Guardrails.MaxTicketCostUSD)
		}
	})

	t.Run("environment variable override", func(t *testing.T) {
		tmpfile, err := os.CreateTemp("", "config_test_*.yaml")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Remove(tmpfile.Name()) }()
		if _, err := tmpfile.WriteString(baseConfig); err != nil {
			t.Fatal(err)
		}
		_ = tmpfile.Close()

		t.Setenv("JIRA_AI_GUARDRAILS_MAX_TICKET_COST_USD", "75.0")

		config, err := LoadConfig(tmpfile.Name())
		if err != nil {
			t.Fatalf("Failed to load config: %v", err)
		}
		if config.Guardrails.MaxTicketCostUSD != 75.0 {
			t.Errorf("MaxTicketCostUSD = %v, want 75.0 (from env)", config.Guardrails.MaxTicketCostUSD)
		}
	})
}

func TestProjectConfig_ValidateTriageLabels(t *testing.T) {
	base := func() ProjectConfig {
		return ProjectConfig{
			ProjectKeys: ProjectKeys{"TEST"},
			StatusTransitions: TicketTypeStatusTransitions{
				"Bug": {Todo: "To Do", InProgress: "In Progress", InReview: "In Review"},
			},
			Components: ComponentMap{"c": ComponentConfig{Workspace: "default"}},
			Workspaces: map[string]WorkspaceConfig{
				"default": {Repos: []RepoEntry{{Name: "r", URL: "https://github.com/o/r.git", Profile: "p"}}},
			},
			Profiles: map[string]Profile{"p": {}},
		}
	}

	tests := []struct {
		name    string
		triage  TriageLabels
		wantErr string
	}{
		{
			name:    "no triage config is valid",
			triage:  TriageLabels{},
			wantErr: "",
		},
		{
			name:    "active set with empty new_status",
			triage:  TriageLabels{Active: []string{"label1"}, Stale: "stale", NewStatus: ""},
			wantErr: "triage_labels.new_status is required",
		},
		{
			name:    "active set with empty stale",
			triage:  TriageLabels{Active: []string{"label1"}, Stale: "", NewStatus: "New"},
			wantErr: "triage_labels.stale is required",
		},
		{
			name:    "active list contains empty string",
			triage:  TriageLabels{Active: []string{"label1", ""}, Stale: "stale", NewStatus: "New"},
			wantErr: "triage_labels.active must not contain empty strings",
		},
		{
			name:    "stale overlaps active label",
			triage:  TriageLabels{Active: []string{"label1", "label2"}, Stale: "label1", NewStatus: "New"},
			wantErr: "triage_labels.stale",
		},
		{
			name:    "stale overlaps active label case-insensitive",
			triage:  TriageLabels{Active: []string{"Label1"}, Stale: "label1", NewStatus: "New"},
			wantErr: "triage_labels.stale",
		},
		{
			name:    "fully configured is valid",
			triage:  TriageLabels{Active: []string{"label1", "label2"}, Stale: "stale", NewStatus: "New"},
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc := base()
			pc.TriageLabels = tt.triage
			err := pc.validate(0)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("validate() unexpected error: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("validate() error = %v, want error containing %q", err, tt.wantErr)
				}
			}
		})
	}
}

func TestFailureLabels_ExecutorOwnedCoversAllFields(t *testing.T) {
	fl := FailureLabels{
		CIFailing:       "ci",
		Rejected:        "rej",
		Blocked:         "blk",
		ForkUserMissing: "fork",
	}

	wantExecutorOwned := map[string]bool{"blk": true, "fork": true}
	wantScannerManaged := map[string]bool{"ci": true, "rej": true}

	got := make(map[string]bool, len(fl.ExecutorOwned()))
	for _, l := range fl.ExecutorOwned() {
		if got[l] {
			t.Errorf("ExecutorOwned() returned duplicate %q", l)
		}
		got[l] = true
	}
	if len(got) != len(wantExecutorOwned) {
		t.Fatalf("ExecutorOwned() = %v, want %v", fl.ExecutorOwned(), wantExecutorOwned)
	}
	for l := range wantExecutorOwned {
		if !got[l] {
			t.Errorf("ExecutorOwned() missing %q", l)
		}
	}

	wantAll := make(map[string]bool, len(wantExecutorOwned)+len(wantScannerManaged))
	for l := range wantExecutorOwned {
		wantAll[l] = true
	}
	for l := range wantScannerManaged {
		wantAll[l] = true
	}

	allSet := make(map[string]bool, len(fl.All()))
	for _, l := range fl.All() {
		if allSet[l] {
			t.Errorf("All() returned duplicate %q", l)
		}
		allSet[l] = true
	}
	if len(allSet) != len(wantAll) {
		t.Fatalf("All()=%d, want %d: update wantExecutorOwned or wantScannerManaged when adding a new FailureLabels field",
			len(allSet), len(wantAll))
	}
	for l := range wantAll {
		if !allSet[l] {
			t.Errorf("All() missing label %q", l)
		}
	}
}
