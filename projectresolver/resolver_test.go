package projectresolver_test

import (
	"strings"
	"testing"

	"jira-ai-issue-solver/models"
	"jira-ai-issue-solver/projectresolver"
)

// --- NewConfigResolver ---

func TestNewConfigResolver_NilConfig(t *testing.T) {
	_, err := projectresolver.NewConfigResolver(nil)
	if err == nil {
		t.Fatal("expected error for nil config")
	}
}

func TestNewConfigResolver_ValidConfig(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil resolver")
	}
}

// --- ResolveProject ---

func TestResolveProject_HappyPath(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-42",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" {
		t.Errorf("owner = %q, want %q", ps.Repos[0].Owner, "my-org")
	}
	if ps.Repos[0].Repo != "backend" {
		t.Errorf("repo = %q, want %q", ps.Repos[0].Repo, "backend")
	}
	if ps.Repos[0].CloneURL != "https://github.com/my-org/backend.git" {
		t.Errorf("clone URL = %q, want %q", ps.Repos[0].CloneURL, "https://github.com/my-org/backend.git")
	}
	if ps.Repos[0].BaseBranch != "main" {
		t.Errorf("base branch = %q, want %q", ps.Repos[0].BaseBranch, "main")
	}
	if ps.InProgressStatus != "In Progress" {
		t.Errorf("in-progress status = %q, want %q", ps.InProgressStatus, "In Progress")
	}
	if ps.InReviewStatus != "In Review" {
		t.Errorf("in-review status = %q, want %q", ps.InReviewStatus, "In Review")
	}
	if ps.TodoStatus != "To Do" {
		t.Errorf("todo status = %q, want %q", ps.TodoStatus, "To Do")
	}
	if ps.PRURLFieldName != "customfield_10100" {
		t.Errorf("PR field = %q, want %q", ps.PRURLFieldName, "customfield_10100")
	}
	if ps.DisableErrorComments {
		t.Error("disable error comments should be false")
	}
	if ps.AIProvider != "claude" {
		t.Errorf("AI provider = %q, want %q", ps.AIProvider, "claude")
	}
}

