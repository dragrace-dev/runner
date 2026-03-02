package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// ParseChallengeSpec parses YAML content into a ChallengeSpec
func ParseChallengeSpec(yamlContent []byte) (*ChallengeSpec, error) {
	var spec ChallengeSpec
	if err := yaml.Unmarshal(yamlContent, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// ParseSolutionConfig parses a solution.yml file into a SolutionConfig
func ParseSolutionConfig(filePath string) (*SolutionConfig, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var config SolutionConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}
