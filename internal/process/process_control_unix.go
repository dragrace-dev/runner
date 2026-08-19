//go:build linux || darwin

package process

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type groupUsage struct {
	MemoryBytes int64
	CPUPercent  float64
	Processes   int
}

func validateProcessControls() error {
	if _, err := exec.LookPath("ps"); err != nil {
		return fmt.Errorf("native executor requires ps for resource enforcement: %w", err)
	}
	return nil
}

func classifyFilesystemExit(err error, diskLimited bool) *LimitError {
	if !diskLimited || err == nil {
		return nil
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		return nil
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return nil
	}
	if status.Signal() == syscall.SIGXFSZ || status.ExitStatus() == 128+int(syscall.SIGXFSZ) {
		return &LimitError{Kind: LimitFilesystem}
	}
	return nil
}

func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcessGroup(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func processGroupUsage(pgid int) (groupUsage, error) {
	out, err := exec.Command("ps", "-axo", "pgid=,rss=,pcpu=").Output()
	if err != nil {
		return groupUsage{}, fmt.Errorf("inspect process group: %w", err)
	}

	var usage groupUsage
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != strconv.Itoa(pgid) {
			continue
		}
		rssKB, _ := strconv.ParseInt(fields[1], 10, 64)
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		usage.MemoryBytes += rssKB * 1024
		usage.CPUPercent += cpu
		usage.Processes++
	}
	return usage, nil
}
