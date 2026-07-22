package services

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestGitOps_StripRemoteAuth(t *testing.T) {
	tmpDir := t.TempDir()

	// Initialize a git repo with a remote URL containing credentials.
	commands := [][]string{
		{"git", "init"},
		{"git", "remote", "add", "origin", "https://token@github.com/org/repo.git"},
	}
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup %v failed: %v\n%s", args, err, string(out))
		}
	}

	logger := zap.NewNop()
	gitOps := NewGitOps(exec.Command, logger)

	if err := gitOps.StripRemoteAuth(tmpDir); err != nil {
		t.Fatalf("StripRemoteAuth failed: %v", err)
	}

	// Verify credentials were removed.
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = tmpDir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("get-url failed: %v", err)
	}

	url := string(out)
	if url != "https://github.com/org/repo.git\n" {
		t.Errorf("expected stripped URL, got %q", url)
	}
}

func TestGitOps_ConfigureUser(t *testing.T) {
	tmpDir := t.TempDir()

	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, string(out))
	}

	logger := zap.NewNop()
	gitOps := NewGitOps(exec.Command, logger)

	if err := gitOps.ConfigureUser(tmpDir, "test-bot", "bot@test.com"); err != nil {
		t.Fatalf("ConfigureUser failed: %v", err)
	}

	// Verify.
	nameCmd := exec.Command("git", "config", "user.name")
	nameCmd.Dir = tmpDir
	nameOut, _ := nameCmd.Output()
	if string(nameOut) != "test-bot\n" {
		t.Errorf("expected user.name=test-bot, got %q", string(nameOut))
	}

	emailCmd := exec.Command("git", "config", "user.email")
	emailCmd.Dir = tmpDir
	emailOut, _ := emailCmd.Output()
	if string(emailOut) != "bot@test.com\n" {
		t.Errorf("expected user.email=bot@test.com, got %q", string(emailOut))
	}
}

func TestGitOps_ConfigureSSHSigning_NoKey(t *testing.T) {
	tmpDir := t.TempDir()

	cmd := exec.Command("git", "init")
	cmd.Dir = tmpDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, string(out))
	}

	logger := zap.NewNop()
	gitOps := NewGitOps(exec.Command, logger)

	// Empty path should be a no-op.
	if err := gitOps.ConfigureSSHSigning(tmpDir, ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Non-existent path should be a no-op (returns nil).
	if err := gitOps.ConfigureSSHSigning(tmpDir, "/nonexistent/key"); err != nil {
		t.Fatalf("unexpected error for nonexistent key: %v", err)
	}
}

func TestGitOps_CloneImport(t *testing.T) {
	// Create a bare repo to clone from.
	bareDir := t.TempDir()
	cmd := exec.Command("git", "init", "--bare")
	cmd.Dir = bareDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare failed: %v\n%s", err, string(out))
	}

	destDir := filepath.Join(t.TempDir(), "clone")

	logger := zap.NewNop()
	gitOps := NewGitOps(exec.Command, logger)

	if err := gitOps.CloneImport(bareDir, destDir, ""); err != nil {
		t.Fatalf("CloneImport failed: %v", err)
	}

	// Verify .git directory exists in destination.
	if _, err := os.Stat(filepath.Join(destDir, ".git")); err != nil {
		t.Errorf("expected .git directory in clone, got error: %v", err)
	}
}
