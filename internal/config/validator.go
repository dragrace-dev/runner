package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ValidateChallengeSpec validates a ChallengeSpec for correctness
func ValidateChallengeSpec(spec *ChallengeSpec) error {
	if spec.Type != "challenge" {
		return fmt.Errorf("invalid type: expected 'challenge', got '%s'", spec.Type)
	}

	if spec.Challenge.ID == "" {
		return errors.New("challenge.id is required")
	}

	if spec.Challenge.Name == "" {
		return errors.New("challenge.name is required")
	}

	// Limits validation
	if _, err := spec.Limits.Parse(); err != nil {
		return fmt.Errorf("invalid limits: %w", err)
	}

	return nil
}

// ValidateSolutionSpec validates a SolutionConfig for correctness
func ValidateSolutionSpec(spec *SolutionConfig) error {
	if spec.Type != "solution" {
		return fmt.Errorf("invalid type: expected 'solution', got '%s'", spec.Type)
	}

	if spec.Runtime.Docker == "" {
		return errors.New("runtime.docker is required")
	}

	if spec.Run.Script == "" {
		return errors.New("run.script is required")
	}

	return nil
}

// ValidateScriptExists checks if a script file exists in the repo
func ValidateScriptExists(repoDir, scriptPath string) error {
	fullPath := filepath.Join(repoDir, scriptPath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return fmt.Errorf("script not found: %s", scriptPath)
	}
	return nil
}
