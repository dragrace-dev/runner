package process

import (
	"context"
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

// Compile-time check that Executor implements the interface.
var _ executor.Executor = (*Executor)(nil)

func NewExecutor(baseDataDir string) (*Executor, error) {
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

	output, err := cmd.CombinedOutput()
	logs := string(output)

	if err != nil {
		return logs, fmt.Errorf("script failed: %w", err)
	}

	return logs, nil
}

// RunMeasured executes a script and collects metrics via OS-level tools.
func (e *Executor) RunMeasured(ctx context.Context, opts *executor.RunOptions) (*metrics.RunMetrics, error) {
	log.Printf("🏃 Running script with metrics (process): %s", opts.ScriptPath)

	cmd, err := e.buildCommand(ctx, opts)
	if err != nil {
		return nil, err
	}

	// Capture output
	var outputBuf strings.Builder
	cmd.Stdout = &outputBuf
	cmd.Stderr = &outputBuf

	// If stdout redirect is specified, write to file instead
	if opts.Stdout != "" {
		stdoutPath := filepath.Join(opts.RepoDir, opts.Stdout)
		f, err := os.Create(stdoutPath)
		if err != nil {
			return nil, fmt.Errorf("failed to create stdout file %s: %w", stdoutPath, err)
		}
		defer f.Close()
		cmd.Stdout = f
	}

	startTime := time.Now()

	// Start process
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start process: %w", err)
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
	err = cmd.Wait()
	executionTime := time.Since(startTime)

	// Stop collector
	runMetrics := collector.stop(executionTime)

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
			log.Printf("❌ Run phase exited with code %d: %s", exitCode, outputBuf.String())
			return nil, fmt.Errorf("run script exited with code %d", exitCode)
		}
		return nil, fmt.Errorf("run phase error: %w", err)
	}

	runMetrics.Aggregates.ExitCode = exitCode
	log.Printf("📊 Collected %d samples over %dms", len(runMetrics.TimeSeries.Samples), runMetrics.Aggregates.ExecutionTimeMs)

	return runMetrics, nil
}

// EnsureDataDir creates a local directory as data storage.
func (e *Executor) EnsureDataDir(ctx context.Context, name string) error {
	dir := filepath.Join(e.baseDataDir, name)
	return os.MkdirAll(dir, 0755)
}

// DataDirExists checks if the data directory exists.
func (e *Executor) DataDirExists(ctx context.Context, name string) bool {
	dir := filepath.Join(e.baseDataDir, name)
	info, err := os.Stat(dir)
	return err == nil && info.IsDir()
}

// RemoveDataDir removes the data directory.
func (e *Executor) RemoveDataDir(ctx context.Context, name string) error {
	dir := filepath.Join(e.baseDataDir, name)
	return os.RemoveAll(dir)
}

// buildCommand creates the exec.Cmd for a script execution.
func (e *Executor) buildCommand(ctx context.Context, opts *executor.RunOptions) (*exec.Cmd, error) {
	scriptPath := filepath.Join(opts.RepoDir, opts.ScriptPath)

	// Make script executable
	if err := os.Chmod(scriptPath, 0755); err != nil {
		return nil, fmt.Errorf("failed to chmod script %s: %w", scriptPath, err)
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", fmt.Sprintf("cd %s && ./%s", opts.RepoDir, opts.ScriptPath))

	// Set up environment with data dir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("DRAGRACE_DATA_DIR=%s", filepath.Join(e.baseDataDir, opts.DataDir)),
		fmt.Sprintf("DRAGRACE_REPO_DIR=%s", opts.RepoDir),
	)

	return cmd, nil
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
