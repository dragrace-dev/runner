package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"

	"dragrace/internal/executor"
	"dragrace/internal/gpu"
	"dragrace/internal/metrics"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Executor implements executor.Executor using Docker containers.
type Executor struct {
	client *client.Client
	// gpuPolicy is the operator's GPU exposure ceiling (#65), resolved and
	// validated once at startup. Its zero value exposes no GPU.
	gpuPolicy gpu.Policy
}

// Baseline resource ceilings per phase, applied when a challenge sets no
// explicit limits.yaml values. The build phase is a compile/setup step and
// stays modest; the run phase is generous by default because that's where
// measured solutions execute. Both are scaled down to fit the actual host
// in resolveResources — see #58: a hardcoded 8-CPU run default made every
// host with fewer cores fail to start a measured run at all.
const (
	defaultBuildMemory = 512 * 1024 * 1024 // 512MB
	defaultBuildCPU    = 1_000_000_000     // 1 CPU

	defaultRunMemory = 32 * 1024 * 1024 * 1024 // 32GB
	defaultRunCPU    = 8_000_000_000           // 8 CPUs
)

// Compile-time check that Executor implements the interface.
var _ executor.Executor = (*Executor)(nil)

// NewExecutor builds a Docker executor. gpuPolicy is the operator's GPU
// exposure ceiling, already resolved against the host's actual hardware
// (#65); pass the zero Policy to expose no GPU, which is the default.
func NewExecutor(dockerHost string, gpuPolicy gpu.Policy) (*Executor, error) {
	opts := []client.Opt{
		client.FromEnv,
	}

	if dockerHost != "" {
		opts = append(opts, client.WithHost(dockerHost))
	}

	cli, err := client.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}

	// The client above connected to nothing: it is lazy. Block until the daemon
	// actually answers, so a daemon that is absent or still starting (#70:
	// dockerd boots inside this very container) surfaces here instead of as a
	// failure of the first job submitted. See wait.go.
	if err := waitForDaemon(context.Background(), cli, cli.DaemonHost(), daemonWaitTimeout()); err != nil {
		cli.Close()
		return nil, err
	}

	log.Println("✅ Docker client initialized")

	return &Executor{
		client:    cli,
		gpuPolicy: gpuPolicy,
	}, nil
}

func (e *Executor) Close() error {
	if e.client != nil {
		return e.client.Close()
	}
	return nil
}

// GetClient returns the underlying Docker client (for internal use only).
func (e *Executor) GetClient() *client.Client {
	return e.client
}

// RunScript executes a script in a Docker container and waits for completion.
func (e *Executor) RunScript(ctx context.Context, opts *executor.RunOptions) (string, error) {
	log.Printf("🏃 Running script: %s in %s", opts.ScriptPath, opts.Image)

	// Pull image
	reader, err := e.client.ImagePull(ctx, opts.Image, image.PullOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to pull image %s: %w", opts.Image, err)
	}
	defer reader.Close()
	io.Copy(io.Discard, reader)

	// Build command with optional args
	cmd := buildDockerCmd(opts)

	// Configure mounts
	mounts := buildMounts(opts)
	binds := buildBinds(opts)

	// Configure resources, sized to what this host can actually run (#58).
	resources, err := e.resolveResources(ctx, opts, defaultBuildMemory, defaultBuildCPU)
	if err != nil {
		return "", err
	}

	// Build env vars
	env := buildDockerEnv(opts)

	containerCfg := &container.Config{
		Image:      opts.Image,
		Cmd:        cmd,
		Env:        env,
		Tty:        false,
		WorkingDir: "/workspace",
		Labels:     sandboxLabels,
	}
	hostCfg := baseHostConfig(mounts, binds, resources, opts.NetworkEnabled)
	if !opts.Trusted {
		hardenSandbox(containerCfg, hostCfg)
	}
	if err := applyGPUPolicy(hostCfg, e.gpuPolicy); err != nil {
		return "", err
	}

	// Create container
	resp, err := e.client.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return "", fmt.Errorf("failed to create container: %w", err)
	}

	containerID := resp.ID
	log.Printf("   Container created: %s", containerID[:12])

	// Ensure cleanup
	defer func() {
		removeOpts := container.RemoveOptions{Force: true}
		if err := e.client.ContainerRemove(context.Background(), containerID, removeOpts); err != nil {
			log.Printf("⚠️  Failed to remove container: %v", err)
		}
	}()

	// Start container
	if err := e.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return "", fmt.Errorf("failed to start container: %w", err)
	}

	// Wait for completion
	statusCh, errCh := e.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			return "", fmt.Errorf("container wait error: %w", err)
		}
	case status := <-statusCh:
		if status.StatusCode != 0 {
			logs, _ := e.getContainerLogs(ctx, containerID)
			return logs, fmt.Errorf("script exited with code %d", status.StatusCode)
		}
	case <-ctx.Done():
		return "", fmt.Errorf("execution timeout")
	}

	return e.getContainerLogs(ctx, containerID)
}

