package system

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// HardwareInfo contains complete hardware configuration
type HardwareInfo struct {
	// System identification
	HostUUID     string    `json:"host_uuid"`
	Hostname     string    `json:"hostname"`
	Fingerprint  string    `json:"fingerprint"` // SHA256 of critical hardware config
	
	// OS Information
	OS           string    `json:"os"`            // linux, darwin, windows
	OSVersion    string    `json:"os_version"`    // Kernel version
	Architecture string    `json:"architecture"`  // amd64, arm64
	
	// CPU Information
	CPUModel     string    `json:"cpu_model"`
	CPUCores     int       `json:"cpu_cores"`
	CPUThreads   int       `json:"cpu_threads"`
	CPUFreqMHz   int       `json:"cpu_freq_mhz"`  // Base frequency
	
	// Memory Information
	MemoryTotalGB float64  `json:"memory_total_gb"`
	
	// GPU Information
	GPUs         []GPUInfo `json:"gpus"`
	
	// Collection timestamp
	CollectedAt  time.Time `json:"collected_at"`
}

// GPUInfo contains information about a single GPU
type GPUInfo struct {
	DeviceID     int    `json:"device_id"`
	Vendor       string `json:"vendor"`        // nvidia, amd, apple
	Model        string `json:"model"`
	MemoryMB     int    `json:"memory_mb"`
	Driver       string `json:"driver"`
	PCIBusID     string `json:"pci_bus_id"`
}

// CollectHardwareInfo gathers complete hardware configuration
func CollectHardwareInfo() (*HardwareInfo, error) {
	log.Println("🔍 Collecting hardware information...")
	
	info := &HardwareInfo{
		OS:           runtime.GOOS,
		Architecture: runtime.GOARCH,
		CollectedAt:  time.Now(),
	}
	
	// Hostname
	hostname, _ := os.Hostname()
	info.Hostname = hostname
	
	// Host UUID (system-specific)
	info.HostUUID = getHostUUID()
	
	// OS Version
	info.OSVersion = getOSVersion()
	
	// CPU Information
	info.CPUCores = runtime.NumCPU()
	info.CPUModel, info.CPUThreads, info.CPUFreqMHz = getCPUInfo()
	
	// Memory
	info.MemoryTotalGB = getMemoryTotal()
	
	// GPU Information
	info.GPUs = detectGPUs()
	
	// Calculate fingerprint
	info.Fingerprint = calculateFingerprint(info)
	
	log.Printf("✅ Hardware info collected: %s", info.Fingerprint[:16])
	return info, nil
}

// getHostUUID returns a unique hardware identifier
func getHostUUID() string {
	switch runtime.GOOS {
	case "linux":
		// Try /sys/class/dmi/id/product_uuid
		if uuid, err := os.ReadFile("/sys/class/dmi/id/product_uuid"); err == nil {
			return strings.TrimSpace(string(uuid))
		}
		// Try /etc/machine-id
		if id, err := os.ReadFile("/etc/machine-id"); err == nil {
			return strings.TrimSpace(string(id))
		}
	case "darwin":
		// macOS: use IOPlatformUUID
		cmd := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
		if output, err := cmd.Output(); err == nil {
			lines := strings.Split(string(output), "\n")
			for _, line := range lines {
				if strings.Contains(line, "IOPlatformUUID") {
					parts := strings.Split(line, "\"")
					if len(parts) >= 4 {
						return parts[3]
					}
				}
			}
		}
	case "windows":
		// Windows: use wmic
		cmd := exec.Command("wmic", "csproduct", "get", "UUID")
		if output, err := cmd.Output(); err == nil {
			lines := strings.Split(string(output), "\n")
			if len(lines) > 1 {
				return strings.TrimSpace(lines[1])
			}
		}
	}
	
	// Fallback: generate from MAC address or hostname
	return "unknown"
}

// getOSVersion returns OS version/kernel
func getOSVersion() string {
	switch runtime.GOOS {
	case "linux":
		cmd := exec.Command("uname", "-r")
		if output, err := cmd.Output(); err == nil {
			return strings.TrimSpace(string(output))
		}
	case "darwin":
		cmd := exec.Command("sw_vers", "-productVersion")
		if output, err := cmd.Output(); err == nil {
			return strings.TrimSpace(string(output))
		}
	case "windows":
		cmd := exec.Command("ver")
		if output, err := cmd.Output(); err == nil {
			return strings.TrimSpace(string(output))
		}
	}
	return "unknown"
}

