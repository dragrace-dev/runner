package config

import (
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

// ParseSolutionConfig parses a dragrace.yaml file into a SolutionConfig.
// It supports both single-document (solution only) and unified files
// (challenge + solution). In unified files, only the solution document
// is extracted — the challenge section is ignored for security.
func ParseSolutionConfig(filePath string) (*SolutionConfig, error) {
	return ExtractSolutionFromFile(filePath)
}
