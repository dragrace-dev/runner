package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"

	"dragrace/internal/executor"
	"dragrace/internal/metrics"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// Executor implements executor.Executor using Docker containers.
type Executor struct {
	client *client.Client
}

// Compile-time check that Executor implements the interface.
var _ executor.Executor = (*Executor)(nil)

func NewExecutor(dockerHost string) (*Executor, error) {
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

	log.Println("✅ Docker client initialized")

	return &Executor{
		client: cli,
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
	binds := []string{
		fmt.Sprintf("%s:/workspace:ro", opts.RepoDir),
	}

	if opts.DataDir != "" {
		mode := "rw"
		if opts.ReadOnlyData {
			mode = "ro"
		}
		binds = append(binds, fmt.Sprintf("%s:/data:%s", opts.DataDir, mode))
	}

	// Configure resources
	resources := container.Resources{
		Memory:   512 * 1024 * 1024, // Default 512MB
		NanoCPUs: 1000000000,        // Default 1 CPU
	}
	if opts.Limits != nil {
		if opts.Limits.MemoryBytes > 0 {
			resources.Memory = opts.Limits.MemoryBytes
		}
		if opts.Limits.CPUNano > 0 {
			resources.NanoCPUs = opts.Limits.CPUNano
		}
	}

	// Build env vars
	env := buildDockerEnv(opts)

	// Create container
	resp, err := e.client.ContainerCreate(ctx, &container.Config{
		Image:      opts.Image,
		Cmd:        cmd,
		Env:        env,
		Tty:        false,
		WorkingDir: "/workspace",
	}, &container.HostConfig{
		Binds:       binds,
		Resources:   resources,
		NetworkMode: "bridge", // Network enabled for init/build/validate (download deps, data, etc.)
	}, nil, nil, "")
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
func (e *Executor) RunMeasured(ctx context.Context, opts *executor.RunOptions) (*metrics.RunMetrics, error) {
	log.Printf("🏃 Running script with metrics: %s in %s", opts.ScriptPath, opts.Image)

	// Pull image
	reader, err := e.client.ImagePull(ctx, opts.Image, image.PullOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to pull image %s: %w", opts.Image, err)
	}
	defer reader.Close()
	io.Copy(io.Discard, reader)

	// Build command with optional args
	cmd := buildDockerCmd(opts)

	// Configure mounts
	binds := []string{
		fmt.Sprintf("%s:/workspace:ro", opts.RepoDir),
	}
	if opts.DataDir != "" {
		mode := "rw"
		if opts.ReadOnlyData {
			mode = "ro"
		}
		binds = append(binds, fmt.Sprintf("%s:/data:%s", opts.DataDir, mode))
	}

	// Configure resources
	resources := container.Resources{
		Memory:   32 * 1024 * 1024 * 1024, // 32GB default for run phase
		NanoCPUs: 8 * 1000000000,          // 8 CPUs default
	}
	if opts.Limits != nil {
		if opts.Limits.MemoryBytes > 0 {
			resources.Memory = opts.Limits.MemoryBytes
		}
		if opts.Limits.CPUNano > 0 {
			resources.NanoCPUs = opts.Limits.CPUNano
		}
	}

	// Build env vars
	env := buildDockerEnv(opts)

	// Create container
	resp, err := e.client.ContainerCreate(ctx, &container.Config{
		Image:      opts.Image,
		Cmd:        cmd,
		Env:        env,
		Tty:        false,
		WorkingDir: "/workspace",
	}, &container.HostConfig{
		Binds:       binds,
		Resources:   resources,
		NetworkMode: "none", // Network disabled for measured run (fairness + security)
	}, nil, nil, "")
	if err != nil {
		return nil, fmt.Errorf("failed to create container: %w", err)
	}

	containerID := resp.ID
	log.Printf("   Container created: %s (measuring metrics)", containerID[:12])

	// Start container
	if err := e.client.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		e.client.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("failed to start container: %w", err)
	}

	// Start metrics collector
	collector := metrics.NewCollector(e.client, containerID, 100)
	collector.Start(ctx)

	// Wait for container to finish
	exitCode, logs, err := e.waitContainer(ctx, containerID)

	// Stop metrics collector
	runMetrics := collector.Stop()

	// Cleanup container
	if cleanupErr := e.client.ContainerRemove(context.Background(), containerID, container.RemoveOptions{Force: true}); cleanupErr != nil {
		log.Printf("⚠️  Failed to remove container: %v", cleanupErr)
	}

	if err != nil {
		log.Printf("❌ Run phase error: %s", logs)
		return nil, err
	}

	if exitCode != 0 {
		log.Printf("❌ Run phase exited with code %d: %s", exitCode, logs)
		return nil, fmt.Errorf("run script exited with code %d", exitCode)
	}

	runMetrics.Aggregates.ExitCode = int(exitCode)
	log.Printf("📊 Collected %d samples over %dms", len(runMetrics.TimeSeries.Samples), runMetrics.Aggregates.ExecutionTimeMs)

	return runMetrics, nil
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

	output, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("failed to read container logs: %w", err)
	}

	return string(output), nil
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
