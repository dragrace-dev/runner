package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// rocmSMIQueryArgs deliberately requests byte-valued VRAM counters in addition
// to the percentages printed by --showmemuse. The JSON keys are defined by the
// official ROCm-SMI CLI (python_smi_tools/rocm_smi.py).
var rocmSMIQueryArgs = []string{
	"--json",
	"--showproductname",
	"--showuse",
	"--showmemuse",
	"--showmeminfo", "vram",
	"--showtemp",
	"--showpower",
	"--showmaxpower",
	"--showclocks",
}

var amdSMIMetricArgs = []string{
	"metric",
	"--usage",
	"--mem-usage",
	"--power",
	"--clock",
	"--temperature",
	"--json",
}

var firstNumber = regexp.MustCompile(`[-+]?(?:\d+(?:\.\d*)?|\.\d+)`)

// collectAMD samples every AMD device. AMD-SMI is preferred because ROCm-SMI
// entered maintenance mode with ROCm 7; older installations remain supported.
// Acquisition and parsing stay separate so both contracts remain testable
// without AMD hardware.
func (c *GPUCollector) collectAMD() ([]GPUSample, error) {
	if _, err := exec.LookPath("amd-smi"); err == nil {
		output, runErr := runVendorTool("amd-smi", amdSMIMetricArgs...)
		if runErr == nil {
			if samples, parseErr := parseAMDSMIJSON(output, time.Now()); parseErr == nil {
				return samples, nil
			} else if _, legacyErr := exec.LookPath("rocm-smi"); legacyErr != nil {
				return nil, parseErr
			}
		} else if _, legacyErr := exec.LookPath("rocm-smi"); legacyErr != nil {
			return nil, fmt.Errorf("amd-smi failed: %w", runErr)
		}
	}

	output, err := runVendorTool("rocm-smi", rocmSMIQueryArgs...)
	if err != nil {
		return nil, fmt.Errorf("rocm-smi failed: %w", err)
	}
	return parseROCmSMIJSON(output, time.Now())
}

// parseAMDSMIJSON reads `amd-smi metric --json`. Depending on the AMD-SMI
// release, multi-device output is either a JSON array or an object containing
// gpu_data. Unit-bearing values use the documented {value, unit} shape.
func parseAMDSMIJSON(output []byte, now time.Time) ([]GPUSample, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()

	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode amd-smi JSON: %w", err)
	}
	records := amdSMIRecords(document)
	samples := make([]GPUSample, 0, len(records))
	for index, record := range records {
		values := record
		if nested, ok := record["values"].(map[string]any); ok {
			values = nested
		}

		deviceID := index
		if value, ok := amdMetric(record["gpu"]); ok {
			deviceID = int(value)
		}
		sample := GPUSample{
			Timestamp:  now,
			DeviceID:   deviceID,
			Vendor:     GPUVendorAMD,
			DeviceName: fmt.Sprintf("AMD GPU %d", deviceID),
		}

		sample.GPUUtilization, _ = amdPathMetric(values, "usage", "gfx_activity")
		sample.ComputeUtilization = sample.GPUUtilization
		sample.MemoryTotalMB, _ = amdPathMemoryMB(values, "mem_usage", "total_vram")
		sample.MemoryUsedMB, _ = amdPathMemoryMB(values, "mem_usage", "used_vram")
		if sample.MemoryTotalMB > 0 {
			sample.MemoryUtilization = 100 * sample.MemoryUsedMB / sample.MemoryTotalMB
		}
		sample.PowerUsageW, _ = amdPathMetric(values, "power", "socket_power")
		sample.TemperatureC, _ = firstAMDPathMetric(values,
			[]string{"temperature", "edge"},
			[]string{"temperature", "hotspot"},
			[]string{"temperature", "mem"},
		)
		if value, ok := amdPathMetric(values, "clock", "gfx_0", "clk"); ok {
			sample.ClockSpeedMHz = int(value)
		}
		if value, ok := amdPathMetric(values, "clock", "mem_0", "clk"); ok {
			sample.MemoryClockMHz = int(value)
		}

		samples = append(samples, sample)
	}
	return samples, nil
}