// RunContainer executes code in an isolated Docker container (legacy helper).
func (e *Executor) RunContainer(ctx context.Context, imageName string, cmd []string, timeoutSeconds int) (string, error) {
	return e.RunScript(ctx, &executor.RunOptions{
		Image:      imageName,
		ScriptPath: cmd[len(cmd)-1],
		Limits: &executor.ResourceLimits{
			Timeout: timeoutSeconds,
		},
	})
}

// RunMeasured executes a script and collects metrics during execution.
// Owns the full lifecycle: create → start → collect metrics → wait → cleanup.
// This runs solution-controlled code, so it always applies the strict
// sandbox (non-root, read-only rootfs) on top of the baseline hardening.
func (e *Executor) RunMeasured(ctx context.Context, opts *executor.RunOptions) (*metrics.RunMetrics, string, error) {
	log.Printf("🏃 Running script with metrics: %s in %s", opts.ScriptPath, opts.Image)

	// Pull image
	reader, err := e.client.ImagePull(ctx, opts.Image, image.PullOptions{})
	if err != nil {
		return nil, "", fmt.Errorf("failed to pull image %s: %w", opts.Image, err)
	}
	defer reader.Close()
	io.Copy(io.Discard, reader)

	// The run phase is non-root; a writable data dir (only used by local
	// `runner test` runs) needs to be handed over to that user first, since
	// Docker always creates named volumes root-owned.
	if opts.DataDir != "" && !opts.ReadOnlyData {
		if err := e.chownDataDir(ctx, opts.Image, opts.DataDir); err != nil {
			return nil, "", fmt.Errorf("failed to prepare data dir: %w", err)
		}
	}

	// Build command with optional args
	cmd := buildDockerCmd(opts)

	// Configure mounts
	mounts := buildMounts(opts)
	binds := buildBinds(opts)

	// Configure resources, sized to what this host can actually run (#58).
	resources, err := e.resolveResources(ctx, opts, defaultRunMemory, defaultRunCPU)
	if err != nil {
		return nil, "", err
	}

	// Build env vars
	env := buildDockerEnv(opts)

	containerCfg := &container.Config{
		Image:      opts.Image,
		Cmd:        cmd,
		Env:        env,
		Tty:        false,
		WorkingDir: "/workspace",
		Labels:     sandboxLabels,
	}
	hostCfg := baseHostConfig(mounts, binds, resources, opts.NetworkEnabled)
	hardenSandbox(containerCfg, hostCfg)
	if err := applyGPUPolicy(hostCfg, e.gpuPolicy); err != nil {
		return nil, "", err
	}

	// Create container
	resp, err := e.client.ContainerCreate(ctx, containerCfg, hostCfg, nil, nil, "")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create container: %w", err)
	}

	containerID := resp.ID
	log.Printf("   Container created: %s (measuring metrics)", containerID[:12])

	// Ensure cleanup even if a future change adds an early return below.
	defer func() {
		if err := e.client.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true}); err != nil {
			log.Printf("⚠️  Failed to remove container: %v", err)
		}
	}()

	// Start container
	if err := e.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		return nil, "", fmt.Errorf("failed to start container: %w", err)
	}

	// Start metrics collector
	collector := metrics.NewCollector(e.client, containerID, 100)
	collector.Start(ctx)

	// Wait for container to finish
	exitCode, logs, err := e.waitContainer(ctx, containerID)

	// Stop metrics collector
	runMetrics := collector.Stop()

	if err != nil {
		log.Printf("❌ Run phase error: %s", logs)
		return nil, logs, err
	}

	if exitCode != 0 {
		log.Printf("❌ Run phase exited with code %d: %s", exitCode, logs)
		return nil, logs, fmt.Errorf("run script exited with code %d", exitCode)
	}

	runMetrics.Aggregates.ExitCode = int(exitCode)
	log.Printf("📊 Collected %d samples over %dms", len(runMetrics.TimeSeries.Samples), runMetrics.Aggregates.ExecutionTimeMs)

	return runMetrics, logs, nil
}

