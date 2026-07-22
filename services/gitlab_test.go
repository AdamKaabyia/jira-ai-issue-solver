package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/models"
)

func newTestGitLabConfig(baseURL string) *models.Config {
	return &models.Config{
		GitLab: struct {
			BaseURL           string   `yaml:"base_url" mapstructure:"base_url"`
			AccessToken       string   `yaml:"access_token" mapstructure:"access_token"`
			BotUsername       string   `yaml:"bot_username" mapstructure:"bot_username"`
			BotEmail          string   `yaml:"bot_email" mapstructure:"bot_email"`
			MRLabel           string   `yaml:"mr_label" mapstructure:"mr_label"`
			SSHKeyPath        string   `yaml:"ssh_key_path" mapstructure:"ssh_key_path"`
			SkipMRLabel       string   `yaml:"skip_mr_label" mapstructure:"skip_mr_label"`
			MaxThreadDepth    int      `yaml:"max_thread_depth" mapstructure:"max_thread_depth"`
			KnownBotUsernames []string `yaml:"known_bot_usernames" mapstructure:"known_bot_usernames"`
			IgnoredUsernames  []string `yaml:"ignored_usernames" mapstructure:"ignored_usernames"`
		}{
			BaseURL:     baseURL,
			AccessToken: "test-token",
			BotUsername: "ai-bot",
			BotEmail:    "ai-bot@example.com",
			MRLabel:     "ai-mr",
		},
	}
}

func TestGitLabCreatePR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("PRIVATE-TOKEN") != "test-token" {
			t.Errorf("expected auth token, got %s", r.Header.Get("PRIVATE-TOKEN"))
		}

		w.WriteHeader(http.StatusCreated)
		resp := map[string]interface{}{
			"iid":     42,
			"web_url": "https://gitlab.example.com/org/repo/-/merge_requests/42",
			"state":   "opened",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	pr, err := svc.CreatePR(models.PRParams{
		Owner:  "org",
		Repo:   "repo",
		Head:   "feature-branch",
		Base:   "main",
		Title:  "Test MR",
		Body:   "Test body",
		Labels: []string{"ai-mr"},
	})
	if err != nil {
		t.Fatalf("CreatePR failed: %v", err)
	}
	if pr.Number != 42 {
		t.Errorf("expected MR number 42, got %d", pr.Number)
	}
	if pr.URL != "https://gitlab.example.com/org/repo/-/merge_requests/42" {
		t.Errorf("unexpected URL: %s", pr.URL)
	}
}

func TestGitLabGetPRForBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}

		resp := []map[string]interface{}{
			{
				"iid":           10,
				"title":         "Feature branch MR",
				"source_branch": "feature",
				"target_branch": "main",
				"web_url":       "https://gitlab.example.com/org/repo/-/merge_requests/10",
				"sha":           "abc123",
				"created_at":    "2026-01-15T10:00:00Z",
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	details, err := svc.GetPRForBranch("org", "repo", "feature")
	if err != nil {
		t.Fatalf("GetPRForBranch failed: %v", err)
	}
	if details == nil {
		t.Fatal("expected non-nil PRDetails")
	}
	if details.Number != 10 {
		t.Errorf("expected MR IID 10, got %d", details.Number)
	}
	if details.Branch != "feature" {
		t.Errorf("expected source branch 'feature', got %s", details.Branch)
	}
}

func TestGitLabGetPRForBranch_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]interface{}{})
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	details, err := svc.GetPRForBranch("org", "repo", "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if details != nil {
		t.Error("expected nil for non-existent branch")
	}
}

