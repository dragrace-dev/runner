package metrics

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPUVendor represents the GPU manufacturer
type GPUVendor string

const (
	GPUVendorNVIDIA  GPUVendor = "nvidia"
	GPUVendorAMD     GPUVendor = "amd"
	GPUVendorApple   GPUVendor = "apple"
	GPUVendorUnknown GPUVendor = "unknown"
)

// GPUSample represents GPU metrics at a point in time for a single GPU
type GPUSample struct {
	Timestamp time.Time `json:"timestamp"`
	
	// GPU identification
	DeviceID    int       `json:"device_id"`     // GPU index (0, 1, 2...)
	Vendor      GPUVendor `json:"vendor"`        // nvidia, amd, apple
	DeviceName  string    `json:"device_name"`   // e.g., "NVIDIA GeForce RTX 4090"
	
	// Utilization (0-100%)
	GPUUtilization     float64 `json:"gpu_utilization"`      // GPU core usage
	MemoryUtilization  float64 `json:"memory_utilization"`   // VRAM usage %
	
	// Memory (in MB)
	MemoryUsedMB       float64 `json:"memory_used_mb"`
	MemoryTotalMB      float64 `json:"memory_total_mb"`
	
	// Temperature (Celsius)
	TemperatureC       float64 `json:"temperature_c"`
	
	// Power (Watts)
	PowerUsageW        float64 `json:"power_usage_w"`
	PowerLimitW        float64 `json:"power_limit_w"`
	
	// Clock speeds (MHz)
	ClockSpeedMHz      int     `json:"clock_speed_mhz"`      // GPU core clock
	MemoryClockMHz     int     `json:"memory_clock_mhz"`     // Memory clock
	
	// Compute (for ML workloads)
	ComputeUtilization float64 `json:"compute_utilization"`  // CUDA/ROCm/Metal compute %
}

// GPUTimeSeries contains all GPU samples for all GPUs
type GPUTimeSeries struct {
	Samples          []GPUSample `json:"samples"`
	GPUCount         int         `json:"gpu_count"`
	SamplingInterval int         `json:"sampling_interval_ms"`
}

// GPUAggregates contains computed GPU statistics
type GPUAggregates struct {
	// Per-GPU aggregates (indexed by device_id)
	PerGPU map[int]GPUDeviceAggregates `json:"per_gpu"`
	
	// Overall aggregates (across all GPUs)
	TotalMemoryUsedMB     float64 `json:"total_memory_used_mb"`
	TotalMemoryAvailableMB float64 `json:"total_memory_available_mb"`
	AvgGPUUtilization     float64 `json:"avg_gpu_utilization"`
	MaxGPUUtilization     float64 `json:"max_gpu_utilization"`
	AvgTemperature        float64 `json:"avg_temperature"`
	MaxTemperature        float64 `json:"max_temperature"`
	TotalPowerUsageW      float64 `json:"total_power_usage_w"`
}

// GPUDeviceAggregates contains statistics for a single GPU
type GPUDeviceAggregates struct {
	DeviceID              int       `json:"device_id"`
	DeviceName            string    `json:"device_name"`
	Vendor                GPUVendor `json:"vendor"`
	
	GPUUtilizationAvg     float64   `json:"gpu_utilization_avg"`
	GPUUtilizationMax     float64   `json:"gpu_utilization_max"`
	
	MemoryUsedAvgMB       float64   `json:"memory_used_avg_mb"`
	MemoryUsedMaxMB       float64   `json:"memory_used_max_mb"`
	MemoryTotalMB         float64   `json:"memory_total_mb"`
	
	TemperatureAvgC       float64   `json:"temperature_avg_c"`
	TemperatureMaxC       float64   `json:"temperature_max_c"`
	
	PowerUsageAvgW        float64   `json:"power_usage_avg_w"`
	PowerUsageMaxW        float64   `json:"power_usage_max_w"`
}

// GPUCollector collects GPU metrics
type GPUCollector struct {
	vendor           GPUVendor
	samplingInterval time.Duration
	samples          []GPUSample
	stopChan         chan struct{}
	stopped          bool
}

// NewGPUCollector creates a GPU metrics collector
func NewGPUCollector(samplingIntervalMs int) *GPUCollector {
	if samplingIntervalMs <= 0 {
		samplingIntervalMs = 100
	}
	
	// Detect GPU vendor
	vendor := detectGPUVendor()
	
	return &GPUCollector{
		vendor:           vendor,
		samplingInterval: time.Duration(samplingIntervalMs) * time.Millisecond,
		samples:          make([]GPUSample, 0, 1000),
		stopChan:         make(chan struct{}),
	}
}

// Start begins collecting GPU metrics
func (c *GPUCollector) Start(ctx context.Context) {
	if c.vendor == GPUVendorUnknown {
		log.Println("⚠️  No GPU detected or unsupported GPU vendor")
		return
	}
	
	go c.collectLoop(ctx)
	log.Printf("🎮 GPU metrics collector started (vendor: %s, interval: %v)", c.vendor, c.samplingInterval)
}