func amdSMIRecords(document any) []map[string]any {
	if object, ok := document.(map[string]any); ok {
		if nested, exists := object["gpu_data"]; exists {
			return amdSMIRecords(nested)
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

func amdPathMetric(values map[string]any, path ...string) (float64, bool) {
	var current any = values
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current, ok = object[part]
		if !ok {
			return 0, false
		}
	}
	return amdMetric(current)
}

func amdMetric(value any) (float64, bool) {
	if object, ok := value.(map[string]any); ok {
		value = object["value"]
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" || strings.EqualFold(text, "N/A") || text == "<nil>" {
		return 0, false
	}
	match := firstNumber.FindString(text)
	if match == "" {
		return 0, false
	}
	number, err := strconv.ParseFloat(match, 64)
	return number, err == nil
}

func amdPathMemoryMB(values map[string]any, path ...string) (float64, bool) {
	var current any = values
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current, ok = object[part]
		if !ok {
			return 0, false
		}
	}
	value, ok := amdMetric(current)
	if !ok {
		return 0, false
	}
	unit := "MB"
	if object, ok := current.(map[string]any); ok {
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
	return value, true
}

func firstAMDPathMetric(values map[string]any, paths ...[]string) (float64, bool) {
	for _, path := range paths {
		if value, ok := amdPathMetric(values, path...); ok {
			return value, true
		}
	}
	return 0, false
}

// parseROCmSMIJSON reads the object emitted by rocm-smi --json. Values are
// strings in current ROCm-SMI releases, but json.Number is accepted as well so
// harmless output changes do not drop a whole device.
func parseROCmSMIJSON(output []byte, now time.Time) ([]GPUSample, error) {
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()

	var document map[string]map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode rocm-smi JSON: %w", err)
	}

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

	samples := make([]GPUSample, 0, len(cards))
	for _, card := range cards {
		values := card.values
		sample := GPUSample{
			Timestamp:  now,
			DeviceID:   card.id,
			Vendor:     GPUVendorAMD,
			DeviceName: textValue(values, "Card Series"),
		}
		if sample.DeviceName == "" {
			sample.DeviceName = fmt.Sprintf("AMD GPU %d", card.id)
		}

		sample.GPUUtilization, _ = numericValue(values, "GPU use (%)")
		sample.ComputeUtilization = sample.GPUUtilization
		sample.MemoryUtilization, _ = numericValue(values, "GPU Memory Allocated (VRAM%)")

		if bytesUsed, ok := numericValue(values, "VRAM Total Used Memory (B)"); ok {
			sample.MemoryUsedMB = bytesUsed / (1024 * 1024)
		}
		if bytesTotal, ok := numericValue(values, "VRAM Total Memory (B)"); ok {
			sample.MemoryTotalMB = bytesTotal / (1024 * 1024)
		}
		if sample.MemoryUtilization == 0 && sample.MemoryTotalMB > 0 {
			sample.MemoryUtilization = 100 * sample.MemoryUsedMB / sample.MemoryTotalMB
		}

		sample.TemperatureC, _ = firstMetric(values,
			"Temperature (Sensor edge) (C)",
			"Temperature (Sensor junction) (C)",
			"Temperature (Sensor hotspot) (C)",
			"Temperature (Sensor memory) (C)",
		)
		sample.PowerUsageW, _ = firstMetric(values,
			"Current Socket Graphics Package Power (W)",
			"Average Graphics Package Power (W)",
		)
		sample.PowerLimitW, _ = numericValue(values, "Max Graphics Package Power (W)")
		if clock, ok := numericValue(values, "sclk clock speed:"); ok {
			sample.ClockSpeedMHz = int(clock)
		}
		if clock, ok := numericValue(values, "mclk clock speed:"); ok {
			sample.MemoryClockMHz = int(clock)
		}

		samples = append(samples, sample)
	}

	return samples, nil
}

func textValue(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok || value == nil {
		return ""
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if strings.EqualFold(text, "N/A") {
		return ""
	}
	return text
}

func numericValue(values map[string]any, key string) (float64, bool) {
	text := textValue(values, key)
	match := firstNumber.FindString(text)
	if match == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(match, 64)
	return value, err == nil
}

func firstMetric(values map[string]any, keys ...string) (float64, bool) {
	for _, key := range keys {
		if value, ok := numericValue(values, key); ok {
			return value, true
		}
	}
	return 0, false
}
