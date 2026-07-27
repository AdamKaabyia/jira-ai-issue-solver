package container_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"jira-ai-issue-solver/container"
	"jira-ai-issue-solver/container/containertest"
)

// --- NewRuntimeManager ---

func TestNewRuntimeManager_RejectsNilRunner(t *testing.T) {
	resolver := mustResolver(t)
	cfg := container.RuntimeManagerConfig{NamePrefix: "test"}

	_, err := container.NewRuntimeManager(nil, resolver, cfg, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for nil runner")
	}
}

func TestNewRuntimeManager_RejectsNilResolver(t *testing.T) {
	runner := &containertest.StubRunner{}
	cfg := container.RuntimeManagerConfig{NamePrefix: "test"}

	_, err := container.NewRuntimeManager(runner, nil, cfg, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for nil resolver")
	}
}

func TestNewRuntimeManager_RejectsNilLogger(t *testing.T) {
	runner := &containertest.StubRunner{}
	resolver := mustResolver(t)
	cfg := container.RuntimeManagerConfig{NamePrefix: "test"}

	_, err := container.NewRuntimeManager(runner, resolver, cfg, nil)
	if err == nil {
		t.Fatal("expected error for nil logger")
	}
}

func TestNewRuntimeManager_RejectsEmptyPrefix(t *testing.T) {
	runner := &containertest.StubRunner{}
	resolver := mustResolver(t)
	cfg := container.RuntimeManagerConfig{NamePrefix: ""}

	_, err := container.NewRuntimeManager(runner, resolver, cfg, zap.NewNop())
	if err == nil {
		t.Fatal("expected error for empty prefix")
	}
}

func TestNewRuntimeManager_ValidConfig(t *testing.T) {
	runner := &containertest.StubRunner{}
	resolver := mustResolver(t)
	cfg := container.RuntimeManagerConfig{NamePrefix: "ai-bot"}

	mgr, err := container.NewRuntimeManager(runner, resolver, cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mgr == nil {
		t.Fatal("expected non-nil manager")
	}
}

// --- Start ---

func TestStart_PassesCorrectRunOptions(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "ai-bot", 0)

	cfg := &container.Config{
		Image:             "my-image:latest",
		ResourceLimits:    container.ResourceLimits{Memory: "8g", CPUs: "4"},
		Env:               map[string]string{"STATIC": "val"},
		PostCreateCommand: "",
	}
	runtimeEnv := map[string]string{"API_KEY": "secret"}

	ctr, err := mgr.Start(context.Background(), cfg, "/workspace/PROJ-1", "PROJ-1", runtimeEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Container returned.
	if ctr.ID != "abc123" {
		t.Errorf("Container.ID = %q, want abc123", ctr.ID)
	}
	if !strings.HasPrefix(ctr.Name, "ai-bot-PROJ-1-") {
		t.Errorf("Container.Name = %q, does not start with ai-bot-PROJ-1-", ctr.Name)
	}

	// RunOptions verified.
	if captured.Name != ctr.Name {
		t.Errorf("RunOptions.Name = %q, want %q", captured.Name, ctr.Name)
	}
	if captured.Image != "my-image:latest" {
		t.Errorf("RunOptions.Image = %q, want my-image:latest", captured.Image)
	}
	if captured.Memory != "8g" {
		t.Errorf("RunOptions.Memory = %q, want 8g", captured.Memory)
	}
	if captured.CPUs != "4" {
		t.Errorf("RunOptions.CPUs = %q, want 4", captured.CPUs)
	}

	// Volume mount.
	if len(captured.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1", len(captured.Mounts))
	}
	mount := captured.Mounts[0]
	if mount.Source != "/workspace/PROJ-1" {
		t.Errorf("Mount.Source = %q, want /workspace/PROJ-1", mount.Source)
	}
	if mount.Target != "/workspace" {
		t.Errorf("Mount.Target = %q, want /workspace", mount.Target)
	}
	if mount.Options != "Z" {
		t.Errorf("Mount.Options = %q, want Z", mount.Options)
	}

	// Keep-alive command.
	if len(captured.Command) != 2 || captured.Command[0] != "sleep" || captured.Command[1] != "infinity" {
		t.Errorf("Command = %v, want [sleep infinity]", captured.Command)
	}
}

func TestStart_MergesEnv(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image: "img:latest",
		Env:   map[string]string{"SHARED": "from-config", "CONFIG_ONLY": "yes"},
	}
	runtimeEnv := map[string]string{"SHARED": "from-runtime", "RUNTIME_ONLY": "yes"}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", runtimeEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Runtime env overrides config env for same key.
	if captured.Env["SHARED"] != "from-runtime" {
		t.Errorf("Env[SHARED] = %q, want from-runtime (runtime overrides config)", captured.Env["SHARED"])
	}
	if captured.Env["CONFIG_ONLY"] != "yes" {
		t.Errorf("Env[CONFIG_ONLY] = %q, want yes", captured.Env["CONFIG_ONLY"])
	}
	if captured.Env["RUNTIME_ONLY"] != "yes" {
		t.Errorf("Env[RUNTIME_ONLY] = %q, want yes", captured.Env["RUNTIME_ONLY"])
	}
}