func TestResolveProject_MultipleComponents_FirstMatchUsed(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Workspaces["frontend"] = models.WorkspaceConfig{
		Repos: []models.RepoEntry{{Name: "frontend", URL: "https://github.com/my-org/frontend.git", Profile: "default"}},
	}
	cfg.Jira.Projects[0].Components["frontend"] = models.ComponentConfig{Workspace: "frontend"}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "backend" appears first and matches, so it should be used.
	wi := models.WorkItem{
		Key:        "PROJ-10",
		Type:       "Bug",
		Components: []string{"backend", "frontend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "backend" {
		t.Errorf("expected my-org/backend, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_MultipleComponents_SecondMatches(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Workspaces["frontend"] = models.WorkspaceConfig{
		Repos: []models.RepoEntry{{Name: "frontend", URL: "https://github.com/my-org/frontend", Profile: "default"}},
	}
	cfg.Jira.Projects[0].Components["frontend"] = models.ComponentConfig{Workspace: "frontend"}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// "unknown" does not match, but "frontend" does.
	wi := models.WorkItem{
		Key:        "PROJ-10",
		Type:       "Bug",
		Components: []string{"unknown", "frontend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "frontend" {
		t.Errorf("expected my-org/frontend, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_NoComponents_NoDefault(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-5",
		Type:       "Bug",
		Components: []string{},
	}

	_, err = r.ResolveProject(wi)
	if err == nil {
		t.Fatal("expected error for empty components with no default workspace")
	}
	assertContains(t, err.Error(), "no default_workspace")
}

func TestResolveProject_NoComponents_UsesDefaultWorkspace(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].DefaultWorkspace = "backend"

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-5",
		Type:       "Bug",
		Components: []string{},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "backend" {
		t.Errorf("expected my-org/backend from default workspace, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_UnmatchedComponents_UsesDefaultWorkspace(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].DefaultWorkspace = "backend"

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-5",
		Type:       "Bug",
		Components: []string{"nonexistent-component"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "backend" {
		t.Errorf("expected my-org/backend from default workspace, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_NoMatchingComponent_NoDefault(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-5",
		Type:       "Bug",
		Components: []string{"unknown-component"},
	}

	_, err = r.ResolveProject(wi)
	if err == nil {
		t.Fatal("expected error for non-matching component with no default workspace")
	}
	assertContains(t, err.Error(), "no component mapping found")
	assertContains(t, err.Error(), "no default_workspace")
}

func TestResolveProject_NoProjectConfig(t *testing.T) {
	// Config with no projects at all. GetProjectConfigForTicket returns nil.
	cfg := &models.Config{}
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "NOPE-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	_, err = r.ResolveProject(wi)
	if err == nil {
		t.Fatal("expected error for missing project config")
	}
	assertContains(t, err.Error(), "no project configuration")
}

func TestResolveProject_FallbackProject(t *testing.T) {
	// GetProjectConfigForTicket falls back to the first project when
	// no project key matches. Verify the resolver handles this.
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "OTHER-99",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Falls back to the first (and only) project config.
	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "backend" {
		t.Errorf("expected my-org/backend from fallback, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_URLWithGitSuffix(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Repo != "backend" {
		t.Errorf("repo = %q, want %q (should strip .git)", ps.Repos[0].Repo, "backend")
	}
}

func TestResolveProject_URLWithoutGitSuffix(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Workspaces["backend"] = models.WorkspaceConfig{
		Repos: []models.RepoEntry{{Name: "backend", URL: "https://github.com/my-org/backend", Profile: "default"}},
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "backend" {
		t.Errorf("expected my-org/backend, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_URLWithTrailingSlash(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Workspaces["backend"] = models.WorkspaceConfig{
		Repos: []models.RepoEntry{{Name: "backend", URL: "https://github.com/my-org/backend/", Profile: "default"}},
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Owner != "my-org" || ps.Repos[0].Repo != "backend" {
		t.Errorf("expected my-org/backend, got %s/%s", ps.Repos[0].Owner, ps.Repos[0].Repo)
	}
}

func TestResolveProject_StatusTransitions_DifferentTypes(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].StatusTransitions["Story"] = models.StatusTransitions{
		Todo:       "Backlog",
		InProgress: "Working",
		InReview:   "Reviewing",
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		ticketType string
		wantTodo   string
		wantInProg string
		wantReview string
	}{
		{"Bug", "To Do", "In Progress", "In Review"},
		{"Story", "Backlog", "Working", "Reviewing"},
	}

	for _, tt := range tests {
		t.Run(tt.ticketType, func(t *testing.T) {
			wi := models.WorkItem{
				Key:        "PROJ-1",
				Type:       tt.ticketType,
				Components: []string{"backend"},
			}

			ps, err := r.ResolveProject(wi)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if ps.TodoStatus != tt.wantTodo {
				t.Errorf("todo = %q, want %q", ps.TodoStatus, tt.wantTodo)
			}
			if ps.InProgressStatus != tt.wantInProg {
				t.Errorf("in_progress = %q, want %q", ps.InProgressStatus, tt.wantInProg)
			}
			if ps.InReviewStatus != tt.wantReview {
				t.Errorf("in_review = %q, want %q", ps.InReviewStatus, tt.wantReview)
			}
		})
	}
}

func TestResolveProject_DisableErrorComments(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].DisableErrorComments = true

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !ps.DisableErrorComments {
		t.Error("expected DisableErrorComments to be true")
	}
}

// --- LocateRepo ---

func TestLocateRepo_HappyPath(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-42",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	owner, repo, err := r.LocateRepo(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if owner != "my-org" {
		t.Errorf("owner = %q, want %q", owner, "my-org")
	}
	if repo != "backend" {
		t.Errorf("repo = %q, want %q", repo, "backend")
	}
}

func TestLocateRepo_NoMatchingComponent(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-5",
		Type:       "Bug",
		Components: []string{"nonexistent"},
	}

	_, _, err = r.LocateRepo(wi)
	if err == nil {
		t.Fatal("expected error for non-matching component")
	}
}

func TestLocateRepo_NoComponents_NoDefault(t *testing.T) {
	cfg := minimalConfig()
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-5",
		Type:       "Bug",
		Components: []string{},
	}

	_, _, err = r.LocateRepo(wi)
	if err == nil {
		t.Fatal("expected error for empty components with no default workspace")
	}
}

func TestLocateRepo_URLParsing(t *testing.T) {
	tests := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
	}{
		{
			name:      "with .git suffix",
			url:       "https://github.com/org/repo.git",
			wantOwner: "org",
			wantRepo:  "repo",
		},
		{
			name:      "without .git suffix",
			url:       "https://github.com/org/repo",
			wantOwner: "org",
			wantRepo:  "repo",
		},
		{
			name:      "with trailing slash",
			url:       "https://github.com/org/repo/",
			wantOwner: "org",
			wantRepo:  "repo",
		},
		{
			name:      "with .git and trailing slash",
			url:       "https://github.com/org/repo.git/",
			wantOwner: "org",
			wantRepo:  "repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.Jira.Projects[0].Workspaces["backend"] = models.WorkspaceConfig{
				Repos: []models.RepoEntry{{Name: "backend", URL: tt.url, Profile: "default"}},
			}

			r, err := projectresolver.NewConfigResolver(cfg)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			wi := models.WorkItem{
				Key:        "PROJ-1",
				Type:       "Bug",
				Components: []string{"backend"},
			}

			owner, repo, err := r.LocateRepo(wi)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if owner != tt.wantOwner {
				t.Errorf("owner = %q, want %q", owner, tt.wantOwner)
			}
			if repo != tt.wantRepo {
				t.Errorf("repo = %q, want %q", repo, tt.wantRepo)
			}
		})
	}
}

// --- Case-insensitive component matching ---

func TestResolveProject_ComponentMatchingCaseInsensitive(t *testing.T) {
	// Viper lowercases YAML map keys, so "FlightCtl" in YAML becomes
	// "flightctl" in the loaded config. The component from Jira retains
	// original casing ("FlightCtl-Core"). Case-insensitive matching
	// bridges this gap.
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Workspaces["flightctl"] = models.WorkspaceConfig{
		Repos: []models.RepoEntry{{Name: "flightctl", URL: "https://github.com/org/flightctl.git", Profile: "default"}},
	}
	cfg.Jira.Projects[0].Components["flightctl"] = models.ComponentConfig{Workspace: "flightctl"}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"FlightCtl"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Repo != "flightctl" {
		t.Errorf("repo = %q, want %q", ps.Repos[0].Repo, "flightctl")
	}
}

func TestResolveProject_ComponentMatchingExactTakesPriority(t *testing.T) {
	cfg := minimalConfig()
	// Exact match for "Backend" and case-insensitive match for "backend"
	// should both exist. Exact match should win.
	cfg.Jira.Projects[0].Workspaces["exact"] = models.WorkspaceConfig{
		Repos: []models.RepoEntry{{Name: "exact", URL: "https://github.com/org/exact.git", Profile: "default"}},
	}
	cfg.Jira.Projects[0].Components["Backend"] = models.ComponentConfig{Workspace: "exact"}
	// "backend" already exists from minimalConfig

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"Backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Repo != "exact" {
		t.Errorf("repo = %q, want %q (exact match should take priority)", ps.Repos[0].Repo, "exact")
	}
}

func TestResolveProject_ComponentMatchingUppercase(t *testing.T) {
	cfg := minimalConfig()
	// Config has lowercase "backend", Jira component is "BACKEND"
	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"BACKEND"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Repo != "backend" {
		t.Errorf("repo = %q, want %q", ps.Repos[0].Repo, "backend")
	}
}

// --- Container settings passthrough ---

func TestResolveProject_ContainerSettingsPassedThrough(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Profiles["default"] = models.Profile{
		Container: models.ContainerSettings{
			Image: "custom-image:latest",
			ResourceLimits: models.ContainerResourceLimits{
				Memory: "16g",
				CPUs:   "8",
			},
		},
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	ps, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ps.Repos[0].Container.Image != "custom-image:latest" {
		t.Errorf("container image = %q, want %q", ps.Repos[0].Container.Image, "custom-image:latest")
	}
	if ps.Repos[0].Container.ResourceLimits.Memory != "16g" {
		t.Errorf("container memory = %q, want %q", ps.Repos[0].Container.ResourceLimits.Memory, "16g")
	}
	if ps.Repos[0].Container.ResourceLimits.CPUs != "8" {
		t.Errorf("container cpus = %q, want %q", ps.Repos[0].Container.ResourceLimits.CPUs, "8")
	}
}

// --- ForkOwner ---

func TestConfigResolver_ForkOwner(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].ForkMode = true
	cfg.Jira.AssigneeToGitHubUsername = map[string]string{
		"alice@example.com": "alice-gh",
		"bob@example.com":   "bob-gh",
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tests := []struct {
		name     string
		workItem models.WorkItem
		want     string
	}{
		{
			name: "assignee in mapping",
			workItem: models.WorkItem{
				Key:        "PROJ-1",
				Type:       "Bug",
				Components: []string{"backend"},
				Assignee:   &models.Author{Email: "alice@example.com"},
			},
			want: "alice-gh",
		},
		{
			name: "assignee not in mapping",
			workItem: models.WorkItem{
				Key:        "PROJ-2",
				Type:       "Bug",
				Components: []string{"backend"},
				Assignee:   &models.Author{Email: "charlie@example.com"},
			},
			want: "",
		},
		{
			name: "no assignee",
			workItem: models.WorkItem{
				Key:        "PROJ-3",
				Type:       "Bug",
				Components: []string{"backend"},
				Assignee:   nil,
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.ForkOwner(tt.workItem)
			if got != tt.want {
				t.Errorf("ForkOwner() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigResolver_ForkOwnerHeads(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].ForkMode = true
	cfg.Jira.AssigneeToGitHubUsername = map[string]string{
		"alice@example.com": "alice-gh",
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	t.Run("fork mode with mapped assignee returns two heads", func(t *testing.T) {
		got := r.ForkOwnerHeads(models.WorkItem{
			Key:      "PROJ-1",
			Type:     "Bug",
			Assignee: &models.Author{Email: "alice@example.com"},
		}, "bot/PROJ-1")

		want := []string{"alice-gh:bot/PROJ-1", "bot/PROJ-1"}
		if len(got) != len(want) {
			t.Fatalf("ForkOwnerHeads() = %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("ForkOwnerHeads()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("fork mode with unmapped assignee returns one head", func(t *testing.T) {
		got := r.ForkOwnerHeads(models.WorkItem{
			Key:      "PROJ-2",
			Type:     "Bug",
			Assignee: &models.Author{Email: "charlie@example.com"},
		}, "bot/PROJ-2")

		if len(got) != 1 || got[0] != "bot/PROJ-2" {
			t.Errorf("ForkOwnerHeads() = %v, want [bot/PROJ-2]", got)
		}
	})

	t.Run("direct mode returns one head", func(t *testing.T) {
		directCfg := minimalConfig()
		dr, err := projectresolver.NewConfigResolver(directCfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		got := dr.ForkOwnerHeads(models.WorkItem{
			Key:      "PROJ-1",
			Type:     "Bug",
			Assignee: &models.Author{Email: "alice@example.com"},
		}, "bot/PROJ-1")

		if len(got) != 1 || got[0] != "bot/PROJ-1" {
			t.Errorf("ForkOwnerHeads() = %v, want [bot/PROJ-1]", got)
		}
	})
}

// --- Imports propagation ---

func TestResolveProject_PropagatesImports(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Profiles["default"] = models.Profile{
		Imports: []models.ImportConfig{
			{Repo: "https://github.com/org/workflows", Path: ".ai-workflows", Ref: "main"},
			{Repo: "https://github.com/org/tools", Path: ".tools"},
		},
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}
	settings, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(settings.Repos[0].Imports) != 2 {
		t.Fatalf("len(Imports) = %d, want 2", len(settings.Repos[0].Imports))
	}
	if settings.Repos[0].Imports[0].Repo != "https://github.com/org/workflows" {
		t.Errorf("Imports[0].Repo = %q, want workflows URL", settings.Repos[0].Imports[0].Repo)
	}
	if settings.Repos[0].Imports[0].Ref != "main" {
		t.Errorf("Imports[0].Ref = %q, want %q", settings.Repos[0].Imports[0].Ref, "main")
	}
	if settings.Repos[0].Imports[1].Path != ".tools" {
		t.Errorf("Imports[1].Path = %q, want %q", settings.Repos[0].Imports[1].Path, ".tools")
	}
}

func TestResolveProject_NoImports_EmptySlice(t *testing.T) {
	cfg := minimalConfig()

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}
	settings, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if settings.Repos[0].Imports == nil {
		t.Error("Imports should be non-nil empty slice")
	}
	if len(settings.Repos[0].Imports) != 0 {
		t.Errorf("len(Imports) = %d, want 0", len(settings.Repos[0].Imports))
	}
}

// --- RootRepo propagation ---

func TestResolveProject_PropagatesRootRepoURL(t *testing.T) {
	cfg := minimalConfig()
	cfg.Jira.Projects[0].Workspaces["backend"] = models.WorkspaceConfig{
		RootRepo: "https://github.com/osac-project/osac-workspace",
		Repos: []models.RepoEntry{
			{Name: "backend", URL: "https://github.com/my-org/backend.git", Profile: "default"},
		},
	}

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}
	settings, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if settings.RootRepoURL != "https://github.com/osac-project/osac-workspace" {
		t.Errorf("RootRepoURL = %q, want osac-workspace URL", settings.RootRepoURL)
	}
}

func TestResolveProject_EmptyRootRepoURL(t *testing.T) {
	cfg := minimalConfig()

	r, err := projectresolver.NewConfigResolver(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}
	settings, err := r.ResolveProject(wi)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if settings.RootRepoURL != "" {
		t.Errorf("RootRepoURL = %q, want empty string", settings.RootRepoURL)
	}
}

// --- helpers ---

// minimalConfig returns a Config with a single project, one workspace
// with one repo, one component mapping, and Bug status transitions --
// enough for most tests.
func minimalConfig() *models.Config {
	cfg := &models.Config{
		AIProvider: "claude",
		Jira: models.JiraConfig{
			Projects: []models.ProjectConfig{
				{
					ProjectKeys: models.ProjectKeys{"PROJ"},
					StatusTransitions: models.TicketTypeStatusTransitions{
						"Bug": models.StatusTransitions{
							Todo:       "To Do",
							InProgress: "In Progress",
							InReview:   "In Review",
						},
					},
					GitPullRequestFieldName: "customfield_10100",
					Workspaces: map[string]models.WorkspaceConfig{
						"backend": {
							Repos: []models.RepoEntry{
								{Name: "backend", URL: "https://github.com/my-org/backend.git", Profile: "default"},
							},
						},
					},
					Components: models.ComponentMap{
						"backend": models.ComponentConfig{Workspace: "backend"},
					},
					Profiles: map[string]models.Profile{
						"default": {},
					},
				},
			},
		},
	}
	return cfg
}

func TestResolveProject_FailureLabels(t *testing.T) {
	t.Run("passes through configured labels", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].FailureLabels = models.FailureLabels{
			CIFailing: "ci-fail",
			Rejected:  "rejected",
			Blocked:   "blocked",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.FailureLabels.CIFailing != "ci-fail" {
			t.Errorf("CIFailing = %q, want %q", ps.FailureLabels.CIFailing, "ci-fail")
		}
		if ps.FailureLabels.Rejected != "rejected" {
			t.Errorf("Rejected = %q, want %q", ps.FailureLabels.Rejected, "rejected")
		}
		if ps.FailureLabels.Blocked != "blocked" {
			t.Errorf("Blocked = %q, want %q", ps.FailureLabels.Blocked, "blocked")
		}
	})

	t.Run("defaults to empty when not configured", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.FailureLabels != (models.FailureLabels{}) {
			t.Errorf("expected zero FailureLabels, got %+v", ps.FailureLabels)
		}
	})
}

func TestResolveProject_PRValidationLabels(t *testing.T) {
	t.Run("passes through configured labels", func(t *testing.T) {
		customCCE := "custom-cce"
		cfg := minimalConfig()
		cfg.Jira.Projects[0].PRValidationLabels = models.PRValidationLabels{
			ValidationFailed: "custom-vf",
			NonzeroExit:      "custom-nze",
			CostCapExceeded:  &customCCE,
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.PRValidationLabels.ValidationFailed != "custom-vf" {
			t.Errorf("ValidationFailed = %q, want %q", ps.PRValidationLabels.ValidationFailed, "custom-vf")
		}
		if ps.PRValidationLabels.NonzeroExit != "custom-nze" {
			t.Errorf("NonzeroExit = %q, want %q", ps.PRValidationLabels.NonzeroExit, "custom-nze")
		}
		if ps.PRValidationLabels.CostCapLabel() != "custom-cce" {
			t.Errorf("CostCapLabel() = %q, want %q", ps.PRValidationLabels.CostCapLabel(), "custom-cce")
		}
	})

	t.Run("defaults cost_cap_exceeded when not configured", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.PRValidationLabels.ValidationFailed != "" {
			t.Errorf("ValidationFailed = %q, want empty", ps.PRValidationLabels.ValidationFailed)
		}
		if ps.PRValidationLabels.NonzeroExit != "" {
			t.Errorf("NonzeroExit = %q, want empty", ps.PRValidationLabels.NonzeroExit)
		}
		if ps.PRValidationLabels.CostCapLabel() != "ai-budget-exceeded" {
			t.Errorf("CostCapLabel() = %q, want %q", ps.PRValidationLabels.CostCapLabel(), "ai-budget-exceeded")
		}
	})

	t.Run("explicit empty disables cost_cap_exceeded", func(t *testing.T) {
		empty := ""
		cfg := minimalConfig()
		cfg.Jira.Projects[0].PRValidationLabels = models.PRValidationLabels{
			CostCapExceeded: &empty,
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.PRValidationLabels.CostCapLabel() != "" {
			t.Errorf("CostCapLabel() = %q, want empty (disabled)", ps.PRValidationLabels.CostCapLabel())
		}
	})
}

func TestResolveFailureLabels(t *testing.T) {
	t.Run("returns labels for known project", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].FailureLabels = models.FailureLabels{
			CIFailing: "ci-label",
			Blocked:   "blocked-label",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		fl := r.ResolveFailureLabels(models.WorkItem{Key: "PROJ-1", Type: "Bug"})
		if fl.CIFailing != "ci-label" {
			t.Errorf("CIFailing = %q, want %q", fl.CIFailing, "ci-label")
		}
		if fl.Blocked != "blocked-label" {
			t.Errorf("Blocked = %q, want %q", fl.Blocked, "blocked-label")
		}
	})

	t.Run("returns zero value when project has no labels", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		fl := r.ResolveFailureLabels(models.WorkItem{Key: "PROJ-1", Type: "Bug"})
		if fl != (models.FailureLabels{}) {
			t.Errorf("expected zero FailureLabels, got %+v", fl)
		}
	})
}

func TestResolveProject_LifecycleLabels(t *testing.T) {
	t.Run("passes through configured labels", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].LifecycleLabels = models.LifecycleLabels{
			Queued: "jira-autofix",
			Review: "jira-autofix-review",
			Merged: "jira-autofix-merged",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.LifecycleLabels.Queued != "jira-autofix" {
			t.Errorf("Queued = %q, want %q", ps.LifecycleLabels.Queued, "jira-autofix")
		}
		if ps.LifecycleLabels.Review != "jira-autofix-review" {
			t.Errorf("Review = %q, want %q", ps.LifecycleLabels.Review, "jira-autofix-review")
		}
		if ps.LifecycleLabels.Merged != "jira-autofix-merged" {
			t.Errorf("Merged = %q, want %q", ps.LifecycleLabels.Merged, "jira-autofix-merged")
		}
	})

	t.Run("defaults to empty when not configured", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.LifecycleLabels != (models.LifecycleLabels{}) {
			t.Errorf("expected zero LifecycleLabels, got %+v", ps.LifecycleLabels)
		}
	})
}

func TestResolveProject_MergedStatus(t *testing.T) {
	t.Run("passes through configured merged status", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].StatusTransitions["Bug"] = models.StatusTransitions{
			Todo:       "To Do",
			InProgress: "In Progress",
			InReview:   "In Review",
			Merged:     "MODIFIED",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.MergedStatus != "MODIFIED" {
			t.Errorf("MergedStatus = %q, want %q", ps.MergedStatus, "MODIFIED")
		}
	})

	t.Run("defaults to empty when not configured", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.MergedStatus != "" {
			t.Errorf("MergedStatus = %q, want empty", ps.MergedStatus)
		}
	})
}

func TestResolveLifecycleLabels(t *testing.T) {
	t.Run("returns labels for known project", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].LifecycleLabels = models.LifecycleLabels{
			Queued: "queued-label",
			Review: "review-label",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ll := r.ResolveLifecycleLabels(models.WorkItem{Key: "PROJ-1", Type: "Bug"})
		if ll.Queued != "queued-label" {
			t.Errorf("Queued = %q, want %q", ll.Queued, "queued-label")
		}
		if ll.Review != "review-label" {
			t.Errorf("Review = %q, want %q", ll.Review, "review-label")
		}
	})

	t.Run("returns zero value for unknown project", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ll := r.ResolveLifecycleLabels(models.WorkItem{Key: "UNKNOWN-1", Type: "Bug"})
		if ll != (models.LifecycleLabels{}) {
			t.Errorf("expected zero LifecycleLabels, got %+v", ll)
		}
	})
}

func TestResolveMergedStatus(t *testing.T) {
	t.Run("returns merged status for known ticket type", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].StatusTransitions["Bug"] = models.StatusTransitions{
			Todo:       "To Do",
			InProgress: "In Progress",
			InReview:   "In Review",
			Merged:     "MODIFIED",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		status := r.ResolveMergedStatus(models.WorkItem{Key: "PROJ-1", Type: "Bug"})
		if status != "MODIFIED" {
			t.Errorf("MergedStatus = %q, want %q", status, "MODIFIED")
		}
	})

	t.Run("returns empty for unconfigured merged status", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		status := r.ResolveMergedStatus(models.WorkItem{Key: "PROJ-1", Type: "Bug"})
		if status != "" {
			t.Errorf("MergedStatus = %q, want empty", status)
		}
	})

	t.Run("returns empty for unknown project", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		status := r.ResolveMergedStatus(models.WorkItem{Key: "UNKNOWN-1", Type: "Bug"})
		if status != "" {
			t.Errorf("MergedStatus = %q, want empty", status)
		}
	})
}

