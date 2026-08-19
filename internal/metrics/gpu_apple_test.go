package metrics

import (
	"os"
	"testing"
	"time"
)

// Captured with `ioreg -r -c IOAccelerator -a` on a real Apple M5 Pro while the
// GPU was busy, so the fixture proves the parser reads live values rather than
// a machine that happened to be idle.
func TestParseAppleGPUStatsReadsLiveCounters(t *testing.T) {
	output, err := os.ReadFile("testdata/ioreg_apple_m5pro.plist")
	if err != nil {
		t.Fatalf("cannot read fixture: %v", err)
	}

	now := time.Unix(1754784000, 0)
	samples, err := parseAppleGPUStats(output, now)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("expected one accelerator, got %d", len(samples))
	}

	sample := samples[0]
	if sample.Vendor != GPUVendorApple {
		t.Errorf("vendor = %q, want apple", sample.Vendor)
	}
	if sample.DeviceName != "Apple M5 Pro" {
		t.Errorf("device name = %q, want %q", sample.DeviceName, "Apple M5 Pro")
	}
	if sample.GPUUtilization != 35 {
		t.Errorf("utilization = %v, want 35 from the captured busy GPU", sample.GPUUtilization)
	}
	// 1082081280 bytes as reported by ioreg.
	if got := int(sample.MemoryUsedMB); got != 1031 {
		t.Errorf("memory used = %d MB, want 1031", got)
	}
	if !sample.Timestamp.Equal(now) {
		t.Errorf("timestamp = %v, want %v", sample.Timestamp, now)
	}
}

// Temperature, power and clocks need powermetrics, which needs root. Leaving
// them unset is deliberate: a zero would read as a measurement.
func TestParseAppleGPUStatsLeavesRootOnlyFieldsUnset(t *testing.T) {
	output, err := os.ReadFile("testdata/ioreg_apple_m5pro.plist")
	if err != nil {
		t.Fatalf("cannot read fixture: %v", err)
	}

	samples, err := parseAppleGPUStats(output, time.Now())
	if err != nil || len(samples) == 0 {
		t.Fatalf("fixture did not parse: %v", err)
	}
	sample := samples[0]
	if sample.TemperatureC != 0 || sample.PowerUsageW != 0 || sample.ClockSpeedMHz != 0 {
		t.Errorf("expected root-only fields to stay unset, got temp=%v power=%v clock=%v",
			sample.TemperatureC, sample.PowerUsageW, sample.ClockSpeedMHz)
	}
}

// A machine with no reportable accelerator is not an error.
func TestParseAppleGPUStatsReportsNothingWhenEmpty(t *testing.T) {
	empty := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><array/></plist>`)

	samples, err := parseAppleGPUStats(empty, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("expected no sample, got %d", len(samples))
	}
}

func TestParseAppleGPUStatsHandlesTwoAccelerators(t *testing.T) {
	two := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><array>
  <dict>
    <key>model</key><string>Apple M2</string>
    <key>PerformanceStatistics</key>
    <dict><key>Device Utilization %</key><integer>10</integer>
          <key>In use system memory</key><integer>1048576</integer></dict>
  </dict>
  <dict>
    <key>model</key><string>Apple M2 second</string>
    <key>PerformanceStatistics</key>
    <dict><key>Device Utilization %</key><integer>90</integer>
          <key>In use system memory</key><integer>2097152</integer></dict>
  </dict>
</array></plist>`)

	samples, err := parseAppleGPUStats(two, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected two accelerators, got %d", len(samples))
	}
	if samples[0].GPUUtilization != 10 || samples[1].GPUUtilization != 90 {
		t.Errorf("utilizations not read per device: %v / %v", samples[0].GPUUtilization, samples[1].GPUUtilization)
	}
	if samples[0].DeviceID != 0 || samples[1].DeviceID != 1 {
		t.Errorf("device ids not assigned per entry: %d / %d", samples[0].DeviceID, samples[1].DeviceID)
	}
}
