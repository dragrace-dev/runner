package system

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// detectAMDGPUs snapshots stable identity fields. AMD-SMI is preferred on
// current ROCm releases, with ROCm-SMI retained for older installations.
// Metrics use a separate live query; keeping identity here means AMD hardware
// participates in runner fingerprints and scheduling just like NVIDIA/Apple.
func detectAMDGPUs() []GPUInfo {
	if _, err := exec.LookPath("amd-smi"); err == nil {
		output, runErr := exec.Command("amd-smi",
			"static",
			"--asic",
			"--bus",
			"--driver",
			"--vram",
			"--json",
		).Output()
		if runErr == nil {
			if gpus, parseErr := parseAMDSMIHardwareJSON(output); parseErr == nil {
				return gpus
			}
		}
	}

	output, err := exec.Command("rocm-smi",
		"--json",
		"--showproductname",
		"--showmeminfo", "vram",
		"--showbus",
		"--showdriverversion",
	).Output()
	if err != nil {
		return nil
	}

	gpus, err := parseROCmSMIGPUInfo(output)
	if err != nil {
		return nil
	}
	return gpus
}

func parseAMDSMIHardwareJSON(output []byte) ([]GPUInfo, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()

	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode amd-smi hardware JSON: %w", err)
	}
	records := amdHardwareRecords(document)
	gpus := make([]GPUInfo, 0, len(records))
	for index, record := range records {
		deviceID := index
		if value, ok := amdHardwareNumber(record["gpu"]); ok {
			deviceID = int(value)
		}
		model := amdHardwareText(record, "asic", "market_name")
		if model == "" {
			model = fmt.Sprintf("AMD GPU %d", deviceID)
		}
		memoryMB := 0
		if raw, ok := amdHardwarePath(record, "vram", "size"); ok {
			if value, valid := amdHardwareNumber(raw); valid {
				unit := "MB"
				if object, isObject := raw.(map[string]any); isObject {
					unit = strings.ToUpper(strings.TrimSpace(fmt.Sprint(object["unit"])))
				}
				switch unit {
				case "B", "BYTE", "BYTES":
					value /= 1024 * 1024
				case "KB", "KIB":
					value /= 1024
				case "GB", "GIB":
					value *= 1024
				}
				memoryMB = int(value)
			}
		}

		gpus = append(gpus, GPUInfo{
			DeviceID: deviceID,
			Vendor:   "amd",
			Model:    model,
			MemoryMB: memoryMB,
			Driver:   amdHardwareText(record, "driver", "version"),
			PCIBusID: amdHardwareText(record, "bus", "bdf"),
		})
	}
	return gpus, nil
}

func amdHardwareRecords(document any) []map[string]any {
	if object, ok := document.(map[string]any); ok {
		if nested, exists := object["gpu_data"]; exists {
			return amdHardwareRecords(nested)
		}
		return []map[string]any{object}
	}
	items, ok := document.([]any)
	if !ok {
		return nil
	}
	records := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if record, ok := item.(map[string]any); ok {
			records = append(records, record)
		}
	}
	return records
}

func amdHardwarePath(values map[string]any, path ...string) (any, bool) {
	var current any = values
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func amdHardwareText(values map[string]any, path ...string) string {
	value, ok := amdHardwarePath(values, path...)
	if !ok || value == nil {
		return ""
	}
	if object, isObject := value.(map[string]any); isObject {
		value = object["value"]
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" || strings.EqualFold(text, "N/A") {
		return ""
	}
	return text
}

func amdHardwareNumber(value any) (float64, bool) {
	if object, ok := value.(map[string]any); ok {
		value = object["value"]
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || text == "<nil>" || strings.EqualFold(text, "N/A") {
		return 0, false
	}
	number, err := strconv.ParseFloat(text, 64)
	return number, err == nil
}

func parseROCmSMIGPUInfo(output []byte) ([]GPUInfo, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()

	var document map[string]map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode rocm-smi hardware JSON: %w", err)
	}

	driver := firstAMDText(document["system"], "AMDGPU version", "Driver version")

	type card struct {
		id     int
		values map[string]any
	}
	cards := make([]card, 0, len(document))
	for key, values := range document {
		if !strings.HasPrefix(key, "card") {
			continue
		}
		id, err := strconv.Atoi(strings.TrimPrefix(key, "card"))
		if err != nil {
			continue
		}
		cards = append(cards, card{id: id, values: values})
	}
	sort.Slice(cards, func(i, j int) bool { return cards[i].id < cards[j].id })

	gpus := make([]GPUInfo, 0, len(cards))
	for _, card := range cards {
		model := firstAMDText(card.values, "Card Series", "Device Name")
		if model == "" {
			model = fmt.Sprintf("AMD GPU %d", card.id)
		}

		memoryMB := 0
		if value := firstAMDText(card.values, "VRAM Total Memory (B)"); value != "" {
			if bytesTotal, err := strconv.ParseUint(value, 10, 64); err == nil {
				memoryMB = int(bytesTotal / (1024 * 1024))
			}
		}

		gpus = append(gpus, GPUInfo{
			DeviceID: card.id,
			Vendor:   "amd",
			Model:    model,
			MemoryMB: memoryMB,
			Driver:   driver,
			PCIBusID: firstAMDText(card.values, "PCI Bus"),
		})
	}

	return gpus, nil
}

func firstAMDText(values map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := values[key]
		if !ok || value == nil {
			continue
		}
		text := strings.TrimSpace(fmt.Sprint(value))
		if text != "" && !strings.EqualFold(text, "N/A") {
			return text
		}
	}
	return ""
}
