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

// ValidateUnifiedDir loads a unified dragrace.yaml that may contain both
// a challenge and a solution document. It validates both parts and checks
// that all referenced scripts exist relative to dir.
func ValidateUnifiedDir(dir string) (*config.ChallengeSpec, *config.SolutionConfig, error) {
	yamlPath := filepath.Join(dir, "dragrace.yaml")
	challenge, solution, err := config.ParseUnifiedFile(yamlPath)
	if err != nil {
		return nil, nil, err
	}

	// Validate challenge part (if present)
	if challenge != nil {
		if challenge.Type != "challenge" {
			return nil, nil, fmt.Errorf("%s: challenge document type must be 'challenge'", yamlPath)
		}
		if challenge.Challenge.ID == "" {
			return nil, nil, fmt.Errorf("%s: challenge.id is required", yamlPath)
		}
		if challenge.Challenge.Name == "" {
			return nil, nil, fmt.Errorf("%s: challenge.name is required", yamlPath)
		}
		if challenge.Init != nil && challenge.Init.Script != "" {
			scriptPath := filepath.Join(dir, challenge.Init.Script)
			if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("init script not found: %s", scriptPath)
			}
		}
		if challenge.Validate != nil && challenge.Validate.Script != "" {
			scriptPath := filepath.Join(dir, challenge.Validate.Script)
			if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("validate script not found: %s", scriptPath)
			}
		}
	}

	// Validate solution part (if present)
	if solution != nil {
		if solution.Type != "solution" {
			return nil, nil, fmt.Errorf("%s: solution document type must be 'solution'", yamlPath)
		}
		if solution.Runtime.Docker == "" {
			return nil, nil, fmt.Errorf("%s: runtime.docker is required", yamlPath)
		}
		if solution.Run.Script == "" {
			return nil, nil, fmt.Errorf("%s: run.script is required", yamlPath)
		}
		if solution.Build != nil && solution.Build.Script != "" {
			scriptPath := filepath.Join(dir, solution.Build.Script)
			if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
				return nil, nil, fmt.Errorf("build script not found: %s", scriptPath)
			}
		}
		scriptPath := filepath.Join(dir, solution.Run.Script)
		if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("run script not found: %s", scriptPath)
		}
	}

	return challenge, solution, nil
}