func TestStart_NilEnvMaps(t *testing.T) {
	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, _ container.RunOptions) (string, error) {
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{Image: "img:latest"}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStart_RunPostCreateCommand(t *testing.T) {
	var execCmd []string

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, _ container.RunOptions) (string, error) {
			return "abc123", nil
		},
		ExecFunc: func(_ context.Context, _ string, cmd []string) (string, int, error) {
			execCmd = cmd
			return "setup done", 0, nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:             "img:latest",
		PostCreateCommand: "make setup",
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(execCmd) != 3 || execCmd[0] != "sh" || execCmd[1] != "-c" {
		t.Fatalf("exec cmd = %v, want [sh -c ...]", execCmd)
	}
	if !strings.Contains(execCmd[2], "make setup") {
		t.Errorf("exec cmd[2] = %q, want to contain 'make setup'", execCmd[2])
	}
	if !strings.Contains(execCmd[2], "cd /workspace") {
		t.Errorf("exec cmd[2] = %q, want to contain 'cd /workspace'", execCmd[2])
	}
}

func TestStart_PostCreateCommandFailure_CleansUp(t *testing.T) {
	var stopCalled, removeCalled bool

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, _ container.RunOptions) (string, error) {
			return "abc123", nil
		},
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return "", 0, errors.New("exec failed")
		},
		StopFunc: func(_ context.Context, _ string, _ time.Duration) error {
			stopCalled = true
			return nil
		},
		RemoveFunc: func(_ context.Context, _ string) error {
			removeCalled = true
			return nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:             "img:latest",
		PostCreateCommand: "make setup",
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err == nil {
		t.Fatal("expected error from failed post-create command")
	}
	if !stopCalled {
		t.Error("expected stop to be called for cleanup")
	}
	if !removeCalled {
		t.Error("expected remove to be called for cleanup")
	}
}

func TestStart_PostCreateCommandNonZeroExit_CleansUp(t *testing.T) {
	var removeCalled bool

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, _ container.RunOptions) (string, error) {
			return "abc123", nil
		},
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return "error output", 1, nil
		},
		StopFunc: func(_ context.Context, _ string, _ time.Duration) error {
			return nil
		},
		RemoveFunc: func(_ context.Context, _ string) error {
			removeCalled = true
			return nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:             "img:latest",
		PostCreateCommand: "make setup",
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err == nil {
		t.Fatal("expected error from non-zero post-create exit")
	}
	if !strings.Contains(err.Error(), "exited with code 1") {
		t.Errorf("error = %q, want to mention exit code", err.Error())
	}
	if !removeCalled {
		t.Error("expected remove to be called for cleanup")
	}
}

