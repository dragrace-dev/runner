package metrics

import (
	"os"
	"testing"
	"time"
)

// Shaped from the exact --query-gpu list in nvidiaQueryFields with
// --format=csv,noheader,nounits. Hand-written rather than captured: no NVIDIA
// hardware was available, so the values are representative, not measured.
func TestParseNVIDIACSVReadsEveryDevice(t *testing.T) {
	output, err := os.ReadFile("testdata/nvidia_smi_two_gpus.csv")
	if err != nil {
		t.Fatalf("cannot read fixture: %v", err)
	}

	now := time.Unix(1754784000, 0)
	samples, err := parseNVIDIACSV(output, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected two GPUs, got %d", len(samples))
	}

	first := samples[0]
	if first.DeviceID != 0 || first.DeviceName != "NVIDIA GeForce RTX 4090" {
		t.Errorf("first GPU misread: id=%d name=%q", first.DeviceID, first.DeviceName)
	}
	if first.GPUUtilization != 97 || first.MemoryUsedMB != 18342 || first.MemoryTotalMB != 24564 {
		t.Errorf("first GPU values misread: %+v", first)
	}
	if first.TemperatureC != 71 || first.PowerUsageW != 412.35 || first.PowerLimitW != 450 {
		t.Errorf("first GPU thermals/power misread: %+v", first)
	}
	if first.ClockSpeedMHz != 2520 || first.MemoryClockMHz != 10501 {
		t.Errorf("first GPU clocks misread: %d / %d", first.ClockSpeedMHz, first.MemoryClockMHz)
	}
	if samples[1].DeviceID != 1 || samples[1].GPUUtilization != 12 {
		t.Errorf("second GPU misread: %+v", samples[1])
	}
}

// nvidia-smi prints [N/A] for counters a card does not expose. The row must
// still yield the fields that are readable rather than being dropped.
func TestParseNVIDIACSVToleratesUnsupportedCounters(t *testing.T) {
	line := []byte("0, NVIDIA T400, 5, 1, 120, 2048, [N/A], [N/A], [N/A], 1200, 3000\n")

	samples, err := parseNVIDIACSV(line, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("expected the row to be kept, got %d samples", len(samples))
	}
	if samples[0].GPUUtilization != 5 || samples[0].MemoryUsedMB != 120 {
		t.Errorf("readable fields lost: %+v", samples[0])
	}
	if samples[0].TemperatureC != 0 || samples[0].PowerUsageW != 0 {
		t.Errorf("unsupported counters should stay unset, got %+v", samples[0])
	}
}

// A machine with the driver installed but no card prints nothing.
func TestParseNVIDIACSVReportsNothingOnEmptyOutput(t *testing.T) {
	samples, err := parseNVIDIACSV([]byte("\n  \n"), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("expected no sample, got %d", len(samples))
	}
}

func TestParseNVIDIACSVSkipsTruncatedRows(t *testing.T) {
	samples, err := parseNVIDIACSV([]byte("0, NVIDIA T400, 5\n"), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("a row missing fields must be skipped, got %+v", samples)
	}
}

// Two vendors both number their devices from zero, so aggregates must be keyed
// by vendor as well as index or their samples merge into one device.
func TestComputeGPUAggregatesKeepsVendorsApart(t *testing.T) {
	now := time.Now()
	series := &GPUTimeSeries{Samples: []GPUSample{
		{Timestamp: now, Vendor: GPUVendorNVIDIA, DeviceID: 0, DeviceName: "RTX 4090", GPUUtilization: 90},
		{Timestamp: now, Vendor: GPUVendorAMD, DeviceID: 0, DeviceName: "Radeon", GPUUtilization: 10},
	}}

	aggregates := ComputeGPUAggregates(series)
	if len(aggregates.PerGPU) != 2 {
		t.Fatalf("expected two distinct devices, got %d: %+v", len(aggregates.PerGPU), aggregates.PerGPU)
	}
	if _, ok := aggregates.PerGPU["nvidia:0"]; !ok {
		t.Errorf("missing nvidia:0 in %+v", aggregates.PerGPU)
	}
	if _, ok := aggregates.PerGPU["amd:0"]; !ok {
		t.Errorf("missing amd:0 in %+v", aggregates.PerGPU)
	}
}
