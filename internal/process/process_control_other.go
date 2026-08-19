//go:build !linux && !darwin

package process

import (
	"fmt"
	"os/exec"
	"runtime"
)

type groupUsage struct {
	MemoryBytes int64
	CPUPercent  float64
	Processes   int
}

func validateProcessControls() error {
	return fmt.Errorf("native executor is not supported on %s", runtime.GOOS)
}

func classifyFilesystemExit(_ error, _ bool) *LimitError { return nil }

func configureProcessGroup(_ *exec.Cmd)           {}
func terminateProcessGroup(pid int)               { _ = pid }
func processGroupUsage(_ int) (groupUsage, error) { return groupUsage{}, nil }
