package container

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"time"

	"go.uber.org/zap"
)

// Compile-time check that RuntimeManager implements Manager.
var _ Manager = (*RuntimeManager)(nil)

const (
	// defaultStopTimeout is the grace period before SIGKILL when
	// stopping a container.
	defaultStopTimeout = 10 * time.Second

	// workspaceMountTarget is where the workspace is mounted inside
	// the container.
	workspaceMountTarget = "/workspace"

	// workspaceMountOptions enables SELinux relabeling. This is a
	// no-op on systems without SELinux.
	workspaceMountOptions = "Z"

	// defaultMaxOutputBytes is the default output truncation limit
	// for Exec (1 MB).
	defaultMaxOutputBytes = 1 << 20
)

// RuntimeManager implements [Manager] by composing a [Runner] for
// container operations and a [Resolver] for configuration resolution.
// It manages the full container lifecycle: resolving configuration,
// starting containers with appropriate mounts and resource limits,
// executing commands, stopping containers, and cleaning up orphans.
type RuntimeManager struct {
	runner     Runner
	resolver   *Resolver
	namePrefix string
	maxOutput  int
	logger     *zap.Logger
}

// RuntimeManagerConfig holds construction parameters for
// [RuntimeManager].
type RuntimeManagerConfig struct {
	// NamePrefix is prepended to container names for identification
	// and orphan cleanup (e.g., "ai-bot").
	NamePrefix string

	// MaxOutputBytes limits the output captured from [Manager.Exec].
	// Output exceeding this limit is truncated with a marker. Zero
	// or negative uses the default (1 MB).
	MaxOutputBytes int
}

// NewRuntimeManager creates a Manager backed by the given Runner and
// Resolver. Returns an error if any required dependency is nil or the
// configuration is invalid.
func NewRuntimeManager(runner Runner, resolver *Resolver, cfg RuntimeManagerConfig, logger *zap.Logger) (*RuntimeManager, error) {
	if runner == nil {
		return nil, errors.New("runner must not be nil")
	}
	if resolver == nil {
		return nil, errors.New("resolver must not be nil")
	}
	if logger == nil {
		return nil, errors.New("logger must not be nil")
	}
	if cfg.NamePrefix == "" {
		return nil, errors.New("name prefix must not be empty")
	}

	maxOutput := cfg.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = defaultMaxOutputBytes
	}

	return &RuntimeManager{
		runner:     runner,
		resolver:   resolver,
		namePrefix: cfg.NamePrefix,
		maxOutput:  maxOutput,
		logger:     logger,
	}, nil
}

func (m *RuntimeManager) ResolveConfig(repoDir string, projectOverride *SettingsOverride) (*Config, error) {
	return m.resolver.Resolve(repoDir, projectOverride)
}

func (m *RuntimeManager) Start(ctx context.Context, cfg *Config, workspaceDir, ticketKey string, env map[string]string) (*Container, error) {
	name := m.generateName(ticketKey)

	// Merge environment: config env as base, runtime env overrides.
	mergedEnv := make(map[string]string, len(cfg.Env)+len(env))
	maps.Copy(mergedEnv, cfg.Env)
	maps.Copy(mergedEnv, env)

	mounts := []Mount{{
		Source:  workspaceDir,
		Target:  workspaceMountTarget,
		Options: workspaceMountOptions,
	}}
	mounts = append(mounts, cfg.ExtraMounts...)

	var securityOpt []string
	if cfg.DisableSELinux {
		securityOpt = append(securityOpt, "label=disable")
	}

	opts := RunOptions{
		Name:        name,
		Image:       cfg.Image,
		Mounts:      mounts,
		Env:         mergedEnv,
		Memory:      cfg.ResourceLimits.Memory,
		CPUs:        cfg.ResourceLimits.CPUs,
		SecurityOpt: securityOpt,
		UserNS:      cfg.UserNS,
		Tmpfs:       cfg.Tmpfs,
		AddHosts:    cfg.AddHosts,
		Command:     []string{"sleep", "infinity"},
	}

	m.logger.Info("Pulling image",
		zap.String("image", cfg.Image))

	if err := m.runner.Pull(ctx, cfg.Image); err != nil {
		return nil, fmt.Errorf("pull image %s: %w", cfg.Image, err)
	}

	m.logger.Info("Starting container",
		zap.String("name", name),
		zap.String("image", cfg.Image),
		zap.String("workspace", workspaceDir))

	id, err := m.runner.Run(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("start container %s: %w", name, err)
	}

	ctr := &Container{ID: id, Name: name}

	// Run post-create command if configured.
	if cfg.PostCreateCommand != "" {
		m.logger.Info("Running post-create command",
			zap.String("container", name),
			zap.String("command", cfg.PostCreateCommand))

		cmd := []string{
			"sh", "-c",
			"cd " + workspaceMountTarget + " && " + cfg.PostCreateCommand,
		}
		output, exitCode, execErr := m.runner.Exec(ctx, id, cmd)
		if execErr != nil {
			_ = m.stopAndRemove(ctx, ctr)
			return nil, fmt.Errorf("post-create command in %s: %w", name, execErr)
		}
		if exitCode != 0 {
			_ = m.stopAndRemove(ctx, ctr)
			return nil, fmt.Errorf(
				"post-create command in %s exited with code %d: %s",
				name, exitCode, output)
		}
	}

	m.logger.Info("Container started",
		zap.String("name", name),
		zap.String("id", id))

	return ctr, nil
}

