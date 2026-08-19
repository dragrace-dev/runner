package config

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseChallengeSpec parses YAML content into a ChallengeSpec
func ParseChallengeSpec(yamlContent []byte) (*ChallengeSpec, error) {
	if len(yamlContent) > maxYAMLBytes {
		return nil, fmt.Errorf("challenge YAML exceeds %d-byte limit", maxYAMLBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(yamlContent))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return nil, err
	}
	if err := validateYAMLNode(&node, 0); err != nil {
		return nil, fmt.Errorf("unsafe challenge YAML: %w", err)
	}
	var spec ChallengeSpec
	if err := decodeStrictNode(&node, &spec); err != nil {
		return nil, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("challenge YAML must contain exactly one document")
	}
	if err := ValidateChallengeSpec(&spec); err != nil {
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
