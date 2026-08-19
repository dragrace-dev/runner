package system

import (
	"os"
	"testing"
)

// Captured from a real machine with `system_profiler -json SPDisplaysDataType`.
// Attached displays were stripped: they vary with whatever is plugged in and
// say nothing about the GPU.
func TestParseAppleGPUProfileReadsModelAndCores(t *testing.T) {
	output, err := os.ReadFile("testdata/system_profiler_apple_m5pro.json")
	if err != nil {
		t.Fatalf("cannot read fixture: %v", err)
	}

	gpu, err := parseAppleGPUProfile(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gpu == nil {
		t.Fatal("expected a GPU to be reported")
	}

	// The point of the whole exercise: the real model, not a hardcoded string.
	if gpu.Model != "Apple M5 Pro" {
		t.Errorf("model = %q, want %q", gpu.Model, "Apple M5 Pro")
	}
	// Core count is what separates a Pro from a Max from an Ultra.
	if gpu.CoreCount != 20 {
		t.Errorf("core count = %d, want 20", gpu.CoreCount)
	}
	if gpu.Vendor != "apple" {
		t.Errorf("vendor = %q, want apple", gpu.Vendor)
	}
	// Unified memory: reporting a VRAM figure would be a lie.
	if gpu.MemoryMB != 0 {
		t.Errorf("memory = %d, want 0 for unified memory", gpu.MemoryMB)
	}
}

func TestParseAppleGPUProfileSkipsNonGPUEntries(t *testing.T) {
	output := []byte(`{"SPDisplaysDataType":[
		{"_name":"DELL U2720Q","sppci_device_type":"spdisplays_display"},
		{"sppci_model":"Apple M2","sppci_cores":"10","sppci_device_type":"spdisplays_gpu"}
	]}`)

	gpu, err := parseAppleGPUProfile(output)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gpu == nil || gpu.Model != "Apple M2" || gpu.CoreCount != 10 {
		t.Fatalf("expected the GPU entry to win, got %+v", gpu)
	}
}

// A Mac reporting no accelerator is not an error, it simply has nothing to say.
func TestParseAppleGPUProfileReportsNothingWhenNoGPU(t *testing.T) {
	gpu, err := parseAppleGPUProfile([]byte(`{"SPDisplaysDataType":[]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gpu != nil {
		t.Fatalf("expected no GPU, got %+v", gpu)
	}
}

func TestParseAppleGPUProfileRejectsUnreadableOutput(t *testing.T) {
	if _, err := parseAppleGPUProfile([]byte("not json")); err == nil {
		t.Fatal("expected an error on unparseable output")
	}
}

// The vendor reports the count as a string; a future format change must be
// loud rather than silently yielding zero cores.
func TestParseAppleGPUProfileRejectsNonNumericCoreCount(t *testing.T) {
	output := []byte(`{"SPDisplaysDataType":[{"sppci_model":"Apple M9","sppci_cores":"many","sppci_device_type":"spdisplays_gpu"}]}`)
	if _, err := parseAppleGPUProfile(output); err == nil {
		t.Fatal("expected an error on a non-numeric core count")
	}
}
