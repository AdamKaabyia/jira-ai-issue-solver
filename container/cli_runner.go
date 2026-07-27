package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Compile-time check that CLIRunner implements Runner.
var _ Runner = (*CLIRunner)(nil)

// CLIRunner implements [Runner] by shelling out to a container runtime
// binary (podman or docker). Both runtimes share the same CLI
// interface, so one implementation handles both.
type CLIRunner struct {
	runtimePath string
}

// NewCLIRunner creates a runner that uses the given runtime binary.
// The [DetectedRuntime] is typically produced by [DetectRuntime].
func NewCLIRunner(detected *DetectedRuntime) *CLIRunner {
	return &CLIRunner{runtimePath: detected.Path}
}

func (r *CLIRunner) Pull(ctx context.Context, image string) error {
	args := []string{"pull", image}

	cmd := exec.CommandContext(ctx, r.runtimePath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("container pull %s: %w: %s",
			image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *CLIRunner) Run(ctx context.Context, opts RunOptions) (string, error) {
	args := []string{"run", "-d", "--pull=never"}

	if opts.Name != "" {
		args = append(args, "--name", opts.Name)
	}

	for _, m := range opts.Mounts {
		spec := m.Source + ":" + m.Target
		if m.Options != "" {
			spec += ":" + m.Options
		}
		args = append(args, "-v", spec)
	}

	// Sort env keys for deterministic flag order (aids debugging).
	keys := make([]string, 0, len(opts.Env))
	for k := range opts.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+opts.Env[k])
	}

	if opts.Memory != "" {
		args = append(args, "--memory", opts.Memory)
	}
	if opts.CPUs != "" {
		args = append(args, "--cpus", opts.CPUs)
	}

	for _, opt := range opts.SecurityOpt {
		args = append(args, "--security-opt", opt)
	}
	if opts.UserNS != "" {
		args = append(args, "--userns", opts.UserNS)
	}
	for _, t := range opts.Tmpfs {
		args = append(args, "--tmpfs", t)
	}
	for _, h := range opts.AddHosts {
		args = append(args, "--add-host", h)
	}

	args = append(args, opts.Image)
	args = append(args, opts.Command...)

	cmd := exec.CommandContext(ctx, r.runtimePath, args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("container run: %w: %s",
			err, strings.TrimSpace(stderr.String()))
	}

	return strings.TrimSpace(string(out)), nil
}

func (r *CLIRunner) Exec(ctx context.Context, containerID string, cmd []string) (string, int, error) {
	args := make([]string, 0, 2+len(cmd))
	args = append(args, "exec", containerID)
	args = append(args, cmd...)

	c := exec.CommandContext(ctx, r.runtimePath, args...)
	out, err := c.CombinedOutput()

	if err != nil {
		// Context cancellation takes priority: the exit error is a
		// side effect of the process being killed.
		if ctx.Err() != nil {
			return string(out), -1, fmt.Errorf("container exec: %w", ctx.Err())
		}
		// Non-zero exit code from the command itself (not an
		// infrastructure error).
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode(), nil
		}
		return string(out), -1, fmt.Errorf("container exec: %w", err)
	}

	return string(out), 0, nil
}

func (r *CLIRunner) Stop(ctx context.Context, containerID string, timeout time.Duration) error {
	timeoutSec := fmt.Sprintf("%d", int(timeout.Seconds()))
	args := []string{"stop", "-t", timeoutSec, containerID}

	cmd := exec.CommandContext(ctx, r.runtimePath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("container stop: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *CLIRunner) Remove(ctx context.Context, containerID string) error {
	args := []string{"rm", "-f", containerID}

	cmd := exec.CommandContext(ctx, r.runtimePath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("container rm: %w: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (r *CLIRunner) ListContainers(ctx context.Context, namePrefix string) ([]string, error) {
	args := []string{
		"ps", "-a",
		"--filter", "name=" + namePrefix,
		"--format", "{{.Names}}",
	}

	cmd := exec.CommandContext(ctx, r.runtimePath, args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("container list: %w: %s",
			err, strings.TrimSpace(stderr.String()))
	}

	output := strings.TrimSpace(string(out))
	if output == "" {
		return nil, nil
	}

	// Docker's name filter matches substrings, not prefixes. Filter
	// client-side to ensure only true prefix matches are returned.
	var matched []string
	for name := range strings.SplitSeq(output, "\n") {
		name = strings.TrimSpace(name)
		if name != "" && strings.HasPrefix(name, namePrefix) {
			matched = append(matched, name)
		}
	}
	return matched, nil
}