// waitContainer waits for a container to finish and returns the exit code and logs.
func (e *Executor) waitContainer(ctx context.Context, containerID string) (int64, string, error) {
	statusCh, errCh := e.client.ContainerWait(ctx, containerID, container.WaitConditionNotRunning)

	select {
	case err := <-errCh:
		if err != nil {
			logs, _ := e.getContainerLogs(ctx, containerID)
			return -1, logs, fmt.Errorf("container wait error: %w", err)
		}
	case status := <-statusCh:
		logs, _ := e.getContainerLogs(ctx, containerID)
		return status.StatusCode, logs, nil
	case <-ctx.Done():
		logs, _ := e.getContainerLogs(ctx, containerID)
		return -1, logs, fmt.Errorf("execution timeout")
	}

	return -1, "", nil
}

// getContainerLogs retrieves stdout and stderr from a container.
func (e *Executor) getContainerLogs(ctx context.Context, containerID string) (string, error) {
	logOptions := container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     false,
	}

	reader, err := e.client.ContainerLogs(ctx, containerID, logOptions)
	if err != nil {
		return "", fmt.Errorf("failed to get container logs: %w", err)
	}
	defer reader.Close()

	var output strings.Builder
	_, err = stdcopy.StdCopy(&output, &output, reader)
	if err != nil {
		return "", fmt.Errorf("failed to read container logs: %w", err)
	}

	return output.String(), nil
}

// buildDockerCmd constructs the shell command for a container, including optional args.
func buildDockerCmd(opts *executor.RunOptions) []string {
	if !isSafeRelativePath(opts.ScriptPath) {
		// Fail inside container command for consistent UX
		return []string{"/bin/sh", "-c", "echo '❌ ERROR: invalid script path' >&2; exit 126"}
	}
	quotedScript := shellQuote(opts.ScriptPath)

	// Pre-check: verify the script is executable (must be committed with +x in Git).
	// We use test -x instead of chmod +x because /workspace may be mounted read-only.
	shell := fmt.Sprintf(
		"cd /workspace && "+
			"if ! test -x %s; then "+
			"echo '❌ ERROR: script is not executable.' >&2; "+
			"echo '  Fix: chmod +x <script> && git add <script>' >&2; "+
			"echo '  Or:  git update-index --chmod=+x <script>' >&2; "+
			"exit 126; "+
			"fi && ./%s",
		quotedScript,
		quotedScript,
	)

	// Append pass-through args
	if len(opts.Args) > 0 {
		for _, arg := range opts.Args {
			shell += " " + shellQuote(arg)
		}
	}

	// Redirect stdout if specified
	if opts.Stdout != "" {
		if !isSafeRelativePath(opts.Stdout) {
			return []string{"/bin/sh", "-c", "echo '❌ ERROR: invalid stdout path' >&2; exit 126"}
		}
		shell += " > " + shellQuote(opts.Stdout)
	}

	return []string{"/bin/sh", "-c", shell}
}

