package metrics

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// runVendorTool runs a vendor CLI and returns its stdout. Kept separate from
// parsing so every parser can be exercised against captured fixtures, with no
// GPU and no vendor tooling installed.
func runVendorTool(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// Apple reports live GPU counters through ioreg, which emits an XML property
// list with `-a`. Only a narrow shape is needed — an array of dictionaries
// holding a model string and a PerformanceStatistics dictionary of integers —
// so it is decoded here rather than pulling in a plist dependency.
//
// Temperature, power and clocks are deliberately absent: on macOS those come
// from powermetrics, which requires root. Reporting them as zero would be
// worse than not reporting them, so they are left unset.

// appleAccelerator is the decoded shape this collector cares about.
type appleAccelerator struct {
	Model                string
	DeviceUtilization    float64
	RendererUtilization  float64
	InUseSystemMemoryB   float64
	AllocSystemMemoryB   float64
}

// parseAppleGPUStats reads `ioreg -r -c IOAccelerator -a`.
//
// Returns an empty slice without error when no accelerator reports statistics:
// a machine without a reportable GPU is not a failure.
func parseAppleGPUStats(output []byte, now time.Time) ([]GPUSample, error) {
	entries, err := decodeAcceleratorPlist(output)
	if err != nil {
		return nil, err
	}

	samples := make([]GPUSample, 0, len(entries))
	for index, entry := range entries {
		sample := GPUSample{
			Timestamp:  now,
			DeviceID:   index,
			Vendor:     GPUVendorApple,
			DeviceName: entry.Model,

			GPUUtilization:     entry.DeviceUtilization,
			ComputeUtilization: entry.DeviceUtilization,
			// Unified memory: there is no VRAM total to take a ratio against,
			// so a memory utilization percentage would be meaningless.
			MemoryUsedMB: entry.InUseSystemMemoryB / (1024 * 1024),
		}
		samples = append(samples, sample)
	}
	return samples, nil
}

// decodeAcceleratorPlist walks the XML plist with a token reader. The generic
// nested-dictionary shape does not map onto struct tags cleanly, and only two
// keys are needed, so the tokens are read directly.
func decodeAcceleratorPlist(output []byte) ([]appleAccelerator, error) {
	decoder := xml.NewDecoder(bytes.NewReader(output))

	var (
		entries     []appleAccelerator
		current     *appleAccelerator
		depth       int
		pendingKey  string
		inStats     bool
		statsDepth  int
		lastKey     string
	)

	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}

		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "dict":
				depth++
				// Depth 1 is one accelerator; deeper dictionaries are its
				// nested values.
				if depth == 1 {
					entries = append(entries, appleAccelerator{})
					current = &entries[len(entries)-1]
				}
				if pendingKey == "PerformanceStatistics" {
					inStats = true
					statsDepth = depth
				}
				pendingKey = ""
			case "key":
				var value string
				if err := decoder.DecodeElement(&value, &element); err == nil {
					pendingKey = value
					lastKey = value
				}
			case "string", "integer", "real":
				var value string
				if err := decoder.DecodeElement(&value, &element); err != nil || current == nil {
					pendingKey = ""
					continue
				}
				assignAppleField(current, lastKey, value, inStats)
				pendingKey = ""
			}
		case xml.EndElement:
			if element.Name.Local == "dict" {
				if inStats && depth == statsDepth {
					inStats = false
				}
				depth--
			}
		}
	}

	if len(entries) == 0 {
		return nil, nil
	}
	return entries, nil
}

func assignAppleField(entry *appleAccelerator, key, value string, inStats bool) {
	if !inStats {
		if key == "model" {
			entry.Model = value
		}
		return
	}

	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return
	}
	switch key {
	case "Device Utilization %":
		entry.DeviceUtilization = number
	case "Renderer Utilization %":
		entry.RendererUtilization = number
	case "In use system memory":
		entry.InUseSystemMemoryB = number
	case "Alloc system memory":
		entry.AllocSystemMemoryB = number
	}
}

// collectApple samples live counters for the Apple GPU.
func (c *GPUCollector) collectApple() ([]GPUSample, error) {
	output, err := runVendorTool("ioreg", "-r", "-c", "IOAccelerator", "-a")
	if err != nil {
		return nil, fmt.Errorf("ioreg failed: %w", err)
	}
	return parseAppleGPUStats(output, time.Now())
}
