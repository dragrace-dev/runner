package testmode

import (
	"fmt"
	"os"
	"path/filepath"

	"dragrace/internal/config"

	"gopkg.in/yaml.v3"
)

// ValidateChallengeDir loads and validates a challenge dragrace.yaml from a directory.
func ValidateChallengeDir(dir string) (*config.ChallengeSpec, error) {
	yamlPath := filepath.Join(dir, "dragrace.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", yamlPath, err)
	}

	var spec config.ChallengeSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("invalid YAML in %s: %w", yamlPath, err)
	}

	// Validate required fields
	if spec.Type != "challenge" {
		return nil, fmt.Errorf("%s: type must be 'challenge', got '%s'", yamlPath, spec.Type)
	}
	if spec.Challenge.ID == "" {
		return nil, fmt.Errorf("%s: challenge.id is required", yamlPath)
	}
	if spec.Challenge.Name == "" {
		return nil, fmt.Errorf("%s: challenge.name is required", yamlPath)
	}

	// Validate init script exists
	if spec.Init != nil && spec.Init.Script != "" {
		scriptPath := filepath.Join(dir, spec.Init.Script)
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("init script not found: %s", scriptPath)
		}
	}

	// Validate validate script exists
	if spec.Validate != nil && spec.Validate.Script != "" {
		scriptPath := filepath.Join(dir, spec.Validate.Script)
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("validate script not found: %s", scriptPath)
		}
	}

	return &spec, nil
}

// ValidateSolutionDir loads and validates a solution dragrace.yaml from a directory.
func ValidateSolutionDir(dir string) (*config.SolutionConfig, error) {
	yamlPath := filepath.Join(dir, "dragrace.yaml")
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", yamlPath, err)
	}

	var spec config.SolutionConfig
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("invalid YAML in %s: %w", yamlPath, err)
	}

	// Validate required fields
	if spec.Type != "solution" {
		return nil, fmt.Errorf("%s: type must be 'solution', got '%s'", yamlPath, spec.Type)
	}
	if spec.Runtime.Docker == "" {
		return nil, fmt.Errorf("%s: runtime.docker is required", yamlPath)
	}
	if spec.Run.Script == "" {
		return nil, fmt.Errorf("%s: run.script is required", yamlPath)
	}

	// Validate build script exists
	if spec.Build != nil && spec.Build.Script != "" {
		scriptPath := filepath.Join(dir, spec.Build.Script)
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			return nil, fmt.Errorf("build script not found: %s", scriptPath)
		}
	}

	// Validate run script exists
	scriptPath := filepath.Join(dir, spec.Run.Script)
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("run script not found: %s", scriptPath)
	}

	return &spec, nil
}
