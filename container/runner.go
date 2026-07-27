package container

import (
	"context"
	"time"
)

// Runner abstracts the container runtime CLI (podman or docker),
// providing the low-level operations needed for container lifecycle
// management. Both podman and docker implement the same CLI interface,
// so a single implementation ([CLIRunner]) handles both.
//
// This is the execution counterpart to [DetectRuntime], which finds
// the runtime binary on PATH. DetectRuntime produces a
// [DetectedRuntime] that is passed to [NewCLIRunner] to create a
// Runner.
type Runner interface {
	// Pull fetches a container image from a registry. This is a
	// no-op if the image is already present and up to date. Pulling
	// is separated from [Run] to allow different timeout profiles:
	// large images (multi-GB) may take minutes to download, while
	// container creation is near-instant.
	Pull(ctx context.Context, image string) error

	// Run creates and starts a container in detached mode. Returns
	// the runtime-assigned container ID. The image must already be
	// present (see [Pull]).
	Run(ctx context.Context, opts RunOptions) (containerID string, err error)

	// Exec runs a command inside a running container. Combined
	// stdout/stderr is returned as output. A non-zero exit code from
	// the command is not treated as an error; it is returned via
	// exitCode. The error return is reserved for infrastructure
	// failures (container not running, runtime error, context
	// cancelled).
	Exec(ctx context.Context, containerID string,
		cmd []string) (output string, exitCode int, err error)

	// Stop stops a running container with the given timeout grace
	// period. After the timeout, the runtime sends SIGKILL. Stopping
	// an already-stopped container is not an error.
	Stop(ctx context.Context, containerID string,
		timeout time.Duration) error

	// Remove removes a container. Removing a non-existent container
	// is not an error.
	Remove(ctx context.Context, containerID string) error

	// ListContainers returns the names of all containers (running or
	// stopped) whose name starts with the given prefix. An empty
	// result is not an error.
	ListContainers(ctx context.Context,
		namePrefix string) ([]string, error)
}

// RunOptions configures a new container created by [Runner.Run].
type RunOptions struct {
	// Name is the container name, used for identification and orphan
	// cleanup. Must be unique across running containers.
	Name string

	// Image is the container image to use.
	Image string

	// Mounts specifies volume mounts into the container.
	Mounts []Mount

	// Env holds environment variables injected into the container.
	Env map[string]string

	// Memory limit in runtime format (e.g., "8g"). Empty means no
	// limit.
	Memory string

	// CPUs limit in runtime format (e.g., "4"). Empty means no
	// limit.
	CPUs string

	// SecurityOpt holds --security-opt flags (e.g., "label=disable").
	SecurityOpt []string

	// UserNS sets the --userns flag (e.g., "keep-id",
	// "keep-id:uid=1000,gid=1000"). Empty means runtime default.
	UserNS string

	// Tmpfs holds --tmpfs mount specs (e.g., "/tmp:size=4g").
	Tmpfs []string

	// AddHosts holds --add-host entries (e.g.,
	// "proxyhost:host-gateway"). Used to make the container
	// resolve custom hostnames to the host machine.
	AddHosts []string

	// Command is the entrypoint command to keep the container alive
	// (e.g., ["sleep", "infinity"]).
	Command []string
}

// Mount specifies a volume mount for a container.
type Mount struct {
	// Source is the host path.
	Source string

	// Target is the container path.
	Target string

	// Options are mount options (e.g., "Z" for SELinux relabeling).
	// Empty means no extra options.
	Options string
}
