package config

// SolutionConfig represents the solution configuration provided by participants
// This is loaded from the solution repository's dragrace.yaml
type SolutionConfig struct {
	Version string        `yaml:"version"`
	Type    string        `yaml:"type"` // Must be "solution"
	Runtime RuntimeConfig `yaml:"runtime"`
	Build   *PhaseConfig  `yaml:"build,omitempty"`
	Run     RunConfig     `yaml:"run"`
}

// RuntimeConfig specifies the Docker image to use
type RuntimeConfig struct {
	Docker string `yaml:"docker"` // Docker image (e.g., "eclipse-temurin:21-jdk")
}

// RunConfig specifies how to execute the solution
type RunConfig struct {
	Script string `yaml:"script"`           // Script path (e.g., "scripts/run.sh")
	Stdout string `yaml:"stdout,omitempty"` // Output file for stdout (e.g., "/data/output.txt")
}
