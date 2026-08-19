package process

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"dragrace/internal/executor"
	"dragrace/internal/metrics"
)

// Executor implements executor.Executor using native OS processes.
// No Docker required — scripts run directly on the host.
type Executor struct {
	baseDataDir string // Root dir for data volumes (e.g. /var/dragrace/data)
}

type LimitKind string

const (
	LimitTimeout       LimitKind = "timeout"
	LimitMemory        LimitKind = "memory"
	LimitCPU           LimitKind = "cpu"
	LimitProcesses     LimitKind = "processes"
	LimitFilesystem    LimitKind = "filesystem"
	defaultPids                  = 64
	defaultBuildMemory           = 512 * 1024 * 1024
	defaultBuildCPU              = 1_000_000_000
	defaultBuildDisk             = 10 * 1024 * 1024 * 1024
)

type LimitError struct {
	Kind LimitKind
}

func (e *LimitError) Error() string { return fmt.Sprintf("native executor %s limit exceeded", e.Kind) }

// Compile-time check that Executor implements the interface.
var _ executor.Executor = (*Executor)(nil)

func NewExecutor(baseDataDir string) (*Executor, error) {
	if err := validateProcessControls(); err != nil {
		return nil, err
	}

	if baseDataDir == "" {
		baseDataDir = "/var/dragrace/data"
	}

	// Ensure base data dir exists
	if err := os.MkdirAll(baseDataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data dir %s: %w", baseDataDir, err)
	}

	log.Printf("✅ Process executor initialized (data dir: %s)", baseDataDir)
	return &Executor{baseDataDir: baseDataDir}, nil
}

func (e *Executor) Close() error {
	return nil
}

// RunScript executes a script as a native process and waits for completion.
func (e *Executor) RunScript(ctx context.Context, opts *executor.RunOptions) (string, error) {
	log.Printf("🏃 Running script (process): %s", opts.ScriptPath)

	cmd, err := e.buildCommand(ctx, opts)
	if err != nil {
		return "", err
	}

	var outputBuf strings.Builder
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("failed to start process: %w", err)
	}
	err = waitForCommand(ctx, cmd, opts.Limits)
	logs := outputBuf.String()

	if err != nil {
		var limitErr *LimitError
		if errors.As(err, &limitErr) {
			return logs, limitErr
		}
		return logs, fmt.Errorf("script failed: %w", err)
	}

	return logs, nil
}

// RunMeasured executes a script and collects metrics via OS-level tools.
func (e *Executor) RunMeasured(ctx context.Context, opts *executor.RunOptions) (*metrics.RunMetrics, string, error) {
	log.Printf("🏃 Running script with metrics (process): %s", opts.ScriptPath)

	cmd, err := e.buildCommand(ctx, opts)
	if err != nil {
		return nil, "", err
	}

	// Capture output
	var outputBuf strings.Builder
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	// If stdout redirect is specified, write to file instead
	if opts.Stdout != "" {
		stdoutPath, err := safeWritablePath(opts.RepoDir, opts.Stdout)
		if err != nil {
			return nil, "", fmt.Errorf("invalid stdout file: %w", err)
		}
		f, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return nil, "", fmt.Errorf("failed to create stdout file %s: %w", stdoutPath, err)
		}
		defer f.Close()
		cmd.Stdout = f
	}

	startTime := time.Now()

	// Start process
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("failed to start process: %w", err)
	}

	pid := cmd.Process.Pid

	// Start metrics collection in background
	collector := &processCollector{
		pid:              pid,
		samplingInterval: 100 * time.Millisecond,
		samples:          make([]metrics.Sample, 0, 1000),
		stopChan:         make(chan struct{}),
	}
	collector.start(ctx)

	// Wait for process
	err = waitForCommand(ctx, cmd, opts.Limits)
	executionTime := time.Since(startTime)

	// Stop collector
	runMetrics := collector.stop(executionTime)

	exitCode := 0
	if err != nil {
		var limitErr *LimitError
		if errors.As(err, &limitErr) {
			// A classified overrun still produced output; #23 persists the logs
			// of every phase outcome, so hand them back like the paths below.
			return nil, outputBuf.String(), limitErr
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			log.Printf("❌ Run phase exited with code %d: %s", exitCode, outputBuf.String())
			return nil, outputBuf.String(), fmt.Errorf("run script exited with code %d", exitCode)
		}
		return nil, outputBuf.String(), fmt.Errorf("run phase error: %w", err)
	}

	runMetrics.Aggregates.ExitCode = exitCode
	log.Printf("📊 Collected %d samples over %dms", len(runMetrics.TimeSeries.Samples), runMetrics.Aggregates.ExecutionTimeMs)

	return runMetrics, outputBuf.String(), nil
}

