package docker

import (
	"strings"
	"testing"

	"dragrace/internal/executor"

	"github.com/docker/docker/api/types/container"
)

// Task #58: a hardcoded 32GB/8-CPU run-phase default made every host with
// fewer than 8 CPUs fail to create a container at all (Docker rejects
// NanoCPUs above the host's actual CPU count). These tests cover the fix
// without needing a real Docker daemon: clampResourcesToHost is the pure
// decision logic resolveResources delegates to after querying host capacity.

func twoCPUHost() (nanoCPUs, memBytes int64) {
	return 2_000_000_000, 4 * 1024 * 1024 * 1024
}

func TestClampResourcesToHostScalesDefaultsDownToModestHost(t *testing.T) {
	hostCPU, hostMem := twoCPUHost()
	resources, err := clampResourcesToHost(
		container.Resources{Memory: defaultRunMemory, NanoCPUs: defaultRunCPU},
		&executor.RunOptions{},
		hostCPU, hostMem,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resources.NanoCPUs != hostCPU {
		t.Fatalf("expected NanoCPUs clamped to host capacity %d, got %d", hostCPU, resources.NanoCPUs)
	}
	if resources.Memory != hostMem {
		t.Fatalf("expected Memory clamped to host capacity %d, got %d", hostMem, resources.Memory)
	}
}

func TestClampResourcesToHostLeavesDefaultsUntouchedWhenHostHasHeadroom(t *testing.T) {
	hostCPU := int64(16_000_000_000)
	hostMem := int64(64 * 1024 * 1024 * 1024)
	resources, err := clampResourcesToHost(
		container.Resources{Memory: defaultRunMemory, NanoCPUs: defaultRunCPU},
		&executor.RunOptions{},
		hostCPU, hostMem,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resources.NanoCPUs != defaultRunCPU {
		t.Fatalf("expected default NanoCPUs %d untouched, got %d", defaultRunCPU, resources.NanoCPUs)
	}
	if resources.Memory != defaultRunMemory {
		t.Fatalf("expected default Memory %d untouched, got %d", defaultRunMemory, resources.Memory)
	}
}

func TestClampResourcesToHostRejectsExplicitCPURequestExceedingHost(t *testing.T) {
	hostCPU, hostMem := twoCPUHost()
	_, err := clampResourcesToHost(
		container.Resources{Memory: defaultRunMemory, NanoCPUs: defaultRunCPU},
		&executor.RunOptions{Limits: &executor.ResourceLimits{CPUNano: 4_000_000_000}},
		hostCPU, hostMem,
	)
	if err == nil {
		t.Fatal("expected an error for a cpu request exceeding host capacity, got nil")
	}
	if !strings.Contains(err.Error(), "cpu") {
		t.Fatalf("expected error to mention the cpu limit, got: %v", err)
	}
}

func TestClampResourcesToHostRejectsExplicitMemoryRequestExceedingHost(t *testing.T) {
	hostCPU, hostMem := twoCPUHost()
	_, err := clampResourcesToHost(
		container.Resources{Memory: defaultRunMemory, NanoCPUs: defaultRunCPU},
		&executor.RunOptions{Limits: &executor.ResourceLimits{MemoryBytes: hostMem * 2}},
		hostCPU, hostMem,
	)
	if err == nil {
		t.Fatal("expected an error for a memory request exceeding host capacity, got nil")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Fatalf("expected error to mention the memory limit, got: %v", err)
	}
}

func TestClampResourcesToHostAcceptsExplicitRequestWithinHostCapacity(t *testing.T) {
	hostCPU, hostMem := twoCPUHost()
	resources, err := clampResourcesToHost(
		container.Resources{Memory: defaultRunMemory, NanoCPUs: defaultRunCPU},
		&executor.RunOptions{Limits: &executor.ResourceLimits{CPUNano: 1_000_000_000, MemoryBytes: 1024 * 1024 * 1024}},
		hostCPU, hostMem,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resources.NanoCPUs != 1_000_000_000 {
		t.Fatalf("expected requested NanoCPUs 1_000_000_000, got %d", resources.NanoCPUs)
	}
	if resources.Memory != 1024*1024*1024 {
		t.Fatalf("expected requested Memory 1GB, got %d", resources.Memory)
	}
}
