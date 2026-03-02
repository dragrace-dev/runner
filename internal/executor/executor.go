package executor

import (
	"context"
	"dragrace/internal/metrics"
)

// Executor defines the interface that all execution backends must implement.
// Docker, Process (native), and future executors (Podman, Firecracker, etc.)
// all implement this interface.
type Executor interface {
	// RunScript executes a script and waits for completion. Returns logs.
	RunScript(ctx context.Context, opts *RunOptions) (logs string, err error)

	// RunMeasured executes a script and collects metrics during execution.
	// Each executor owns its metrics strategy internally.
	RunMeasured(ctx context.Context, opts *RunOptions) (*metrics.RunMetrics, error)

	// EnsureDataDir creates the data storage (volume or directory) if it doesn't exist.
	EnsureDataDir(ctx context.Context, name string) error

	// DataDirExists checks whether the data storage exists.
	DataDirExists(ctx context.Context, name string) bool

	// RemoveDataDir removes the data storage.
	RemoveDataDir(ctx context.Context, name string) error

	// Close releases resources held by the executor.
	Close() error
}

// RunOptions configures script execution.
type RunOptions struct {
	// Image is the Docker image to use (ignored by process executor).
	Image string

	// ScriptPath is the script to execute, relative to RepoDir.
	ScriptPath string

	// RepoDir is the cloned repository directory.
	RepoDir string

	// DataDir is the data storage identifier (volume name for Docker, dir for process).
	DataDir string

	// ReadOnlyData controls whether the data storage is mounted read-only.
	ReadOnlyData bool

	// Stdout is an optional file path to redirect stdout to (for measured output).
	Stdout string

	// Limits sets resource constraints for execution.
	Limits *ResourceLimits
}

// ResourceLimits defines resource constraints for script execution.
type ResourceLimits struct {
	MemoryBytes int64
	CPUNano     int64
	Timeout     int // seconds
}

// DataDirName generates a unique data directory name for a challenge.
// Shared across executors — same naming regardless of backend.
func DataDirName(challengeID, ref string) string {
	// Import-free: keep as simple format, hash done by caller if needed
	return "dragrace-" + challengeID + "-" + ref
}