// EnsureDataDir creates a local directory as data storage.
func (e *Executor) EnsureDataDir(ctx context.Context, name string) error {
	if !isSafeDataDirName(name) {
		return fmt.Errorf("invalid data directory name: %s", name)
	}
	dir := filepath.Join(e.baseDataDir, name)
	return os.MkdirAll(dir, 0755)
}

// DataDirExists checks if the data directory exists.
func (e *Executor) DataDirExists(ctx context.Context, name string) bool {
	if !isSafeDataDirName(name) {
		return false
	}
	dir := filepath.Join(e.baseDataDir, name)
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// RemoveDataDir removes the data directory.
func (e *Executor) RemoveDataDir(ctx context.Context, name string) error {
	if !isSafeDataDirName(name) {
		return fmt.Errorf("invalid data directory name: %s", name)
	}
	dir := filepath.Join(e.baseDataDir, name)
	return os.RemoveAll(dir)
}

// buildCommand creates the exec.Cmd for a script execution.
func (e *Executor) buildCommand(ctx context.Context, opts *executor.RunOptions) (*exec.Cmd, error) {
	if !isSafeRelativePath(opts.ScriptPath) {
		return nil, fmt.Errorf("invalid script path: %s", opts.ScriptPath)
	}
	repoDir, err := filepath.EvalSymlinks(opts.RepoDir)
	if err != nil {
		return nil, fmt.Errorf("invalid repository directory: %w", err)
	}
	scriptPath, err := filepath.EvalSymlinks(filepath.Join(repoDir, opts.ScriptPath))
	if err != nil {
		return nil, fmt.Errorf("script not found: %s", opts.ScriptPath)
	}
	relScript, err := filepath.Rel(repoDir, scriptPath)
	if err != nil || !isSafeRelativePath(relScript) {
		return nil, fmt.Errorf("script escapes repository: %s", opts.ScriptPath)
	}

	// Verify script is executable (must be committed with +x in Git)
	info, err := os.Stat(scriptPath)
	if err != nil {
		return nil, fmt.Errorf("script not found: %s", scriptPath)
	}
	if info.Mode()&0111 == 0 {
		return nil, fmt.Errorf(
			"script %s is not executable.\n"+
				"  Fix: chmod +x %s && git add %s\n"+
				"  Or:  git update-index --chmod=+x %s",
			opts.ScriptPath, opts.ScriptPath, opts.ScriptPath, opts.ScriptPath,
		)
	}

	// Build shell command with optional args
	fileBlocks := (diskLimitBytes(opts.Limits) + 511) / 512
	shell := fmt.Sprintf("ulimit -f %d && cd %s && exec ./%s", fileBlocks, shellQuote(repoDir), shellQuote(relScript))
	if len(opts.Args) > 0 {
		for _, arg := range opts.Args {
			shell += " " + shellQuote(arg)
		}
	}

	cmd := exec.Command("/bin/sh", "-c", shell)
	configureProcessGroup(cmd)

	homeDir := filepath.Join(repoDir, ".dragrace-home")
	tmpDir := filepath.Join(repoDir, ".dragrace-tmp")
	if err := os.MkdirAll(homeDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create isolated HOME: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create isolated TMPDIR: %w", err)
	}

	// Do not expose runner credentials or service secrets to challenge code.
	cmd.Env = append(safeHostEnvironment(),
		fmt.Sprintf("DRAGRACE_DATA_DIR=%s", filepath.Join(e.baseDataDir, opts.DataDir)),
		fmt.Sprintf("DRAGRACE_REPO_DIR=%s", repoDir),
		fmt.Sprintf("HOME=%s", homeDir),
		fmt.Sprintf("TMPDIR=%s", tmpDir),
	)

	// Add extra env vars from options
	for k, v := range opts.Env {
		if strings.Contains(k, "=") || k == "" {
			return nil, fmt.Errorf("invalid environment variable name: %q", k)
		}
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	return cmd, nil
}

func safeHostEnvironment() []string {
	allowed := []string{"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}
	env := make([]string, 0, len(allowed))
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func waitForCommand(ctx context.Context, cmd *exec.Cmd, limits *executor.ResourceLimits) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	violations, stopMonitor := monitorProcessGroup(cmd.Process.Pid, limits)
	defer stopMonitor()

	select {
	case err := <-done:
		if limitErr := classifyFilesystemExit(err, diskLimitBytes(limits) > 0); limitErr != nil {
			return limitErr
		}
		return err
	case violation := <-violations:
		terminateProcessGroup(cmd.Process.Pid)
		<-done
		return violation
	case <-ctx.Done():
		terminateProcessGroup(cmd.Process.Pid)
		<-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &LimitError{Kind: LimitTimeout}
		}
		return ctx.Err()
	}
}

func monitorProcessGroup(pid int, limits *executor.ResourceLimits) (<-chan *LimitError, func()) {
	violations := make(chan *LimitError, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopMonitor := func() { stopOnce.Do(func() { close(stop) }) }
	if limits == nil {
		limits = &executor.ResourceLimits{
			MemoryBytes: defaultBuildMemory,
			CPUNano:     defaultBuildCPU,
			DiskBytes:   defaultBuildDisk,
			PidsLimit:   defaultPids,
		}
	}

	pidsLimit := limits.PidsLimit
	if pidsLimit == 0 {
		pidsLimit = defaultPids
	}
	go func() {
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		consecutiveCPU := 0
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
			usage, err := processGroupUsage(pid)
			if err != nil || usage.Processes == 0 {
				continue
			}
			if limits.MemoryBytes > 0 && usage.MemoryBytes > limits.MemoryBytes {
				violations <- &LimitError{Kind: LimitMemory}
				return
			}
			if pidsLimit > 0 && usage.Processes > pidsLimit {
				violations <- &LimitError{Kind: LimitProcesses}
				return
			}
			if limits.CPUNano > 0 {
				allowedPercent := float64(limits.CPUNano) / 1_000_000_000 * 100
				if usage.CPUPercent > allowedPercent*1.25 {
					consecutiveCPU++
				} else {
					consecutiveCPU = 0
				}
				if consecutiveCPU >= 8 {
					violations <- &LimitError{Kind: LimitCPU}
					return
				}
			}
		}
	}()
	return violations, stopMonitor
}

func diskLimitBytes(limits *executor.ResourceLimits) int64 {
	if limits != nil && limits.DiskBytes > 0 {
		return limits.DiskBytes
	}
	return defaultBuildDisk
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

func isSafeDataDirName(name string) bool {
	return isSafeRelativePath(name) && filepath.Base(name) == name
}

func safeWritablePath(repoDir, relativePath string) (string, error) {
	if !isSafeRelativePath(relativePath) {
		return "", fmt.Errorf("path must stay inside repository")
	}
	repo, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(repo, filepath.Clean(relativePath))
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(repo, parent)
	if err != nil || (!isSafeRelativePath(rel) && rel != ".") {
		return "", fmt.Errorf("parent directory escapes repository")
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("symbolic-link output is forbidden")
	}
	return target, nil
}

// ── Process Metrics Collector ──────────────────────────────────────────────

type processCollector struct {
	pid              int
	samplingInterval time.Duration
	mu               sync.Mutex
	samples          []metrics.Sample
	stopChan         chan struct{}
	stopped          bool
}

func (c *processCollector) start(ctx context.Context) {
	go c.collectLoop(ctx)
	log.Printf("📊 Process metrics collector started (pid: %d, interval: %v)", c.pid, c.samplingInterval)
}

func (c *processCollector) stop(executionTime time.Duration) *metrics.RunMetrics {
	c.mu.Lock()
	if !c.stopped {
		close(c.stopChan)
		c.stopped = true
	}
	c.mu.Unlock()

	time.Sleep(50 * time.Millisecond) // Wait for last sample

	execMs := executionTime.Milliseconds()
	log.Printf("📊 Process metrics collector stopped (%d samples collected)", len(c.samples))

	return &metrics.RunMetrics{
		TimeSeries: metrics.TimeSeries{
			Samples:          c.samples,
			SamplingInterval: int(c.samplingInterval.Milliseconds()),
		},
		Aggregates: c.computeAggregates(execMs),
	}
}

func (c *processCollector) collectLoop(ctx context.Context) {
	ticker := time.NewTicker(c.samplingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.stopChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.collectSample(); err != nil {
				// Process may have exited, stop collecting
				return
			}
		}
	}
}

func (c *processCollector) collectSample() error {
	sample := metrics.Sample{
		Timestamp: time.Now(),
	}

	switch runtime.GOOS {
	case "linux":
		c.collectLinuxSample(&sample)
	case "darwin":
		c.collectDarwinSample(&sample)
	default:
		// Minimal: just record timestamp
	}

	c.mu.Lock()
	c.samples = append(c.samples, sample)
	c.mu.Unlock()

	return nil
}

// collectLinuxSample reads metrics from /proc/[pid]/stat and /proc/[pid]/status.
func (c *processCollector) collectLinuxSample(sample *metrics.Sample) {
	// Read /proc/[pid]/stat for CPU info
	statPath := fmt.Sprintf("/proc/%d/stat", c.pid)
	data, err := os.ReadFile(statPath)
	if err != nil {
		return
	}

	fields := strings.Fields(string(data))
	if len(fields) >= 23 {
		utime, _ := strconv.ParseUint(fields[13], 10, 64)
		stime, _ := strconv.ParseUint(fields[14], 10, 64)
		sample.CPUUserTime = utime * 10_000_000 // clock ticks to ns (assuming 100 Hz)
		sample.CPUSystemTime = stime * 10_000_000
	}

	// Read /proc/[pid]/status for memory (VmRSS)
	statusPath := fmt.Sprintf("/proc/%d/status", c.pid)
	statusData, err := os.ReadFile(statusPath)
	if err != nil {
		return
	}

	for _, line := range strings.Split(string(statusData), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				kbytes, _ := strconv.ParseFloat(parts[1], 64)
				sample.MemoryUsageMB = kbytes / 1024.0
			}
		}
	}
}

