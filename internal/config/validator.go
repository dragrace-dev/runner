package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ValidateChallengeSpec validates a ChallengeSpec for correctness
func ValidateChallengeSpec(spec *ChallengeSpec) error {
	if spec.Version != SchemaVersion {
		return fmt.Errorf("unsupported challenge schema version %q (expected %s)", spec.Version, SchemaVersion)
	}
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
	if spec.Scoring.Primary == "" || (spec.Scoring.Direction != "minimize" && spec.Scoring.Direction != "maximize") {
		return errors.New("scoring.primary and scoring.direction (minimize|maximize) are required")
	}
	if spec.Init != nil {
		if err := validatePhase("init", spec.Init.Docker, spec.Init.Script); err != nil {
			return err
		}
		for _, output := range spec.Init.Outputs {
			if !isSafeDataPath(output) {
				return fmt.Errorf("init.outputs contains unsafe data path %q", output)
			}
		}
	}
	if spec.Validate != nil {
		if err := validatePhase("validate", spec.Validate.Docker, spec.Validate.Script); err != nil {
			return err
		}
	}

	return nil
}

// ValidateSolutionSpec validates a SolutionConfig for correctness
func ValidateSolutionSpec(spec *SolutionConfig) error {
	if spec.Version != SchemaVersion {
		return fmt.Errorf("unsupported solution schema version %q (expected %s)", spec.Version, SchemaVersion)
	}
	if spec.Type != "solution" {
		return fmt.Errorf("invalid type: expected 'solution', got '%s'", spec.Type)
	}

	if !isSafeImage(spec.Runtime.Docker) {
		return errors.New("runtime.docker must be a safe container image reference")
	}

	if !isSafeRepoPath(spec.Run.Script) {
		return errors.New("run.script must be a safe relative repository path")
	}
	if spec.Build != nil {
		if !isSafeRepoPath(spec.Build.Script) {
			return errors.New("build.script must be a safe relative repository path")
		}
	}
	if spec.Run.Stdout != "" && !isSafeDataPath(spec.Run.Stdout) {
		return errors.New("run.stdout must be a path below /data")
	}

	return nil
}

// ValidateScriptExists checks if a script file exists in the repo
func ValidateScriptExists(repoDir, scriptPath string) error {
	if !isSafeRepoPath(scriptPath) {
		return fmt.Errorf("unsafe script path: %s", scriptPath)
	}
	fullPath := filepath.Join(repoDir, scriptPath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return fmt.Errorf("script not found: %s", scriptPath)
	}
	return nil
}

func validatePhase(name, image, script string) error {
	if !isSafeImage(image) {
		return fmt.Errorf("%s.docker must be a safe container image reference", name)
	}
	if !isSafeRepoPath(script) {
		return fmt.Errorf("%s.script must be a safe relative repository path", name)
	}
	return nil
}

func isSafeRepoPath(path string) bool {
	if path == "" || strings.ContainsAny(path, "\r\n\x00") || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func isSafeDataPath(path string) bool {
	if strings.ContainsAny(path, "\r\n\x00") || !strings.HasPrefix(path, "/data/") {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "/data" && strings.HasPrefix(clean, "/data/")
}

func isSafeImage(image string) bool {
	if image == "" || len(image) > 255 || strings.ContainsAny(image, "\r\n\x00 ;&|`$(){}<>\\") || strings.Contains(image, "://") || strings.Contains(image, "..") || strings.HasPrefix(image, "/") {
		return false
	}
	parts := strings.Split(image, "@")
	if len(parts) > 2 || parts[0] == "" {
		return false
	}
	if len(parts) == 2 {
		if !strings.HasPrefix(parts[1], "sha256:") || len(parts[1]) != len("sha256:")+64 {
			return false
		}
		for _, char := range parts[1][len("sha256:"):] {
			if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') && !(char >= 'A' && char <= 'F') {
				return false
			}
		}
		return true
	}
	return true
}