func TestGitLabGetPRComments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := []map[string]interface{}{
			{
				"id":         int64(1),
				"body":       "Review comment",
				"author":     map[string]string{"username": "reviewer"},
				"created_at": "2026-01-15T10:00:00Z",
				"system":     false,
				"resolvable": true,
				"position":   map[string]interface{}{"new_path": "main.go", "new_line": 42},
			},
			{
				"id":         int64(2),
				"body":       "System note",
				"author":     map[string]string{"username": "system"},
				"created_at": "2026-01-15T11:00:00Z",
				"system":     true,
				"resolvable": false,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	comments, err := svc.GetPRComments("org", "repo", 10, time.Time{})
	if err != nil {
		t.Fatalf("GetPRComments failed: %v", err)
	}
	if len(comments) != 1 {
		t.Fatalf("expected 1 non-system comment, got %d", len(comments))
	}
	if comments[0].Body != "Review comment" {
		t.Errorf("unexpected body: %s", comments[0].Body)
	}
	if comments[0].FilePath != "main.go" {
		t.Errorf("unexpected file path: %s", comments[0].FilePath)
	}
	if comments[0].Line != 42 {
		t.Errorf("expected line 42, got %d", comments[0].Line)
	}
}

func TestGitLabListCheckRunsForRef(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount == 1 {
			resp := []map[string]interface{}{
				{"id": 100, "status": "failed"},
			}
			json.NewEncoder(w).Encode(resp)
		} else {
			resp := []map[string]interface{}{
				{"id": 200, "name": "test-job"},
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	failures, allComplete, err := svc.ListCheckRunsForRef("org", "repo", "abc123")
	if err != nil {
		t.Fatalf("ListCheckRunsForRef failed: %v", err)
	}
	if !allComplete {
		t.Error("expected allComplete=true for 'failed' status")
	}
	if len(failures) != 1 {
		t.Fatalf("expected 1 failure, got %d", len(failures))
	}
	if failures[0].Name != "test-job" {
		t.Errorf("unexpected job name: %s", failures[0].Name)
	}
}

func TestGitLabAddRemovePRLabel(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Method == "GET" {
			resp := map[string]interface{}{
				"labels": []string{"existing-label"},
			}
			json.NewEncoder(w).Encode(resp)
		} else if r.Method == "PUT" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{})
		}
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	err := svc.AddPRLabel("org", "repo", 1, "new-label")
	if err != nil {
		t.Fatalf("AddPRLabel failed: %v", err)
	}

	err = svc.RemovePRLabel("org", "repo", 1, "existing-label")
	if err != nil {
		t.Fatalf("RemovePRLabel failed: %v", err)
	}
}

func TestGitLabRemoteBranchExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"name": "feature"})
		}
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	exists, err := svc.RemoteBranchExists("org", "repo", "feature")
	if err != nil {
		t.Fatalf("RemoteBranchExists failed: %v", err)
	}
	if !exists {
		t.Error("expected branch to exist")
	}
}

func TestGitLabRemoteBranchNotExists(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	config := newTestGitLabConfig(server.URL)
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger)

	exists, err := svc.RemoteBranchExists("org", "repo", "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected branch to not exist")
	}
}

func TestGitLabCloneRepository(t *testing.T) {
	tmpDir := t.TempDir()
	repoDir := filepath.Join(tmpDir, "repo")

	// Create a fake git repo.
	os.MkdirAll(filepath.Join(repoDir, ".git"), 0750)

	fakeExec := func(name string, args ...string) *exec.Cmd {
		return exec.Command("true")
	}

	config := newTestGitLabConfig("https://gitlab.example.com")
	logger := zap.NewNop()
	svc := NewGitLabService(config, logger, fakeExec)

	err := svc.CloneRepository("https://gitlab.example.com/org/repo.git", repoDir)
	if err != nil {
		t.Fatalf("CloneRepository failed: %v", err)
	}
}

func TestExtractGitLabRepoInfo(t *testing.T) {
	tests := []struct {
		url       string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"https://gitlab.com/org/repo.git", "org", "repo", false},
		{"https://gitlab.cee.redhat.com/group/subgroup/repo.git", "group/subgroup", "repo", false},
		{"https://gitlab.com/a/b/c/d.git", "a/b/c", "d", false},
		{"invalid", "", "", true},
	}

	for _, tt := range tests {
		owner, repo, err := extractGitLabRepoInfo(tt.url)
		if (err != nil) != tt.wantErr {
			t.Errorf("extractGitLabRepoInfo(%s): err=%v, wantErr=%v", tt.url, err, tt.wantErr)
			continue
		}
		if owner != tt.wantOwner {
			t.Errorf("extractGitLabRepoInfo(%s): owner=%q, want %q", tt.url, owner, tt.wantOwner)
		}
		if repo != tt.wantRepo {
			t.Errorf("extractGitLabRepoInfo(%s): repo=%q, want %q", tt.url, repo, tt.wantRepo)
		}
	}
}
