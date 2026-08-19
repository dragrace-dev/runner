package metrics

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"sort"
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
	DeviceID   int       `json:"device_id"`   // GPU index (0, 1, 2...)
	Vendor     GPUVendor `json:"vendor"`      // nvidia, amd, apple
	DeviceName string    `json:"device_name"` // e.g., "NVIDIA GeForce RTX 4090"

	// Utilization (0-100%)
	GPUUtilization    float64 `json:"gpu_utilization"`    // GPU core usage
	MemoryUtilization float64 `json:"memory_utilization"` // VRAM usage %

	// Memory (in MB)
	MemoryUsedMB  float64 `json:"memory_used_mb"`
	MemoryTotalMB float64 `json:"memory_total_mb"`

	// Temperature (Celsius)
	TemperatureC float64 `json:"temperature_c"`

	// Power (Watts)
	PowerUsageW float64 `json:"power_usage_w"`
	PowerLimitW float64 `json:"power_limit_w"`

	// Clock speeds (MHz)
	ClockSpeedMHz  int `json:"clock_speed_mhz"`  // GPU core clock
	MemoryClockMHz int `json:"memory_clock_mhz"` // Memory clock

	// Compute (for ML workloads)
	ComputeUtilization float64 `json:"compute_utilization"` // CUDA/ROCm/Metal compute %
}

// GPUTimeSeries contains all GPU samples for all GPUs
type GPUTimeSeries struct {
	Samples          []GPUSample `json:"samples"`
	GPUCount         int         `json:"gpu_count"`
	SamplingInterval int         `json:"sampling_interval_ms"`
}

// GPUAggregates contains computed GPU statistics
type GPUAggregates struct {
	// Per-GPU aggregates, keyed "vendor:device_id". A plain device id would
	// collide on a machine holding both an NVIDIA and an AMD card, which both
	// number their devices from zero.
	PerGPU map[string]GPUDeviceAggregates `json:"per_gpu"`

	// MeasuredDevices lists, sorted, the same "vendor:device_id" keys as
	// PerGPU. It exists so a consumer of the result can verify which GPUs
	// were measured explicitly (#67), rather than inferring it from map
	// membership.
	MeasuredDevices []string `json:"measured_devices"`

	// Overall aggregates (across all GPUs)
	TotalMemoryUsedMB      float64 `json:"total_memory_used_mb"`
	TotalMemoryAvailableMB float64 `json:"total_memory_available_mb"`
	AvgGPUUtilization      float64 `json:"avg_gpu_utilization"`
	MaxGPUUtilization      float64 `json:"max_gpu_utilization"`
	AvgTemperature         float64 `json:"avg_temperature"`
	MaxTemperature         float64 `json:"max_temperature"`
	TotalPowerUsageW       float64 `json:"total_power_usage_w"`
}

// GPUDeviceAggregates contains statistics for a single GPU
type GPUDeviceAggregates struct {
	DeviceID   int       `json:"device_id"`
	DeviceName string    `json:"device_name"`
	Vendor     GPUVendor `json:"vendor"`

	GPUUtilizationAvg float64 `json:"gpu_utilization_avg"`
	GPUUtilizationMax float64 `json:"gpu_utilization_max"`

	MemoryUsedAvgMB float64 `json:"memory_used_avg_mb"`
	MemoryUsedMaxMB float64 `json:"memory_used_max_mb"`
	MemoryTotalMB   float64 `json:"memory_total_mb"`

	TemperatureAvgC float64 `json:"temperature_avg_c"`
	TemperatureMaxC float64 `json:"temperature_max_c"`

	PowerUsageAvgW float64 `json:"power_usage_avg_w"`
	PowerUsageMaxW float64 `json:"power_usage_max_w"`
}

// GPUCollector collects GPU metrics
type GPUCollector struct {
	vendors          []GPUVendor
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

	// Every vendor present, not just the first: a workstation can hold an
	// NVIDIA and an AMD card at once.
	vendors := detectGPUVendors()

	return &GPUCollector{
		vendors:          vendors,
		samplingInterval: time.Duration(samplingIntervalMs) * time.Millisecond,
		samples:          make([]GPUSample, 0, 1000),
		stopChan:         make(chan struct{}),
	}
}