// collectDarwinSample uses ps to get process metrics on macOS.
func (c *processCollector) collectDarwinSample(sample *metrics.Sample) {
	// Use ps to get RSS and CPU for the process
	out, err := exec.Command("ps", "-p", strconv.Itoa(c.pid), "-o", "rss=,pcpu=").Output()
	if err != nil {
		return
	}

	fields := strings.Fields(string(out))
	if len(fields) >= 2 {
		rssKB, _ := strconv.ParseFloat(fields[0], 64)
		cpuPct, _ := strconv.ParseFloat(fields[1], 64)
		sample.MemoryUsageMB = rssKB / 1024.0
		sample.CPUPercent = cpuPct
	}
}

func (c *processCollector) computeAggregates(executionTimeMs int64) metrics.Aggregates {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.samples) == 0 {
		return metrics.Aggregates{
			ExecutionTimeMs: executionTimeMs,
		}
	}

	agg := metrics.Aggregates{
		ExecutionTimeMs: executionTimeMs,
		MemoryMinMB:     c.samples[0].MemoryUsageMB,
	}

	var cpuSum, memSum float64

	for _, s := range c.samples {
		if s.CPUPercent > agg.CPUPercentMax {
			agg.CPUPercentMax = s.CPUPercent
		}
		cpuSum += s.CPUPercent

		if s.MemoryUsageMB > agg.MemoryPeakMB {
			agg.MemoryPeakMB = s.MemoryUsageMB
		}
		if s.MemoryUsageMB < agg.MemoryMinMB {
			agg.MemoryMinMB = s.MemoryUsageMB
		}
		memSum += s.MemoryUsageMB
	}

	n := float64(len(c.samples))
	agg.CPUPercentAvg = cpuSum / n
	agg.MemoryAvgMB = memSum / n

	// Cumulative from last sample
	last := c.samples[len(c.samples)-1]
	agg.CPUUserTimeMs = last.CPUUserTime / 1_000_000
	agg.CPUSystemTimeMs = last.CPUSystemTime / 1_000_000
	agg.IOReadBytesTotal = last.IOReadBytes
	agg.IOWriteBytesTotal = last.IOWriteBytes

	return agg
}
