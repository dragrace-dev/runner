package config

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"
)

// typeHeader is used to peek at the "type" field of a YAML document.
type typeHeader struct {
	Type string `yaml:"type"`
}

// ParseUnifiedFile reads a dragrace.yaml that may contain one or two YAML
// documents (separated by ---). It returns both the ChallengeSpec and the
// SolutionConfig. Either may be nil if the corresponding document is not
// present. This is the TRUSTED parser — use only in test mode where the
// user is both the challenge author and the solution author.
func ParseUnifiedFile(path string) (*ChallengeSpec, *SolutionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var challenge *ChallengeSpec
	var solution *SolutionConfig

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		// First, decode into a raw yaml.Node to preserve the document.
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}
			return nil, nil, fmt.Errorf("invalid YAML in %s: %w", path, err)
		}

		// Peek at the type field.
		var header typeHeader
		if err := node.Decode(&header); err != nil {
			return nil, nil, fmt.Errorf("cannot parse document in %s: %w", path, err)
		}

		switch header.Type {
		case "challenge":
			if challenge != nil {
				return nil, nil, fmt.Errorf("%s: duplicate 'challenge' document", path)
			}
			challenge = &ChallengeSpec{}
			if err := node.Decode(challenge); err != nil {
				return nil, nil, fmt.Errorf("%s: invalid challenge document: %w", path, err)
			}

		case "solution":
			if solution != nil {
				return nil, nil, fmt.Errorf("%s: duplicate 'solution' document", path)
			}
			solution = &SolutionConfig{}
			if err := node.Decode(solution); err != nil {
				return nil, nil, fmt.Errorf("%s: invalid solution document: %w", path, err)
			}

		default:
			return nil, nil, fmt.Errorf("%s: unknown document type '%s' (expected 'challenge' or 'solution')", path, header.Type)
		}
	}

	if challenge == nil && solution == nil {
		return nil, nil, fmt.Errorf("%s: no valid documents found", path)
	}

	return challenge, solution, nil
}

// ExtractSolutionFromFile reads a dragrace.yaml and extracts ONLY the
// solution document, ignoring any challenge document. This is the UNTRUSTED
// parser — use in production when the challenge spec is provided by the
// backend and the submitter's file should not be able to override it.
func ExtractSolutionFromFile(path string) (*SolutionConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", path, err)
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("invalid YAML in %s: %w", path, err)
		}

		var header typeHeader
		if err := node.Decode(&header); err != nil {
			return nil, fmt.Errorf("cannot parse document in %s: %w", path, err)
		}

		if header.Type == "solution" {
			sol := &SolutionConfig{}
			if err := node.Decode(sol); err != nil {
				return nil, fmt.Errorf("%s: invalid solution document: %w", path, err)
			}
			return sol, nil
		}
		// Silently skip any non-solution documents (including challenge).
	}

	return nil, fmt.Errorf("%s: no 'solution' document found", path)
}
