package system

import "testing"

func TestParseAMDSMIHardwareJSONReadsIdentityAndMemory(t *testing.T) {
	input := []byte(`[
      {
        "gpu": 0,
        "asic": {"market_name": "AMD Instinct MI300X"},
        "bus": {"bdf": "0000:41:00.0"},
        "driver": {"version": "6.10.5"},
        "vram": {"size": {"value": 196608, "unit": "MB"}}
      },
      {
        "gpu": 3,
        "asic": {"market_name": "AMD Radeon PRO W7800"},
        "bus": {"bdf": "0000:42:00.0"},
        "driver": {"version": "6.10.5"},
        "vram": {"size": {"value": 34359738368, "unit": "B"}}
      }
    ]`)

	gpus, err := parseAMDSMIHardwareJSON(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected two GPUs, got %d", len(gpus))
	}
	if gpus[0].DeviceID != 0 || gpus[0].Model != "AMD Instinct MI300X" || gpus[0].MemoryMB != 196608 {
		t.Errorf("first GPU misread: %+v", gpus[0])
	}
	if gpus[0].Driver != "6.10.5" || gpus[0].PCIBusID != "0000:41:00.0" {
		t.Errorf("first GPU driver/bus misread: %+v", gpus[0])
	}
	if gpus[1].DeviceID != 3 || gpus[1].MemoryMB != 32768 {
		t.Errorf("byte-valued VRAM misread: %+v", gpus[1])
	}
}

func TestParseAMDSMIHardwareJSONAcceptsGPUDataWrapper(t *testing.T) {
	input := []byte(`{"gpu_data":[{"gpu":2,"asic":{"market_name":"N/A"},"vram":{"size":"N/A"}}]}`)
	gpus, err := parseAMDSMIHardwareJSON(input)
	if err != nil || len(gpus) != 1 {
		t.Fatalf("wrapped output did not parse: %v (%+v)", err, gpus)
	}
	if gpus[0].Model != "AMD GPU 2" || gpus[0].MemoryMB != 0 {
		t.Errorf("fallback fields misread: %+v", gpus[0])
	}
}

func TestParseAMDSMIHardwareJSONRejectsMalformedJSON(t *testing.T) {
	if _, err := parseAMDSMIHardwareJSON([]byte(`[{"gpu":`)); err == nil {
		t.Fatal("expected malformed JSON to return an explicit error")
	}
}

func TestParseROCmSMIGPUInfoReadsIdentityAndMemory(t *testing.T) {
	input := []byte(`{
      "card0": {
        "Card Series": "AMD Instinct MI210",
        "VRAM Total Memory (B)": "68719476736",
        "PCI Bus": "0000:41:00.0"
      },
      "card1": {
        "Card Series": "AMD Radeon PRO W6800",
        "VRAM Total Memory (B)": "34359738368",
        "PCI Bus": "0000:42:00.0"
      },
      "system": {"AMDGPU version": "6.4.0"}
    }`)

	gpus, err := parseROCmSMIGPUInfo(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gpus) != 2 {
		t.Fatalf("expected two GPUs, got %d", len(gpus))
	}
	if gpus[0].DeviceID != 0 || gpus[0].Vendor != "amd" || gpus[0].Model != "AMD Instinct MI210" {
		t.Errorf("first GPU identity misread: %+v", gpus[0])
	}
	if gpus[0].MemoryMB != 65536 || gpus[0].Driver != "6.4.0" || gpus[0].PCIBusID != "0000:41:00.0" {
		t.Errorf("first GPU hardware fields misread: %+v", gpus[0])
	}
	if gpus[1].DeviceID != 1 || gpus[1].MemoryMB != 32768 {
		t.Errorf("second GPU misread: %+v", gpus[1])
	}
}

func TestParseROCmSMIGPUInfoUsesNonMisleadingFallbacks(t *testing.T) {
	gpus, err := parseROCmSMIGPUInfo([]byte(`{"card2":{"Card Series":"N/A","VRAM Total Memory (B)":"N/A"}}`))
	if err != nil || len(gpus) != 1 {
		t.Fatalf("fixture did not parse: %v (%+v)", err, gpus)
	}
	if gpus[0].Model != "AMD GPU 2" || gpus[0].MemoryMB != 0 || gpus[0].Driver != "" {
		t.Errorf("unexpected fallback values: %+v", gpus[0])
	}
}

func TestParseROCmSMIGPUInfoRejectsMalformedJSON(t *testing.T) {
	if _, err := parseROCmSMIGPUInfo([]byte(`{"card0":`)); err == nil {
		t.Fatal("expected malformed JSON to return an explicit error")
	}
}
