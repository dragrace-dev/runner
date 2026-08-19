package system

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Apple exposes no VRAM figure because the GPU shares system memory, so the
// core count is the only thing that separates a Pro from a Max from an Ultra.
// system_profiler reports it; ioreg does not.

type appleDisplaysProfile struct {
	Displays []appleDisplayEntry `json:"SPDisplaysDataType"`
}

type appleDisplayEntry struct {
	Name       string `json:"_name"`
	Model      string `json:"sppci_model"`
	Vendor     string `json:"spdisplays_vendor"`
	DeviceType string `json:"sppci_device_type"`
	Bus        string `json:"sppci_bus"`
	// Reported as a JSON string, not a number.
	Cores string `json:"sppci_cores"`
}

// parseAppleGPUProfile reads `system_profiler -json SPDisplaysDataType`.
//
// Returns nil without error when the output holds no GPU: a Mac with no
// reportable accelerator is not a failure, it just has nothing to describe.
func parseAppleGPUProfile(output []byte) (*GPUInfo, error) {
	var profile appleDisplaysProfile
	if err := json.Unmarshal(output, &profile); err != nil {
		return nil, fmt.Errorf("system_profiler output is not the expected JSON: %w", err)
	}

	for _, entry := range profile.Displays {
		// Non-GPU display entries (external monitors reported on their own)
		// carry a different device type.
		if entry.DeviceType != "" && entry.DeviceType != "spdisplays_gpu" {
			continue
		}

		model := entry.Model
		if model == "" {
			model = entry.Name
		}
		if model == "" {
			continue
		}

		cores := 0
		if entry.Cores != "" {
			parsed, err := strconv.Atoi(strings.TrimSpace(entry.Cores))
			if err != nil {
				return nil, fmt.Errorf("unexpected GPU core count %q: %w", entry.Cores, err)
			}
			cores = parsed
		}

		return &GPUInfo{
			DeviceID:  0,
			Vendor:    "apple",
			Model:     model,
			CoreCount: cores,
			// Unified memory: there is no dedicated VRAM to report.
			MemoryMB: 0,
			Driver:   "Metal",
			PCIBusID: "integrated",
		}, nil
	}

	return nil, nil
}
