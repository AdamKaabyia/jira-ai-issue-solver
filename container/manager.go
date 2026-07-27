// Package container manages dev container lifecycle for AI agent execution.
//
// Containers provide isolated environments where AI agents run with full
// toolchain access while preventing access to GitHub tokens, Jira
// credentials, and the host system. Teams provide container images with
// their toolchain and AI CLI installed; the bot manages the container
// lifecycle.
//
// Container configuration is resolved from multiple sources by [Resolver]:
//  1. .ai-bot/container.json in the repository (highest priority)
//  2. .devcontainer/devcontainer.json in the repository
//  3. Bot-level defaults (container.default_image, resource limits)
//  4. Built-in minimal fallback (lowest priority)
//
// Fields not set by higher-priority sources inherit values from
// lower-priority sources. See [Resolver.Resolve] for details.
package container

import "context"

// Manager defines the contract for container lifecycle operations.
//
// Implementations manage resolving configuration, starting containers
// with appropriate mounts and resource limits, executing commands inside
// running containers, stopping containers, and cleaning up orphans from
// prior crashes.
type Manager interface {
	// ResolveConfig determines the container configuration for a
	// repository by checking configuration sources in priority order.
	// The projectOverride, if non-nil, sits between repo-level config
	// and the global fallback. See package documentation for the
	// resolution chain.
	ResolveConfig(repoDir string, projectOverride *SettingsOverride) (*Config, error)

	// Start launches a container with the given configuration. The
	// workspace directory is mounted into the container at /workspace.
	// The env map provides runtime environment variables (AI provider,
	// API keys, etc.) that are separate from the container config's
	// static env vars. The ticketKey is included in the container
	// name and labels for operational visibility.
	Start(ctx context.Context, cfg *Config, workspaceDir, ticketKey string,
		env map[string]string) (*Container, error)

	// Exec runs a command inside a running container. It captures
	// combined stdout/stderr output. A non-zero exit code is not
	// treated as an error; it is returned via exitCode. The context
	// controls timeout.
	Exec(ctx context.Context, ctr *Container,
		cmd []string) (output string, exitCode int, err error)

	// Stop stops and removes a running container.
	Stop(ctx context.Context, ctr *Container) error

	// CleanupOrphans finds and removes containers whose names match
	// the given prefix. Used at startup to clean up containers
	// abandoned by a prior crash.
	CleanupOrphans(ctx context.Context, prefix string) error
}

// Container represents a running container instance.
type Container struct {
	// ID is the runtime-assigned identifier (e.g., the container hash).
	ID string

	// Name is the human-readable name following the bot's naming
	// convention, used for orphan identification.
	Name string
}

// Config holds the resolved container configuration. It is produced by
// [Resolver.Resolve], which merges configuration from repository-level
// files, bot-level defaults, and built-in fallbacks.
//
// All fields have usable zero values: an empty Config will cause the
// container to use its image defaults with no extra env vars or limits.
type Config struct {
	// Image is the container image reference (e.g., "my-org/dev:latest").
	Image string

	// Env holds static environment variables from the container config.
	// When [Resolver.Resolve] merges configuration from multiple
	// sources, env vars are combined additively: repo-level keys
	// override bot-level keys with the same name, but bot-level keys
	// not present in repo config are preserved. These are separate
	// from runtime env vars (API keys, etc.) passed to Manager.Start.
	Env map[string]string

	// ResourceLimits constrains the container's resource usage.
	ResourceLimits ResourceLimits

	// PostCreateCommand is a shell command to run after the container
	// starts (e.g., "npm install", "go mod download"). Empty means
	// no post-create command.
	PostCreateCommand string

	// DisableSELinux disables SELinux confinement
	// (--security-opt=label=disable).
	DisableSELinux bool

	// UserNS sets the user namespace mode (e.g., "keep-id").
	UserNS string

	// Tmpfs holds tmpfs mount specs (e.g., "/tmp:size=4g").
	Tmpfs []string

	// ExtraMounts holds additional volume mounts beyond the workspace.
	ExtraMounts []Mount

	// AddHosts holds --add-host entries injected at container
	// creation (e.g., "proxyhost:host-gateway"). Used to let the
	// container resolve custom hostnames pointing at the host.
	AddHosts []string

	// Source identifies which configuration file produced this config,
	// for logging and debugging. Empty for built-in defaults.
	Source string
}

// ResourceLimits constrains container resource usage. Values are passed
// directly to the container runtime (e.g., --memory, --cpus flags).
// Empty values mean no limit is applied.
type ResourceLimits struct {
	// Memory limit in container runtime format (e.g., "8g", "512m").
	Memory string

	// CPUs limit in container runtime format (e.g., "4", "0.5").
	CPUs string
}