// Start begins collecting GPU metrics
func (c *GPUCollector) Start(ctx context.Context) {
	if len(c.vendors) == 0 {
		log.Println("⚠️  No GPU detected or unsupported GPU vendor")
		return
	}

	go c.collectLoop(ctx)
	log.Printf("🎮 GPU metrics collector started (vendors: %v, interval: %v)", c.vendors, c.samplingInterval)
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

// collectSample collects one sample from every vendor present.
//
// A vendor that fails is reported and skipped rather than aborting the sample:
// on a mixed machine, a missing rocm-smi must not cost the NVIDIA readings.
func (c *GPUCollector) collectSample() error {
	var collected []GPUSample
	var failures []string

	for _, vendor := range c.vendors {
		var (
			samples []GPUSample
			err     error
		)
		switch vendor {
		case GPUVendorNVIDIA:
			samples, err = c.collectNVIDIA()
		case GPUVendorAMD:
			samples, err = c.collectAMD()
		case GPUVendorApple:
			samples, err = c.collectApple()
		default:
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", vendor, err))
			continue
		}
		collected = append(collected, samples...)
	}

	c.samples = append(c.samples, collected...)

	if len(failures) > 0 && len(collected) == 0 {
		return fmt.Errorf("every GPU vendor failed to report (%s)", strings.Join(failures, "; "))
	}
	for _, failure := range failures {
		log.Printf("⚠️  GPU vendor did not report this sample (%s)", failure)
	}
	return nil
}

// nvidiaQueryFields is the exact --query-gpu list the parser expects, in order.
const nvidiaQueryFields = "index,name,utilization.gpu,utilization.memory,memory.used,memory.total,temperature.gpu,power.draw,power.limit,clocks.gr,clocks.mem"

// collectNVIDIA samples NVIDIA GPUs through nvidia-smi.
func (c *GPUCollector) collectNVIDIA() ([]GPUSample, error) {
	output, err := runVendorTool("nvidia-smi",
		"--query-gpu="+nvidiaQueryFields,
		"--format=csv,noheader,nounits",
	)
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi failed: %w", err)
	}
	return parseNVIDIACSV(output, time.Now())
}

// parseNVIDIACSV reads the CSV that nvidia-smi emits for nvidiaQueryFields with
// --format=csv,noheader,nounits.
//
// A row that does not hold every field is skipped rather than partially read:
// nvidia-smi prints "[N/A]" for unsupported counters on some cards, and a
// half-parsed row would look like a real measurement.
func parseNVIDIACSV(output []byte, now time.Time) ([]GPUSample, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	samples := make([]GPUSample, 0, len(lines))

	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 11 {
			continue
		}

		number := func(index int) (float64, bool) {
			value, err := strconv.ParseFloat(strings.TrimSpace(fields[index]), 64)
			if err != nil {
				return 0, false
			}
			return value, true
		}

		deviceID, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			continue
		}

		sample := GPUSample{
			Timestamp:  now,
			Vendor:     GPUVendorNVIDIA,
			DeviceID:   deviceID,
			DeviceName: strings.TrimSpace(fields[1]),
		}
		sample.GPUUtilization, _ = number(2)
		sample.MemoryUtilization, _ = number(3)
		sample.MemoryUsedMB, _ = number(4)
		sample.MemoryTotalMB, _ = number(5)
		sample.TemperatureC, _ = number(6)
		sample.PowerUsageW, _ = number(7)
		sample.PowerLimitW, _ = number(8)
		if clock, ok := number(9); ok {
			sample.ClockSpeedMHz = int(clock)
		}
		if clock, ok := number(10); ok {
			sample.MemoryClockMHz = int(clock)
		}
		// nvidia-smi reports no separate compute figure; utilization.gpu is
		// the closest thing it exposes.
		sample.ComputeUtilization = sample.GPUUtilization

		samples = append(samples, sample)
	}

	return samples, nil
}

