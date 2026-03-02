package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseSamplingInterval converts a sampling interval string to milliseconds
// Supports formats: "100ms", "1s", "500", etc.
func ParseSamplingInterval(interval string) (int, error) {
	if interval == "" {
		return 100, nil // Default: 100ms
	}
	
	// Try to parse as duration
	if duration, err := time.ParseDuration(interval); err == nil {
		return int(duration.Milliseconds()), nil
	}
	
	// Try to parse as plain number (assume milliseconds)
	if val, err := strconv.Atoi(strings.TrimSpace(interval)); err == nil {
		return val, nil
	}
	
	return 0, fmt.Errorf("invalid sampling interval format: %s (expected: '100ms', '1s', or '100')", interval)
}