func TestResolveProject_ForkMode(t *testing.T) {
	t.Run("passes through fork_mode true", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].ForkMode = true
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !ps.ForkMode {
			t.Error("ForkMode = false, want true")
		}
	})

	t.Run("defaults to false when not configured", func(t *testing.T) {
		cfg := minimalConfig()
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.ForkMode {
			t.Error("ForkMode = true, want false")
		}
	})

	t.Run("populates GitHubUsername only when fork_mode true", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.Projects[0].ForkMode = true
		cfg.Jira.AssigneeToGitHubUsername = map[string]string{
			"alice@example.com": "alice-gh",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
			Assignee:   &models.Author{Email: "alice@example.com"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if !ps.ForkMode {
			t.Error("ForkMode = false, want true")
		}
		if ps.GitHubUsername != "alice-gh" {
			t.Errorf("GitHubUsername = %q, want %q", ps.GitHubUsername, "alice-gh")
		}
	})

	t.Run("does not populate GitHubUsername when fork_mode false", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Jira.AssigneeToGitHubUsername = map[string]string{
			"alice@example.com": "alice-gh",
		}
		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(models.WorkItem{
			Key:        "PROJ-1",
			Type:       "Bug",
			Components: []string{"backend"},
			Assignee:   &models.Author{Email: "alice@example.com"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.ForkMode {
			t.Error("ForkMode = true, want false")
		}
		if ps.GitHubUsername != "" {
			t.Errorf("GitHubUsername = %q, want empty (fork_mode is false)", ps.GitHubUsername)
		}
	})
}

func TestResolveProject_MaxTicketCostUSD(t *testing.T) {
	wi := models.WorkItem{
		Key:        "PROJ-1",
		Type:       "Bug",
		Components: []string{"backend"},
	}

	t.Run("uses global default when project does not override", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Guardrails.MaxTicketCostUSD = 20.0

		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(wi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.MaxTicketCostUSD != 20.0 {
			t.Errorf("MaxTicketCostUSD = %v, want 20.0", ps.MaxTicketCostUSD)
		}
	})

	t.Run("per-project override takes precedence", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Guardrails.MaxTicketCostUSD = 20.0
		override := 50.0
		cfg.Jira.Projects[0].MaxTicketCostUSD = &override

		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(wi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.MaxTicketCostUSD != 50.0 {
			t.Errorf("MaxTicketCostUSD = %v, want 50.0", ps.MaxTicketCostUSD)
		}
	})

	t.Run("per-project negative disables cap", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Guardrails.MaxTicketCostUSD = 20.0
		disabled := -1.0
		cfg.Jira.Projects[0].MaxTicketCostUSD = &disabled

		r, err := projectresolver.NewConfigResolver(cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		ps, err := r.ResolveProject(wi)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if ps.MaxTicketCostUSD != -1.0 {
			t.Errorf("MaxTicketCostUSD = %v, want -1.0 (disabled)", ps.MaxTicketCostUSD)
		}
	})
}

// assertContains is a test helper that fails if s does not contain substr.
func assertContains(t *testing.T, s, substr string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("expected %q to contain %q", s, substr)
	}
}
