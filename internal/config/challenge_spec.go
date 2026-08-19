package config

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SchemaVersion is the only configuration language understood by this runner.
// Bumping it is a deliberate compatibility boundary, not a best-effort parse.
const SchemaVersion = "1.0"

const (
	RunnerMaxMemoryBytes int64 = 64 * 1024 * 1024 * 1024
	RunnerMaxCPUNano     int64 = 16 * 1000000000
	RunnerMaxTimeout           = time.Hour
	RunnerMaxDiskBytes   int64 = 100 * 1024 * 1024 * 1024
)

// ChallengeSpec represents the challenge configuration controlled by the organizer
// This is loaded from the challenge repository's dragrace.yaml
type ChallengeSpec struct {
	Version   string        `yaml:"version"`
	Type      string        `yaml:"type"` // Must be "challenge"
	Challenge ChallengeInfo `yaml:"challenge"`
	Init      *InitConfig   `yaml:"init,omitempty"`
	Validate  *PhaseConfig  `yaml:"validate,omitempty"`
	Limits    LimitsConfig  `yaml:"limits"`
	Scoring   ScoringConfig `yaml:"scoring"`
}

// ChallengeInfo contains metadata about the challenge
type ChallengeInfo struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
}

// InitConfig defines the initialization phase (runs once per machine/ref)
type InitConfig struct {
	Docker  string   `yaml:"docker"`  // Docker image to use
	Script  string   `yaml:"script"`  // Script path relative to repo (e.g., "scripts/init.sh")
	Outputs []string `yaml:"outputs"` // Paths that will be created in /data
}

// PhaseConfig represents a phase execution (validate, etc.)
type PhaseConfig struct {
	Docker string `yaml:"docker"` // Docker image to use
	Script string `yaml:"script"` // Script path relative to repo
}

// LimitsConfig defines resource limits for execution
type LimitsConfig struct {
	Memory  string `yaml:"memory"`  // e.g., "32GB"
	CPU     string `yaml:"cpu"`     // e.g., "8.0"
	Timeout string `yaml:"timeout"` // e.g., "600s"
	Disk    string `yaml:"disk,omitempty"`
	Network string `yaml:"network,omitempty"` // "enabled" or "disabled"

	// GPU is the number of GPUs the challenge needs, e.g. "gpu: 1" (#66).
	// Deliberately a portable COUNT, not a list of device indexes ("gpu:
	// [0,1]"): which physical card(s) a job actually sees is the
	// operator's call (RUNNER_GPUS, #65), resolved per-runner against
	// whatever hardware happens to be installed there. A challenge that
	// asked for index 0 would be assuming a topology — that a device 0
	// even exists, and that it means the same thing — that has no reason
	// to hold on another runner. That would break the run-anywhere
	// comparability #26 exists to protect, and undermine a Golden
	// certification (#60), which is supposed to certify the challenge
	// itself rather than one operator's specific card layout. Omitted or
	// 0 means the challenge does not need a GPU.
	GPU int `yaml:"gpu,omitempty"`
}

// ScoringConfig defines how to calculate the final score
type ScoringConfig struct {
	Primary   string             `yaml:"primary"`   // Primary metric name
	Direction string             `yaml:"direction"` // "minimize" or "maximize"
	Weights   map[string]float64 `yaml:"weights,omitempty"`
}

// ParsedLimits contains parsed resource limits
type ParsedLimits struct {
	MemoryBytes    int64
	CPUNano        int64
	Timeout        time.Duration
	DiskBytes      int64
	NetworkEnabled bool
	// GPUCount is the number of GPUs this run needs (0 = none), parsed
	// from LimitsConfig.GPU (#66).
	GPUCount int
}