func detectGPUVendors() []GPUVendor {
	vendors := []GPUVendor{}

	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		vendors = append(vendors, GPUVendorNVIDIA)
	}
	_, amdSMIErr := exec.LookPath("amd-smi")
	_, rocmSMIErr := exec.LookPath("rocm-smi")
	if amdSMIErr == nil || rocmSMIErr == nil {
		vendors = append(vendors, GPUVendorAMD)
	}
	if _, err := exec.LookPath("ioreg"); err == nil {
		if output, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			if strings.Contains(string(output), "Apple") {
				vendors = append(vendors, GPUVendorApple)
			}
		}
	}

	return vendors
}

// deviceKey namespaces a device by vendor: NVIDIA and AMD both index from 0.
func deviceKey(vendor GPUVendor, deviceID int) string {
	return fmt.Sprintf("%s:%d", vendor, deviceID)
}

// getGPUCount returns the number of GPUs detected
func (c *GPUCollector) getGPUCount() int {
	devices := make(map[string]bool)
	for _, sample := range c.samples {
		devices[deviceKey(sample.Vendor, sample.DeviceID)] = true
	}
	return len(devices)
}

// ComputeGPUAggregates calculates aggregate GPU statistics
func ComputeGPUAggregates(timeSeries *GPUTimeSeries) *GPUAggregates {
	if len(timeSeries.Samples) == 0 {
		return &GPUAggregates{
			PerGPU:          make(map[string]GPUDeviceAggregates),
			MeasuredDevices: []string{},
		}
	}

	// Group by vendor and device: two vendors in one machine both start
	// numbering at zero, so a bare device id would merge their samples.
	perDevice := make(map[string][]GPUSample)
	for _, sample := range timeSeries.Samples {
		key := deviceKey(sample.Vendor, sample.DeviceID)
		perDevice[key] = append(perDevice[key], sample)
	}

	agg := &GPUAggregates{
		PerGPU: make(map[string]GPUDeviceAggregates),
	}

	// Compute per-device aggregates
	for key, samples := range perDevice {
		deviceAgg := computeDeviceAggregates(samples[0].DeviceID, samples)
		agg.PerGPU[key] = deviceAgg

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

	agg.MeasuredDevices = make([]string, 0, len(perDevice))
	for key := range perDevice {
		agg.MeasuredDevices = append(agg.MeasuredDevices, key)
	}
	sort.Strings(agg.MeasuredDevices)

	return agg
}

// AllocatedGPUAggregates computes GPU aggregates restricted to the samples
// the allocated predicate accepts (#67).
//
// The collector samples every GPU visible to the runner process itself
// (nvidia-smi/rocm-smi/ioreg run on the host, not inside the job's
// container, and there is no way to scope that invocation to one
// container's view of the hardware). Filtering therefore happens here,
// after collection, against whatever policy actually governs what the job's
// container was allowed to see — not what it asked for. Two jobs whose
// containers were granted distinct cards get disjoint filtered sample sets,
// and therefore disjoint aggregates, by construction.
//
// Returns nil, not an empty aggregate, when no sample is attributable: a job
// with no GPU allocated (allocated never returns true) must produce no GPU
// aggregate at all, so a consumer of the result cannot mistake "measured,
// found idle" for "never measured". The same nil result also covers the
// degenerate case where a GPU was allocated but sampling produced nothing
// usable for it — there is nothing to report either way.
func AllocatedGPUAggregates(series *GPUTimeSeries, allocated func(vendor GPUVendor, deviceID int) bool) *GPUAggregates {
	if series == nil || allocated == nil {
		return nil
	}

	filtered := make([]GPUSample, 0, len(series.Samples))
	for _, sample := range series.Samples {
		if allocated(sample.Vendor, sample.DeviceID) {
			filtered = append(filtered, sample)
		}
	}
	if len(filtered) == 0 {
		return nil
	}

	return ComputeGPUAggregates(&GPUTimeSeries{
		Samples:          filtered,
		SamplingInterval: series.SamplingInterval,
	})
}

// computeDeviceAggregates computes aggregates for a single GPU
func computeDeviceAggregates(deviceID int, samples []GPUSample) GPUDeviceAggregates {
	if len(samples) == 0 {
		return GPUDeviceAggregates{DeviceID: deviceID}
	}

	agg := GPUDeviceAggregates{
		DeviceID:      deviceID,
		DeviceName:    samples[0].DeviceName,
		Vendor:        samples[0].Vendor,
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
