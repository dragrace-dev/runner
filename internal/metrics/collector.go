package metrics

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// Sample represents a single metrics sample at a point in time
type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	
	// CPU
	CPUPercent       float64 `json:"cpu_percent"`
	CPUUserTime      uint64  `json:"cpu_user_time"`
	CPUSystemTime    uint64  `json:"cpu_system_time"`
	
	// Memory
	MemoryUsageMB    float64 `json:"memory_usage_mb"`
	MemoryCacheMB    float64 `json:"memory_cache_mb"`
	MemoryLimitMB    float64 `json:"memory_limit_mb"`
	
	// I/O
	IOReadBytes      uint64  `json:"io_read_bytes"`
	IOWriteBytes     uint64  `json:"io_write_bytes"`
	
	// Network (if enabled)
	NetworkRxBytes   uint64  `json:"network_rx_bytes"`
	NetworkTxBytes   uint64  `json:"network_tx_bytes"`
	
	// System
	PIDs             uint64  `json:"pids"`
}

// TimeSeries contains all samples collected during execution
type TimeSeries struct {
	Samples          []Sample  `json:"samples"`
	SamplingInterval int       `json:"sampling_interval_ms"` // in milliseconds
}

// Aggregates contains computed statistics from time series
type Aggregates struct {
	// Execution
	ExecutionTimeMs  int64   `json:"execution_time_ms"`
	ExitCode         int     `json:"exit_code"`
	
	// CPU
	CPUPercentAvg    float64 `json:"cpu_percent_avg"`
	CPUPercentMax    float64 `json:"cpu_percent_max"`
	CPUUserTimeMs    uint64  `json:"cpu_user_time_ms"`
	CPUSystemTimeMs  uint64  `json:"cpu_system_time_ms"`
	
	// Memory
	MemoryPeakMB     float64 `json:"memory_peak_mb"`
	MemoryAvgMB      float64 `json:"memory_avg_mb"`
	MemoryMinMB      float64 `json:"memory_min_mb"`
	
	// I/O
	IOReadBytesTotal  uint64 `json:"io_read_bytes_total"`
	IOWriteBytesTotal uint64 `json:"io_write_bytes_total"`
	
	// Network
	NetworkRxBytesTotal uint64 `json:"network_rx_bytes_total"`
	NetworkTxBytesTotal uint64 `json:"network_tx_bytes_total"`
}

// RunMetrics contains both time series and aggregated metrics
type RunMetrics struct {
	TimeSeries   TimeSeries    `json:"time_series"`
	Aggregates   Aggregates    `json:"aggregates"`
	GPUTimeSeries *GPUTimeSeries `json:"gpu_time_series,omitempty"` // Optional: only if GPU present
	GPUAggregates *GPUAggregates `json:"gpu_aggregates,omitempty"`  // Optional: only if GPU present
}

// Collector collects metrics from a running container
type Collector struct {
	client           *client.Client
	containerID      string
	samplingInterval time.Duration
	
	mu               sync.Mutex
	samples          []Sample
	startTime        time.Time
	stopChan         chan struct{}
	stopped          bool
}

// NewCollector creates a new metrics collector
func NewCollector(dockerClient *client.Client, containerID string, samplingIntervalMs int) *Collector {
	if samplingIntervalMs <= 0 {
		samplingIntervalMs = 100 // Default: 100ms
	}
	
	return &Collector{
		client:           dockerClient,
		containerID:      containerID,
		samplingInterval: time.Duration(samplingIntervalMs) * time.Millisecond,
		samples:          make([]Sample, 0, 1000), // Pre-allocate
		stopChan:         make(chan struct{}),
	}
}

// Start begins collecting metrics in the background
func (c *Collector) Start(ctx context.Context) {
	c.startTime = time.Now()
	
	go c.collectLoop(ctx)
	
	log.Printf("📊 Metrics collector started (interval: %v)", c.samplingInterval)
}

// Stop stops the collector and returns the final metrics
func (c *Collector) Stop() *RunMetrics {
	c.mu.Lock()
	if !c.stopped {
		close(c.stopChan)
		c.stopped = true
	}
	c.mu.Unlock()
	
	// Wait a bit for last sample
	time.Sleep(50 * time.Millisecond)
	
	executionTime := time.Since(c.startTime).Milliseconds()
	
	log.Printf("📊 Metrics collector stopped (%d samples collected)", len(c.samples))
	
	return &RunMetrics{
		TimeSeries: TimeSeries{
			Samples:          c.samples,
			SamplingInterval: int(c.samplingInterval.Milliseconds()),
		},
		Aggregates: c.computeAggregates(executionTime),
	}
}