// Stop stops collection and returns metrics
func (c *GPUCollector) Stop() *GPUTimeSeries {
	if !c.stopped {
		close(c.stopChan)
		c.stopped = true
	}
	
	time.Sleep(50 * time.Millisecond)
	
	gpuCount := c.getGPUCount()
	
	return &GPUTimeSeries{
		Samples:          c.samples,
		GPUCount:         gpuCount,
		SamplingInterval: int(c.samplingInterval.Milliseconds()),
	}
}

// collectLoop periodically collects GPU metrics
func (c *GPUCollector) collectLoop(ctx context.Context) {
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
				log.Printf("⚠️  Failed to collect GPU sample: %v", err)
			}
		}
	}
}

// collectSample collects GPU metrics based on vendor
func (c *GPUCollector) collectSample() error {
	var samples []GPUSample
	var err error
	
	switch c.vendor {
	case GPUVendorNVIDIA:
		samples, err = c.collectNVIDIA()
	case GPUVendorAMD:
		samples, err = c.collectAMD()
	case GPUVendorApple:
		samples, err = c.collectApple()
	default:
		return fmt.Errorf("unsupported GPU vendor: %s", c.vendor)
	}
	
	if err != nil {
		return err
	}
	
	c.samples = append(c.samples, samples...)
	return nil
}

// collectNVIDIA collects metrics from NVIDIA GPUs using nvidia-smi
func (c *GPUCollector) collectNVIDIA() ([]GPUSample, error) {
	// nvidia-smi --query-gpu=index,name,utilization.gpu,utilization.memory,memory.used,memory.total,temperature.gpu,power.draw,power.limit,clocks.gr,clocks.mem --format=csv,noheader,nounits
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,name,utilization.gpu,utilization.memory,memory.used,memory.total,temperature.gpu,power.draw,power.limit,clocks.gr,clocks.mem",
		"--format=csv,noheader,nounits",
	)
	
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	samples := make([]GPUSample, 0, len(lines))
	now := time.Now()
	
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 11 {
			continue
		}
		
		sample := GPUSample{
			Timestamp:  now,
			Vendor:     GPUVendorNVIDIA,
		}
		
		// Parse fields
		sample.DeviceID, _ = strconv.Atoi(strings.TrimSpace(fields[0]))
		sample.DeviceName = strings.TrimSpace(fields[1])
		sample.GPUUtilization, _ = strconv.ParseFloat(strings.TrimSpace(fields[2]), 64)
		sample.MemoryUtilization, _ = strconv.ParseFloat(strings.TrimSpace(fields[3]), 64)
		sample.MemoryUsedMB, _ = strconv.ParseFloat(strings.TrimSpace(fields[4]), 64)
		sample.MemoryTotalMB, _ = strconv.ParseFloat(strings.TrimSpace(fields[5]), 64)
		sample.TemperatureC, _ = strconv.ParseFloat(strings.TrimSpace(fields[6]), 64)
		sample.PowerUsageW, _ = strconv.ParseFloat(strings.TrimSpace(fields[7]), 64)
		sample.PowerLimitW, _ = strconv.ParseFloat(strings.TrimSpace(fields[8]), 64)
		sample.ClockSpeedMHz, _ = strconv.Atoi(strings.TrimSpace(fields[9]))
		sample.MemoryClockMHz, _ = strconv.Atoi(strings.TrimSpace(fields[10]))
		sample.ComputeUtilization = sample.GPUUtilization // Approximation
		
		samples = append(samples, sample)
	}
	
	return samples, nil
}

// collectAMD collects metrics from AMD GPUs using rocm-smi
func (c *GPUCollector) collectAMD() ([]GPUSample, error) {
	// rocm-smi --showid --showproductname --showuse --showmemuse --showtemp --showpower
	cmd := exec.Command("rocm-smi", "--json")
	
	_, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("rocm-smi failed: %w", err)
	}
	
	// TODO: Parse JSON output from rocm-smi
	// For now, return placeholder
	log.Println("⚠️  AMD GPU metrics parsing not fully implemented")
	
	now := time.Now()
	return []GPUSample{
		{
			Timestamp:  now,
			DeviceID:   0,
			Vendor:     GPUVendorAMD,
			DeviceName: "AMD GPU (placeholder)",
		},
	}, nil
}

// collectApple collects metrics from Apple Silicon GPUs
func (c *GPUCollector) collectApple() ([]GPUSample, error) {
	// Apple Silicon: use powermetrics or ioreg
	// Note: This requires root/sudo access
	cmd := exec.Command("ioreg", "-r", "-c", "IOAccelerator")
	
	_, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ioreg failed: %w", err)
	}
	
	// TODO: Parse ioreg output for GPU metrics
	// For now, return placeholder
	log.Println("⚠️  Apple GPU metrics parsing not fully implemented")
	
	now := time.Now()
	return []GPUSample{
		{
			Timestamp:  now,
			DeviceID:   0,
			Vendor:     GPUVendorApple,
			DeviceName: "Apple Silicon GPU (placeholder)",
		},
	}, nil
}