// getCPUInfo returns CPU model, thread count, and frequency
func getCPUInfo() (model string, threads int, freqMHz int) {
	threads = runtime.NumCPU()
	model = "Unknown CPU"
	freqMHz = 0
	
	switch runtime.GOOS {
	case "linux":
		// Read /proc/cpuinfo
		data, err := os.ReadFile("/proc/cpuinfo")
		if err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "model name") {
					parts := strings.Split(line, ":")
					if len(parts) > 1 {
						model = strings.TrimSpace(parts[1])
					}
				}
				if strings.HasPrefix(line, "cpu MHz") {
					parts := strings.Split(line, ":")
					if len(parts) > 1 {
						if freq, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil {
							freqMHz = int(freq)
						}
					}
					break // Only need first CPU
				}
			}
		}
	case "darwin":
		// macOS: use sysctl
		cmd := exec.Command("sysctl", "-n", "machdep.cpu.brand_string")
		if output, err := cmd.Output(); err == nil {
			model = strings.TrimSpace(string(output))
		}
		cmd = exec.Command("sysctl", "-n", "hw.cpufrequency")
		if output, err := cmd.Output(); err == nil {
			if freq, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64); err == nil {
				freqMHz = int(freq / 1_000_000) // Hz to MHz
			}
		}
	case "windows":
		// Windows: use wmic
		cmd := exec.Command("wmic", "cpu", "get", "Name")
		if output, err := cmd.Output(); err == nil {
			lines := strings.Split(string(output), "\n")
			if len(lines) > 1 {
				model = strings.TrimSpace(lines[1])
			}
		}
	}
	
	return model, threads, freqMHz
}

// getMemoryTotal returns total RAM in GB
func getMemoryTotal() float64 {
	switch runtime.GOOS {
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			lines := strings.Split(string(data), "\n")
			for _, line := range lines {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
							return float64(kb) / 1024 / 1024 // KB to GB
						}
					}
					break
				}
			}
		}
	case "darwin":
		cmd := exec.Command("sysctl", "-n", "hw.memsize")
		if output, err := cmd.Output(); err == nil {
			if bytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64); err == nil {
				return float64(bytes) / 1024 / 1024 / 1024 // Bytes to GB
			}
		}
	case "windows":
		cmd := exec.Command("wmic", "computersystem", "get", "TotalPhysicalMemory")
		if output, err := cmd.Output(); err == nil {
			lines := strings.Split(string(output), "\n")
			if len(lines) > 1 {
				if bytes, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); err == nil {
					return float64(bytes) / 1024 / 1024 / 1024
				}
			}
		}
	}
	return 0
}

// detectGPUs detects all GPUs in the system
func detectGPUs() []GPUInfo {
	gpus := []GPUInfo{}
	
	// Try NVIDIA
	if nvGPUs := detectNVIDIAGPUs(); len(nvGPUs) > 0 {
		gpus = append(gpus, nvGPUs...)
	}
	
	// Try AMD
	if amdGPUs := detectAMDGPUs(); len(amdGPUs) > 0 {
		gpus = append(gpus, amdGPUs...)
	}
	
	// Try Apple Silicon
	if appleGPU := detectAppleGPU(); appleGPU != nil {
		gpus = append(gpus, *appleGPU)
	}
	
	return gpus
}

// detectNVIDIAGPUs detects NVIDIA GPUs using nvidia-smi
func detectNVIDIAGPUs() []GPUInfo {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total,driver_version,pci.bus_id",
		"--format=csv,noheader,nounits",
	)
	
	output, err := cmd.Output()
	if err != nil {
		return nil
	}
	
	gpus := []GPUInfo{}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	
	for _, line := range lines {
		fields := strings.Split(line, ",")
		if len(fields) < 5 {
			continue
		}
		
		deviceID, _ := strconv.Atoi(strings.TrimSpace(fields[0]))
		memoryMB, _ := strconv.Atoi(strings.TrimSpace(fields[2]))
		
		gpus = append(gpus, GPUInfo{
			DeviceID: deviceID,
			Vendor:   "nvidia",
			Model:    strings.TrimSpace(fields[1]),
			MemoryMB: memoryMB,
			Driver:   strings.TrimSpace(fields[3]),
			PCIBusID: strings.TrimSpace(fields[4]),
		})
	}
	
	return gpus
}

// detectAMDGPUs detects AMD GPUs using rocm-smi
func detectAMDGPUs() []GPUInfo {
	// TODO: Implement AMD GPU detection via rocm-smi
	return nil
}

// detectAppleGPU detects Apple Silicon GPU
func detectAppleGPU() *GPUInfo {
	if runtime.GOOS != "darwin" {
		return nil
	}
	
	// Check if it's Apple Silicon
	cmd := exec.Command("sysctl", "-n", "machdep.cpu.brand_string")
	output, err := cmd.Output()
	if err != nil || !strings.Contains(string(output), "Apple") {
		return nil
	}
	
	return &GPUInfo{
		DeviceID: 0,
		Vendor:   "apple",
		Model:    "Apple Silicon GPU",
		MemoryMB: 0, // Shared memory with RAM
		Driver:   "Metal",
		PCIBusID: "integrated",
	}
}

// calculateFingerprint creates a SHA256 hash of critical hardware config
// This fingerprint should change if any critical hardware is modified
func calculateFingerprint(info *HardwareInfo) string {
	// Concatenate critical hardware identifiers
	data := fmt.Sprintf("%s|%s|%s|%d|%d|%.0f",
		info.HostUUID,
		info.CPUModel,
		info.Architecture,
		info.CPUCores,
		info.CPUFreqMHz,
		info.MemoryTotalGB,
	)
	
	// Add GPU fingerprints
	for _, gpu := range info.GPUs {
		data += fmt.Sprintf("|%s:%s:%d", gpu.Vendor, gpu.Model, gpu.MemoryMB)
	}
	
	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])
}

// CompareFingerprints checks if two fingerprints match
func CompareFingerprints(fp1, fp2 string) bool {
	return fp1 == fp2
}
