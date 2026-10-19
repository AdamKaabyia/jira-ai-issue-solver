package services

import "testing"

func TestIsExcludedPath(t *testing.T) {
	excludes := mergeExcludes([]string{".artifacts/"})

	tests := []struct {
		path string
		want bool
	}{
		// Built-in .ai-session prefix exclusion
		{".ai-session", true},
		{".ai-session/", true},
		{".ai-session/task.md", true},
		{".ai-session/session-output.json", true},
		{".ai-session.preserve/task.md", true},

		// Import-declared .artifacts exclusion
		{".artifacts", true},
		{".artifacts/", true},
		{".artifacts/bugfix/diagnosis.md", true},

		// Built-in .ai-bot prefix exclusion
		{".ai-bot", true},
		{".ai-bot/", true},
		{".ai-bot/config.yaml", true},
		{".ai-bot/instructions.md", true},
		{".ai-bot.preserve/config.yaml", true},

		// Not excluded
		{"src/main.go", false},
		{"README.md", false},
		{".github/workflows/ci.yaml", false},
		{"ai-session/file.txt", false}, // missing leading dot
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isExcludedPath(tt.path, excludes)
			if got != tt.want {
				t.Errorf("isExcludedPath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestIsExcludedPath_NoImportExcludes(t *testing.T) {
	excludes := mergeExcludes(nil)

	// .ai-session is always excluded (builtin)
	if !isExcludedPath(".ai-session/task.md", excludes) {
		t.Error("expected .ai-session/task.md to be excluded even with no import excludes")
	}

	// .ai-bot is always excluded (builtin)
	if !isExcludedPath(".ai-bot/config.yaml", excludes) {
		t.Error("expected .ai-bot/config.yaml to be excluded even with no import excludes")
	}

	// .artifacts is NOT excluded when not declared by imports
	if isExcludedPath(".artifacts/diagnosis.md", excludes) {
		t.Error("expected .artifacts to NOT be excluded when not in import excludes")
	}
}

func TestMergeExcludes_NormalizesTrailingSlash(t *testing.T) {
	excludes := mergeExcludes([]string{".artifacts", "output/"})

	// Both should work regardless of trailing slash in input
	if !isExcludedPath(".artifacts/file.md", excludes) {
		t.Error("expected .artifacts/file.md to be excluded")
	}
	if !isExcludedPath("output/results.json", excludes) {
		t.Error("expected output/results.json to be excluded")
	}
}