// detectGPUVendor detects which GPU vendor is present
func detectGPUVendor() GPUVendor {
	// Try NVIDIA
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		return GPUVendorNVIDIA
	}
	
	// Try AMD
	if _, err := exec.LookPath("rocm-smi"); err == nil {
		return GPUVendorAMD
	}
	
	// Try Apple Silicon (check if running on macOS with Apple Silicon)
	if _, err := exec.LookPath("ioreg"); err == nil {
		// Additional check for Apple Silicon
		cmd := exec.Command("sysctl", "-n", "machdep.cpu.brand_string")
		if output, err := cmd.Output(); err == nil {
			if strings.Contains(string(output), "Apple") {
				return GPUVendorApple
			}
		}
	}
	
	return GPUVendorUnknown
}

// getGPUCount returns the number of GPUs detected
func (c *GPUCollector) getGPUCount() int {
	deviceIDs := make(map[int]bool)
	for _, sample := range c.samples {
		deviceIDs[sample.DeviceID] = true
	}
	return len(deviceIDs)
}

// ComputeGPUAggregates calculates aggregate GPU statistics
func ComputeGPUAggregates(timeSeries *GPUTimeSeries) *GPUAggregates {
	if len(timeSeries.Samples) == 0 {
		return &GPUAggregates{
			PerGPU: make(map[int]GPUDeviceAggregates),
		}
	}
	
	// Group samples by device ID
	perDevice := make(map[int][]GPUSample)
	for _, sample := range timeSeries.Samples {
		perDevice[sample.DeviceID] = append(perDevice[sample.DeviceID], sample)
	}
	
	agg := &GPUAggregates{
		PerGPU: make(map[int]GPUDeviceAggregates),
	}
	
	// Compute per-device aggregates
	for deviceID, samples := range perDevice {
		deviceAgg := computeDeviceAggregates(deviceID, samples)
		agg.PerGPU[deviceID] = deviceAgg
		
		// Add to overall aggregates
		agg.TotalMemoryUsedMB += deviceAgg.MemoryUsedMaxMB
		agg.TotalMemoryAvailableMB += deviceAgg.MemoryTotalMB
		agg.AvgGPUUtilization += deviceAgg.GPUUtilizationAvg
		if deviceAgg.GPUUtilizationMax > agg.MaxGPUUtilization {
			agg.MaxGPUUtilization = deviceAgg.GPUUtilizationMax
		}
		agg.AvgTemperature += deviceAgg.TemperatureAvgC
		if deviceAgg.TemperatureMaxC > agg.MaxTemperature {
			agg.MaxTemperature = deviceAgg.TemperatureMaxC
		}
		agg.TotalPowerUsageW += deviceAgg.PowerUsageAvgW
	}
	
	// Average across GPUs
	gpuCount := float64(len(perDevice))
	if gpuCount > 0 {
		agg.AvgGPUUtilization /= gpuCount
		agg.AvgTemperature /= gpuCount
	}
	
	return agg
}

// computeDeviceAggregates computes aggregates for a single GPU
func computeDeviceAggregates(deviceID int, samples []GPUSample) GPUDeviceAggregates {
	if len(samples) == 0 {
		return GPUDeviceAggregates{DeviceID: deviceID}
	}
	
	agg := GPUDeviceAggregates{
		DeviceID:   deviceID,
		DeviceName: samples[0].DeviceName,
		Vendor:     samples[0].Vendor,
		MemoryTotalMB: samples[0].MemoryTotalMB,
	}
	
	var gpuUtilSum, memUsedSum, tempSum, powerSum float64
	
	for _, sample := range samples {
		// GPU utilization
		if sample.GPUUtilization > agg.GPUUtilizationMax {
			agg.GPUUtilizationMax = sample.GPUUtilization
		}
		gpuUtilSum += sample.GPUUtilization
		
		// Memory
		if sample.MemoryUsedMB > agg.MemoryUsedMaxMB {
			agg.MemoryUsedMaxMB = sample.MemoryUsedMB
		}
		memUsedSum += sample.MemoryUsedMB
		
		// Temperature
		if sample.TemperatureC > agg.TemperatureMaxC {
			agg.TemperatureMaxC = sample.TemperatureC
		}
		tempSum += sample.TemperatureC
		
		// Power
		if sample.PowerUsageW > agg.PowerUsageMaxW {
			agg.PowerUsageMaxW = sample.PowerUsageW
		}
		powerSum += sample.PowerUsageW
	}
	
	// Compute averages
	sampleCount := float64(len(samples))
	agg.GPUUtilizationAvg = gpuUtilSum / sampleCount
	agg.MemoryUsedAvgMB = memUsedSum / sampleCount
	agg.TemperatureAvgC = tempSum / sampleCount
	agg.PowerUsageAvgW = powerSum / sampleCount
	
	return agg
}