func (m *RuntimeManager) Exec(ctx context.Context, ctr *Container, cmd []string) (string, int, error) {
	output, exitCode, err := m.runner.Exec(ctx, ctr.ID, cmd)
	if err != nil {
		return output, exitCode, err
	}

	if m.maxOutput > 0 && len(output) > m.maxOutput {
		m.logger.Warn("Truncating container exec output",
			zap.String("container", ctr.Name),
			zap.Int("original_bytes", len(output)),
			zap.Int("truncated_to", m.maxOutput))
		output = output[:m.maxOutput] + "\n... [output truncated]"
	}

	return output, exitCode, nil
}

func (m *RuntimeManager) Stop(ctx context.Context, ctr *Container) error {
	return m.stopAndRemove(ctx, ctr)
}

func (m *RuntimeManager) CleanupOrphans(ctx context.Context, prefix string) error {
	names, err := m.runner.ListContainers(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list orphaned containers: %w", err)
	}

	if len(names) == 0 {
		return nil
	}

	m.logger.Info("Found orphaned containers",
		zap.Int("count", len(names)),
		zap.String("prefix", prefix))

	var errs []error
	for _, name := range names {
		m.logger.Info("Removing orphaned container",
			zap.String("name", name))
		// Use name as both ID and Name: container runtimes accept
		// names wherever they accept IDs.
		ctr := &Container{ID: name, Name: name}
		if stopErr := m.stopAndRemove(ctx, ctr); stopErr != nil {
			m.logger.Warn("Failed to remove orphaned container",
				zap.String("name", name),
				zap.Error(stopErr))
			errs = append(errs, stopErr)
		}
	}

	return errors.Join(errs...)
}

// stopAndRemove stops and then removes a container. A stop failure is
// logged but does not prevent the remove attempt (the container may
// already be stopped).
func (m *RuntimeManager) stopAndRemove(ctx context.Context, ctr *Container) error {
	m.logger.Debug("Stopping container",
		zap.String("name", ctr.Name),
		zap.String("id", ctr.ID))

	if err := m.runner.Stop(ctx, ctr.ID, defaultStopTimeout); err != nil {
		m.logger.Debug("Stop returned error (will attempt removal)",
			zap.String("name", ctr.Name),
			zap.Error(err))
	}

	if err := m.runner.Remove(ctx, ctr.ID); err != nil {
		return fmt.Errorf("remove container %s: %w", ctr.Name, err)
	}
	return nil
}

// generateName produces a unique container name using the configured
// prefix, ticket key, and a random suffix. The ticket key provides
// operational visibility (e.g., "ai-bot-EDM-2747-a1b2c3d4").
func (m *RuntimeManager) generateName(ticketKey string) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s-%s-%d", m.namePrefix, ticketKey, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%s-%x", m.namePrefix, ticketKey, b)
}