func TestStart_PullFails(t *testing.T) {
	runner := &containertest.StubRunner{
		PullFunc: func(_ context.Context, _ string) error {
			return errors.New("unauthorized: access denied")
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{Image: "registry.example.com/private:latest"}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err == nil {
		t.Fatal("expected error when pull fails")
	}
	if !strings.Contains(err.Error(), "pull image") {
		t.Errorf("error = %q, want to mention 'pull image'", err.Error())
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("error = %q, want to contain underlying cause", err.Error())
	}
}

func TestStart_PullsBeforeRun(t *testing.T) {
	var sequence []string

	runner := &containertest.StubRunner{
		PullFunc: func(_ context.Context, image string) error {
			sequence = append(sequence, "pull:"+image)
			return nil
		},
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			sequence = append(sequence, "run:"+opts.Image)
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{Image: "my-image:latest"}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(sequence) != 2 {
		t.Fatalf("expected 2 operations, got %d: %v", len(sequence), sequence)
	}
	if sequence[0] != "pull:my-image:latest" {
		t.Errorf("first operation = %q, want pull:my-image:latest", sequence[0])
	}
	if sequence[1] != "run:my-image:latest" {
		t.Errorf("second operation = %q, want run:my-image:latest", sequence[1])
	}
}

func TestStart_RunFails(t *testing.T) {
	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, _ container.RunOptions) (string, error) {
			return "", errors.New("image not found")
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{Image: "bad-image:latest"}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err == nil {
		t.Fatal("expected error when run fails")
	}
	if !strings.Contains(err.Error(), "image not found") {
		t.Errorf("error = %q, want to contain underlying cause", err.Error())
	}
}

// --- Exec ---

func TestExec_ReturnsOutputAndExitCode(t *testing.T) {
	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return "hello world", 0, nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, exitCode, err := mgr.Exec(context.Background(), ctr, []string{"echo", "hello"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "hello world" {
		t.Errorf("output = %q, want 'hello world'", output)
	}
	if exitCode != 0 {
		t.Errorf("exitCode = %d, want 0", exitCode)
	}
}

func TestExec_NonZeroExitCode(t *testing.T) {
	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return "compilation error", 2, nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, exitCode, err := mgr.Exec(context.Background(), ctr, []string{"make", "build"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 2 {
		t.Errorf("exitCode = %d, want 2", exitCode)
	}
	if output != "compilation error" {
		t.Errorf("output = %q, want 'compilation error'", output)
	}
}

func TestExec_TruncatesLargeOutput(t *testing.T) {
	largeOutput := strings.Repeat("x", 200)

	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return largeOutput, 0, nil
		},
	}

	// Set a very small max output for testing.
	mgr := mustManager(t, runner, "test", 100)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, _, err := mgr.Exec(context.Background(), ctr, []string{"cat", "bigfile"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(output, "[output truncated]") {
		t.Error("expected truncation marker in output")
	}
	// The truncated output should be the first 100 bytes + marker.
	if !strings.HasPrefix(output, strings.Repeat("x", 100)) {
		t.Error("expected first 100 bytes preserved")
	}
}

func TestExec_NoTruncationUnderLimit(t *testing.T) {
	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return "short output", 0, nil
		},
	}

	mgr := mustManager(t, runner, "test", 1000)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, _, err := mgr.Exec(context.Background(), ctr, []string{"echo", "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != "short output" {
		t.Errorf("output = %q, want 'short output'", output)
	}
}

func TestExec_ExactlyAtLimit(t *testing.T) {
	exact := strings.Repeat("x", 100)

	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return exact, 0, nil
		},
	}

	mgr := mustManager(t, runner, "test", 100)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, _, err := mgr.Exec(context.Background(), ctr, []string{"cat", "file"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if output != exact {
		t.Error("output at exactly the limit should not be truncated")
	}
}

func TestExec_ErrorPassthrough(t *testing.T) {
	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return "partial", -1, errors.New("container not running")
		},
	}

	mgr := mustManager(t, runner, "test", 0)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, exitCode, err := mgr.Exec(context.Background(), ctr, []string{"ls"})
	if err == nil {
		t.Fatal("expected error to be propagated")
	}
	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1", exitCode)
	}
	if output != "partial" {
		t.Errorf("output = %q, want 'partial'", output)
	}
}

func TestExec_NoTruncationOnError(t *testing.T) {
	largeOutput := strings.Repeat("x", 200)

	runner := &containertest.StubRunner{
		ExecFunc: func(_ context.Context, _ string, _ []string) (string, int, error) {
			return largeOutput, -1, errors.New("runtime error")
		},
	}

	mgr := mustManager(t, runner, "test", 100)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	output, _, err := mgr.Exec(context.Background(), ctr, []string{"ls"})
	if err == nil {
		t.Fatal("expected error")
	}
	// Output should NOT be truncated when there's an infrastructure error.
	if output != largeOutput {
		t.Error("output should not be truncated on infrastructure error")
	}
}

// --- Stop ---

func TestStop_CallsStopAndRemove(t *testing.T) {
	var stopID, removeID string

	runner := &containertest.StubRunner{
		StopFunc: func(_ context.Context, id string, _ time.Duration) error {
			stopID = id
			return nil
		},
		RemoveFunc: func(_ context.Context, id string) error {
			removeID = id
			return nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	if err := mgr.Stop(context.Background(), ctr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stopID != "abc123" {
		t.Errorf("stop called with %q, want abc123", stopID)
	}
	if removeID != "abc123" {
		t.Errorf("remove called with %q, want abc123", removeID)
	}
}

func TestStop_StopFails_StillRemoves(t *testing.T) {
	var removeCalled bool

	runner := &containertest.StubRunner{
		StopFunc: func(_ context.Context, _ string, _ time.Duration) error {
			return errors.New("already stopped")
		},
		RemoveFunc: func(_ context.Context, _ string) error {
			removeCalled = true
			return nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	if err := mgr.Stop(context.Background(), ctr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removeCalled {
		t.Error("expected remove to be called even when stop fails")
	}
}

func TestStop_RemoveFails_ReturnsError(t *testing.T) {
	runner := &containertest.StubRunner{
		StopFunc: func(_ context.Context, _ string, _ time.Duration) error {
			return nil
		},
		RemoveFunc: func(_ context.Context, _ string) error {
			return errors.New("permission denied")
		},
	}

	mgr := mustManager(t, runner, "test", 0)
	ctr := &container.Container{ID: "abc123", Name: "test-123"}

	err := mgr.Stop(context.Background(), ctr)
	if err == nil {
		t.Fatal("expected error when remove fails")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want to contain underlying cause", err.Error())
	}
}

// --- CleanupOrphans ---

func TestCleanupOrphans_NoOrphans(t *testing.T) {
	runner := &containertest.StubRunner{
		ListContainersFunc: func(_ context.Context, _ string) ([]string, error) {
			return nil, nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	if err := mgr.CleanupOrphans(context.Background(), "ai-bot-"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCleanupOrphans_RemovesAllOrphans(t *testing.T) {
	var stopped, removed []string

	runner := &containertest.StubRunner{
		ListContainersFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{"ai-bot-aaa", "ai-bot-bbb"}, nil
		},
		StopFunc: func(_ context.Context, id string, _ time.Duration) error {
			stopped = append(stopped, id)
			return nil
		},
		RemoveFunc: func(_ context.Context, id string) error {
			removed = append(removed, id)
			return nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	if err := mgr.CleanupOrphans(context.Background(), "ai-bot-"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(stopped) != 2 {
		t.Errorf("stopped %d containers, want 2", len(stopped))
	}
	if len(removed) != 2 {
		t.Errorf("removed %d containers, want 2", len(removed))
	}
}

func TestCleanupOrphans_PartialFailure_ContinuesAndReturnsJoinedError(t *testing.T) {
	runner := &containertest.StubRunner{
		ListContainersFunc: func(_ context.Context, _ string) ([]string, error) {
			return []string{"ai-bot-ok", "ai-bot-fail", "ai-bot-ok2"}, nil
		},
		StopFunc: func(_ context.Context, _ string, _ time.Duration) error {
			return nil
		},
		RemoveFunc: func(_ context.Context, id string) error {
			if id == "ai-bot-fail" {
				return errors.New("busy")
			}
			return nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	err := mgr.CleanupOrphans(context.Background(), "ai-bot-")
	if err == nil {
		t.Fatal("expected error from partial failure")
	}
	if !strings.Contains(err.Error(), "busy") {
		t.Errorf("error = %q, want to contain 'busy'", err.Error())
	}
}

func TestCleanupOrphans_ListFails(t *testing.T) {
	runner := &containertest.StubRunner{
		ListContainersFunc: func(_ context.Context, _ string) ([]string, error) {
			return nil, errors.New("runtime unavailable")
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	err := mgr.CleanupOrphans(context.Background(), "ai-bot-")
	if err == nil {
		t.Fatal("expected error when list fails")
	}
}

// --- Start: host policy passthrough ---

func TestStart_DisableSELinux(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:          "img:latest",
		DisableSELinux: true,
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured.SecurityOpt) != 1 || captured.SecurityOpt[0] != "label=disable" {
		t.Errorf("SecurityOpt = %v, want [label=disable]", captured.SecurityOpt)
	}
}

func TestStart_DisableSELinuxFalse_NoSecurityOpt(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:          "img:latest",
		DisableSELinux: false,
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured.SecurityOpt) != 0 {
		t.Errorf("SecurityOpt = %v, want empty", captured.SecurityOpt)
	}
}

func TestStart_UserNS(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:  "img:latest",
		UserNS: "keep-id",
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if captured.UserNS != "keep-id" {
		t.Errorf("UserNS = %q, want keep-id", captured.UserNS)
	}
}

func TestStart_Tmpfs(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image: "img:latest",
		Tmpfs: []string{"/tmp:size=4g", "/run"},
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured.Tmpfs) != 2 {
		t.Fatalf("Tmpfs count = %d, want 2", len(captured.Tmpfs))
	}
	if captured.Tmpfs[0] != "/tmp:size=4g" {
		t.Errorf("Tmpfs[0] = %q, want /tmp:size=4g", captured.Tmpfs[0])
	}
	if captured.Tmpfs[1] != "/run" {
		t.Errorf("Tmpfs[1] = %q, want /run", captured.Tmpfs[1])
	}
}

func TestStart_ExtraMounts(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image: "img:latest",
		ExtraMounts: []container.Mount{
			{Source: "/host/cache", Target: "/cache", Options: "ro"},
			{Source: "/host/data", Target: "/data"},
		},
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// First mount is always the workspace, then extra mounts.
	if len(captured.Mounts) != 3 {
		t.Fatalf("Mounts count = %d, want 3 (workspace + 2 extra)", len(captured.Mounts))
	}

	// Workspace mount first.
	if captured.Mounts[0].Source != "/ws" || captured.Mounts[0].Target != "/workspace" {
		t.Errorf("Mounts[0] = %+v, want workspace mount", captured.Mounts[0])
	}

	// Extra mounts appended in order.
	if captured.Mounts[1].Source != "/host/cache" || captured.Mounts[1].Target != "/cache" || captured.Mounts[1].Options != "ro" {
		t.Errorf("Mounts[1] = %+v, want cache mount", captured.Mounts[1])
	}
	if captured.Mounts[2].Source != "/host/data" || captured.Mounts[2].Target != "/data" {
		t.Errorf("Mounts[2] = %+v, want data mount", captured.Mounts[2])
	}
}

func TestStart_AddHosts(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:    "img:latest",
		AddHosts: []string{"proxyhost:host-gateway", "extra:10.0.0.1"},
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured.AddHosts) != 2 {
		t.Fatalf("AddHosts count = %d, want 2", len(captured.AddHosts))
	}
	if captured.AddHosts[0] != "proxyhost:host-gateway" {
		t.Errorf("AddHosts[0] = %q, want proxyhost:host-gateway", captured.AddHosts[0])
	}
	if captured.AddHosts[1] != "extra:10.0.0.1" {
		t.Errorf("AddHosts[1] = %q, want extra:10.0.0.1", captured.AddHosts[1])
	}
}

func TestStart_AllHostPolicyCombined(t *testing.T) {
	var captured container.RunOptions

	runner := &containertest.StubRunner{
		RunFunc: func(_ context.Context, opts container.RunOptions) (string, error) {
			captured = opts
			return "abc123", nil
		},
	}

	mgr := mustManager(t, runner, "test", 0)

	cfg := &container.Config{
		Image:          "img:latest",
		DisableSELinux: true,
		UserNS:         "keep-id:uid=1000,gid=1000",
		Tmpfs:          []string{"/tmp:size=2g"},
		ExtraMounts: []container.Mount{
			{Source: "/host/tools", Target: "/tools"},
		},
	}

	_, err := mgr.Start(context.Background(), cfg, "/ws", "TEST-1", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured.SecurityOpt) != 1 || captured.SecurityOpt[0] != "label=disable" {
		t.Errorf("SecurityOpt = %v, want [label=disable]", captured.SecurityOpt)
	}
	if captured.UserNS != "keep-id:uid=1000,gid=1000" {
		t.Errorf("UserNS = %q, want keep-id:uid=1000,gid=1000", captured.UserNS)
	}
	if len(captured.Tmpfs) != 1 || captured.Tmpfs[0] != "/tmp:size=2g" {
		t.Errorf("Tmpfs = %v, want [/tmp:size=2g]", captured.Tmpfs)
	}
	if len(captured.Mounts) != 2 {
		t.Fatalf("Mounts count = %d, want 2", len(captured.Mounts))
	}
}

// --- ResolveConfig ---

func TestResolveConfig_DelegatesToResolver(t *testing.T) {
	repoDir := t.TempDir()
	runner := &containertest.StubRunner{}

	mgr := mustManager(t, runner, "test", 0)

	// With no repo config and no profile override, the resolver
	// returns an error (no image configured).
	_, err := mgr.ResolveConfig(repoDir, nil)
	if err == nil {
		t.Fatal("expected error when no image is configured")
	}

	// With a profile override providing an image, it succeeds.
	override := &container.SettingsOverride{Image: "test:latest"}
	cfg, err := mgr.ResolveConfig(repoDir, override)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Image != "test:latest" {
		t.Errorf("Image = %q, want test:latest", cfg.Image)
	}
}

// --- helpers ---

func mustResolver(t *testing.T) *container.Resolver {
	t.Helper()
	r, err := container.NewResolver(container.ResolverDefaults{}, zap.NewNop())
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func mustManager(t *testing.T, runner container.Runner, prefix string, maxOutput int) container.Manager {
	t.Helper()
	resolver := mustResolver(t)
	cfg := container.RuntimeManagerConfig{
		NamePrefix:     prefix,
		MaxOutputBytes: maxOutput,
	}
	mgr, err := container.NewRuntimeManager(runner, resolver, cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("NewRuntimeManager: %v", err)
	}
	return mgr
}