// buildDockerEnv converts RunOptions.Env to the Docker []string format ("KEY=VALUE").
func buildDockerEnv(opts *executor.RunOptions) []string {
	// Always provide standard DragRace env vars so scripts work in both modes
	env := []string{
		"DRAGRACE_REPO_DIR=/workspace",
		"DRAGRACE_DATA_DIR=/data",
	}
	for k, v := range opts.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

// shellQuote wraps a string in single quotes for safe shell usage.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func isSafeRelativePath(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	return !strings.HasPrefix(clean, "..") && !filepath.IsAbs(clean)
}

// hostCapacity returns the Docker daemon's reported CPU and memory
// capacity. This is what the daemon itself validates NanoCPUs against
// (rejecting anything above host NCPU), so querying it first lets the
// runner catch an unsatisfiable request with its own clear error instead
// of surfacing Docker's raw one.
func (e *Executor) hostCapacity(ctx context.Context) (nanoCPUs int64, memBytes int64, err error) {
	info, err := e.client.Info(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to query Docker host capacity: %w", err)
	}
	if info.NCPU <= 0 {
		return 0, 0, fmt.Errorf("docker host reported no usable CPUs")
	}
	// With a nested daemon (#70) the numbers above describe the machine, not
	// the cgroup job containers actually run in. See capacity.go.
	nanoCPUs, memBytes = clampToOwnCgroup(int64(info.NCPU)*1_000_000_000, info.MemTotal)
	return nanoCPUs, memBytes, nil
}

// resolveResources builds the container.Resources for a phase from its
// baseline defaults, the host's actual capacity, and any explicit
// challenge-supplied request (#58).
func (e *Executor) resolveResources(ctx context.Context, opts *executor.RunOptions, defaultMemory, defaultCPU int64) (container.Resources, error) {
	hostCPU, hostMem, err := e.hostCapacity(ctx)
	if err != nil {
		return container.Resources{}, err
	}
	return clampResourcesToHost(container.Resources{Memory: defaultMemory, NanoCPUs: defaultCPU}, opts, hostCPU, hostMem)
}

// clampResourcesToHost layers opts.Limits on top of the phase defaults and
// fits the result to the host: a default that overshoots is silently
// scaled down (so a modest host can still run a phase that set no
// explicit limits), but an explicit challenge request that overshoots is
// rejected outright rather than handed to Docker to fail on.
func clampResourcesToHost(defaults container.Resources, opts *executor.RunOptions, hostNanoCPUs, hostMemBytes int64) (container.Resources, error) {
	resources := defaults

	if opts.Limits != nil && opts.Limits.CPUNano > 0 {
		if opts.Limits.CPUNano > hostNanoCPUs {
			return container.Resources{}, fmt.Errorf(
				"requested cpu limit (%.2f CPUs) exceeds host capacity (%.2f CPUs)",
				float64(opts.Limits.CPUNano)/1e9, float64(hostNanoCPUs)/1e9)
		}
		resources.NanoCPUs = opts.Limits.CPUNano
	} else if resources.NanoCPUs > hostNanoCPUs {
		resources.NanoCPUs = hostNanoCPUs
	}

	if opts.Limits != nil && opts.Limits.MemoryBytes > 0 {
		if hostMemBytes > 0 && opts.Limits.MemoryBytes > hostMemBytes {
			return container.Resources{}, fmt.Errorf(
				"requested memory limit (%d bytes) exceeds host capacity (%d bytes)",
				opts.Limits.MemoryBytes, hostMemBytes)
		}
		resources.Memory = opts.Limits.MemoryBytes
	} else if hostMemBytes > 0 && resources.Memory > hostMemBytes {
		resources.Memory = hostMemBytes
	}

	return resources, nil
}
