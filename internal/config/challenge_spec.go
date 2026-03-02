package config

import (
	"fmt"
	"time"
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
	CPUShares      int64
	Timeout        time.Duration
	DiskBytes      int64
	NetworkEnabled bool
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

	// Parse timeout
	if l.Timeout != "" {
		timeout, err := time.ParseDuration(l.Timeout)
		if err != nil {
			return nil, fmt.Errorf("invalid timeout: %w", err)
		}
		parsed.Timeout = timeout
	}

	// Parse disk
	if l.Disk != "" {
		diskBytes, err := parseSize(l.Disk)
		if err != nil {
			return nil, fmt.Errorf("invalid disk limit: %w", err)
		}
		parsed.DiskBytes = diskBytes
	}

	// Parse network
	parsed.NetworkEnabled = l.Network == "enabled"

	return parsed, nil
}

// parseSize converts strings like "512MB" to bytes
func parseSize(size string) (int64, error) {
	var value int64
	var unit string

	_, err := fmt.Sscanf(size, "%d%s", &value, &unit)
	if err != nil {
		return 0, err
	}

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

	return value * multiplier, nil
}