// collectLoop periodically collects metrics
func (c *Collector) collectLoop(ctx context.Context) {
	ticker := time.NewTicker(c.samplingInterval)
	defer ticker.Stop()
	
	for {
		select {
		case <-c.stopChan:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.collectSample(ctx); err != nil {
				log.Printf("⚠️  Failed to collect sample: %v", err)
			}
		}
	}
}

// collectSample collects a single metrics sample
func (c *Collector) collectSample(ctx context.Context) error {
	// Get container stats
	stats, err := c.client.ContainerStats(ctx, c.containerID, false)
	if err != nil {
		return err
	}
	defer stats.Body.Close()
	
	var v types.StatsJSON
	if err := json.NewDecoder(stats.Body).Decode(&v); err != nil {
		return err
	}
	
	// Calculate CPU percentage
	cpuPercent := calculateCPUPercent(&v)
	
	// Create sample
	sample := Sample{
		Timestamp:      time.Now(),
		CPUPercent:     cpuPercent,
		CPUUserTime:    v.CPUStats.CPUUsage.UsageInUsermode,
		CPUSystemTime:  v.CPUStats.CPUUsage.UsageInKernelmode,
		MemoryUsageMB:  float64(v.MemoryStats.Usage) / 1024 / 1024,
		MemoryCacheMB:  float64(v.MemoryStats.Stats["cache"]) / 1024 / 1024,
		MemoryLimitMB:  float64(v.MemoryStats.Limit) / 1024 / 1024,
		PIDs:           v.PidsStats.Current,
	}
	
	// I/O stats
	for _, bio := range v.BlkioStats.IoServiceBytesRecursive {
		if bio.Op == "read" || bio.Op == "Read" {
			sample.IOReadBytes += bio.Value
		} else if bio.Op == "write" || bio.Op == "Write" {
			sample.IOWriteBytes += bio.Value
		}
	}
	
	// Network stats (if available)
	for _, netStats := range v.Networks {
		sample.NetworkRxBytes += netStats.RxBytes
		sample.NetworkTxBytes += netStats.TxBytes
	}
	
	// Store sample
	c.mu.Lock()
	c.samples = append(c.samples, sample)
	c.mu.Unlock()
	
	return nil
}

// calculateCPUPercent calculates CPU usage percentage
func calculateCPUPercent(stats *types.StatsJSON) float64 {
	cpuDelta := float64(stats.CPUStats.CPUUsage.TotalUsage - stats.PreCPUStats.CPUUsage.TotalUsage)
	systemDelta := float64(stats.CPUStats.SystemUsage - stats.PreCPUStats.SystemUsage)
	onlineCPUs := float64(stats.CPUStats.OnlineCPUs)
	
	if systemDelta > 0.0 && cpuDelta > 0.0 {
		return (cpuDelta / systemDelta) * onlineCPUs * 100.0
	}
	return 0.0
}

// computeAggregates calculates aggregate statistics from samples
func (c *Collector) computeAggregates(executionTimeMs int64) Aggregates {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	if len(c.samples) == 0 {
		return Aggregates{
			ExecutionTimeMs: executionTimeMs,
		}
	}
	
	agg := Aggregates{
		ExecutionTimeMs: executionTimeMs,
		MemoryMinMB:     c.samples[0].MemoryUsageMB,
	}
	
	var cpuSum float64
	var memSum float64
	
	for _, sample := range c.samples {
		// CPU
		if sample.CPUPercent > agg.CPUPercentMax {
			agg.CPUPercentMax = sample.CPUPercent
		}
		cpuSum += sample.CPUPercent
		
		// Memory
		if sample.MemoryUsageMB > agg.MemoryPeakMB {
			agg.MemoryPeakMB = sample.MemoryUsageMB
		}
		if sample.MemoryUsageMB < agg.MemoryMinMB {
			agg.MemoryMinMB = sample.MemoryUsageMB
		}
		memSum += sample.MemoryUsageMB
	}
	
	// Averages
	sampleCount := float64(len(c.samples))
	agg.CPUPercentAvg = cpuSum / sampleCount
	agg.MemoryAvgMB = memSum / sampleCount
	
	// Use last sample for cumulative values
	lastSample := c.samples[len(c.samples)-1]
	agg.CPUUserTimeMs = lastSample.CPUUserTime / 1_000_000   // ns to ms
	agg.CPUSystemTimeMs = lastSample.CPUSystemTime / 1_000_000
	agg.IOReadBytesTotal = lastSample.IOReadBytes
	agg.IOWriteBytesTotal = lastSample.IOWriteBytes
	agg.NetworkRxBytesTotal = lastSample.NetworkRxBytes
	agg.NetworkTxBytesTotal = lastSample.NetworkTxBytes
	
	return agg
}