// Parse converts string limits to actual values
func (l *LimitsConfig) Parse() (*ParsedLimits, error) {
	parsed := &ParsedLimits{}

	// Parse memory
	if l.Memory != "" {
		memBytes, err := parseSize(l.Memory)
		if err != nil {
			return nil, fmt.Errorf("invalid memory limit: %w", err)
		}
		parsed.MemoryBytes = memBytes
	}

	if l.CPU == "" {
		return nil, fmt.Errorf("cpu limit is required")
	}
	cores, err := strconv.ParseFloat(l.CPU, 64)
	if err != nil || math.IsNaN(cores) || math.IsInf(cores, 0) || cores <= 0 {
		return nil, fmt.Errorf("invalid cpu limit: %q", l.CPU)
	}
	parsed.CPUNano = int64(cores * 1000000000)
	if parsed.CPUNano <= 0 {
		return nil, fmt.Errorf("invalid cpu limit: %q", l.CPU)
	}

	// Parse timeout
	if l.Timeout != "" {
		timeout, err := time.ParseDuration(l.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout: %w", err)
		}
		parsed.Timeout = timeout
	}
	if parsed.Timeout <= 0 {
		return nil, fmt.Errorf("timeout limit is required")
	}

	// Parse disk
	if l.Disk != "" {
		diskBytes, err := parseSize(l.Disk)
		if err != nil {
			return nil, fmt.Errorf("invalid disk limit: %w", err)
		}
		parsed.DiskBytes = diskBytes
	}

	// Network is explicit. An omitted setting is the safe default (disabled).
	if l.Network != "" && l.Network != "enabled" && l.Network != "disabled" {
		return nil, fmt.Errorf("invalid network limit: %q", l.Network)
	}
	parsed.NetworkEnabled = l.Network == "enabled"

	// GPU is a count, not a device list (#66) — see the field doc on
	// LimitsConfig.GPU. Negative is nonsensical; whether it fits on this
	// runner is a question for ClampToRunnerCaps, not for parsing.
	if l.GPU < 0 {
		return nil, fmt.Errorf("invalid gpu limit: %d (must be zero or a positive GPU count)", l.GPU)
	}
	parsed.GPUCount = l.GPU

	return parsed, nil
}

// ClampToRunnerCaps ensures organizer requests cannot reserve more than the
// host policy permits. It is applied immediately before runner execution.
// airGapped forces network access off regardless of what the challenge
// requested: the host's air-gap policy always wins over challenge policy.
//
// gpuCount is the number of GPUs the operator's RUNNER_GPUS policy actually
// exposes on this runner (0 under the default "none"; see gpu.Policy.Count,
// #65). Unlike the other caps it is a runtime value passed in by the
// caller rather than a package constant: there is no host-independent
// "reasonable maximum" GPU count to hardcode the way there is for memory
// or CPU — the ceiling only exists once RUNNER_GPUS has been resolved
// against the hardware actually present.
//
// Memory, CPU, timeout and disk are silently scaled down to the runner's
// fixed ceiling: narrowing the resource envelope still runs the exact same
// code path, so the measurement stays legitimate, just less generous than
// asked. GPU is deliberately handled the opposite way (#66): a request
// that exceeds gpuCount is rejected outright, error included, rather than
// silently reduced — even down to zero. A solution built to run its hot
// path on a GPU takes a materially different path (or none at all, or
// fails) the instant the GPU is not there; a "degraded" GPU run is not a
// smaller version of the same measurement, it is a different measurement
// that would be scored as if it were comparable to a genuine GPU run. That
// is worse than refusing the job outright, so it doesn't happen here. This
// mirrors the precedent already set in docker/executor.go's
// clampResourcesToHost: an explicit challenge request that the host
// cannot satisfy is rejected, not silently downgraded.
func ClampToRunnerCaps(requested *ParsedLimits, airGapped bool, gpuCount int) (*ParsedLimits, error) {
	effective := *requested
	if effective.MemoryBytes > RunnerMaxMemoryBytes {
		effective.MemoryBytes = RunnerMaxMemoryBytes
	}
	if effective.CPUNano > RunnerMaxCPUNano {
		effective.CPUNano = RunnerMaxCPUNano
	}
	if effective.Timeout > RunnerMaxTimeout {
		effective.Timeout = RunnerMaxTimeout
	}
	if effective.DiskBytes > RunnerMaxDiskBytes {
		effective.DiskBytes = RunnerMaxDiskBytes
	}
	if effective.GPUCount > gpuCount {
		return nil, fmt.Errorf(
			"challenge requires %d GPU(s) but this runner's RUNNER_GPUS policy exposes %d: "+
				"refusing rather than running a silently degraded, incomparable measurement",
			effective.GPUCount, gpuCount)
	}
	if airGapped {
		effective.NetworkEnabled = false
	}
	return &effective, nil
}

// parseSize converts strings like "512MB" to bytes
func parseSize(size string) (int64, error) {
	match := regexp.MustCompile(`^([1-9][0-9]*)(KB|K|MB|M|GB|G|TB|T)$`).FindStringSubmatch(strings.ToUpper(strings.TrimSpace(size)))
	if match == nil {
		return 0, fmt.Errorf("invalid size: %q", size)
	}
	value, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	unit := match[2]

	multiplier := int64(1)
	switch unit {
	case "KB", "K":
		multiplier = 1024
	case "MB", "M":
		multiplier = 1024 * 1024
	case "GB", "G":
		multiplier = 1024 * 1024 * 1024
	case "TB", "T":
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown unit: %s", unit)
	}

	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("size overflows int64: %q", size)
	}
	return value * multiplier, nil
}
