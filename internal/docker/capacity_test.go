package docker

import (
	"os"
	"path/filepath"
	"testing"
)

// Task #70: under docker-in-docker the daemon reports the machine's CPUs and
// MemTotal, but job containers are nested in the runner container's cgroup and
// cannot exceed it. These tests cover the reading and the clamping without a
// daemon, and without a real /sys/fs/cgroup, by pointing cgroupRoot at a
// fixture tree.

func writeCgroupFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// v2Root builds a cgroup v2 fixture. The marker file is what cgroupCapacity
// uses to tell v2 from v1.
func v2Root(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeCgroupFile(t, root, "cgroup.controllers", "cpuset cpu memory pids")
	for name, content := range files {
		writeCgroupFile(t, root, name, content)
	}
	return root
}

func TestCgroupCapacityReadsV2Limits(t *testing.T) {
	root := v2Root(t, map[string]string{
		"cpu.max":    "150000 100000",
		"memory.max": "4294967296",
	})

	nanoCPUs, memBytes := cgroupCapacity(root)
	if nanoCPUs != 1_500_000_000 {
		t.Fatalf("expected 1.5 CPUs, got %d", nanoCPUs)
	}
	if memBytes != 4*1024*1024*1024 {
		t.Fatalf("expected 4GiB, got %d", memBytes)
	}
}

func TestCgroupCapacityTreatsV2MaxAsUnlimited(t *testing.T) {
	root := v2Root(t, map[string]string{
		"cpu.max":    "max 100000",
		"memory.max": "max",
	})

	nanoCPUs, memBytes := cgroupCapacity(root)
	if nanoCPUs != 0 || memBytes != 0 {
		t.Fatalf("expected no limit reported, got %d CPUs / %d bytes", nanoCPUs, memBytes)
	}
}

func TestCgroupCapacityUsesCPUSetWhenTighterThanQuota(t *testing.T) {
	root := v2Root(t, map[string]string{
		"cpu.max":               "max 100000",
		"cpuset.cpus.effective": "0-2,7",
	})

	nanoCPUs, _ := cgroupCapacity(root)
	if nanoCPUs != 4_000_000_000 {
		t.Fatalf("expected 4 CPUs from the cpuset, got %d", nanoCPUs)
	}
}

func TestCgroupCapacityReadsV1Limits(t *testing.T) {
	root := t.TempDir() // no cgroup.controllers: v1 layout
	writeCgroupFile(t, root, "cpu/cpu.cfs_quota_us", "200000")
	writeCgroupFile(t, root, "cpu/cpu.cfs_period_us", "100000")
	writeCgroupFile(t, root, "memory/memory.limit_in_bytes", "2147483648")

	nanoCPUs, memBytes := cgroupCapacity(root)
	if nanoCPUs != 2_000_000_000 {
		t.Fatalf("expected 2 CPUs, got %d", nanoCPUs)
	}
	if memBytes != 2*1024*1024*1024 {
		t.Fatalf("expected 2GiB, got %d", memBytes)
	}
}

// cgroup v1 spells "unlimited" as a page-aligned int64 maximum, which must not
// be mistaken for a machine with a petabyte of RAM.
func TestCgroupCapacityTreatsV1SentinelAsUnlimited(t *testing.T) {
	root := t.TempDir()
	writeCgroupFile(t, root, "cpu/cpu.cfs_quota_us", "-1")
	writeCgroupFile(t, root, "memory/memory.limit_in_bytes", "9223372036854771712")

	nanoCPUs, memBytes := cgroupCapacity(root)
	if nanoCPUs != 0 || memBytes != 0 {
		t.Fatalf("expected no limit reported, got %d CPUs / %d bytes", nanoCPUs, memBytes)
	}
}

func TestCgroupCapacityIgnoresUnreadableCgroup(t *testing.T) {
	nanoCPUs, memBytes := cgroupCapacity(filepath.Join(t.TempDir(), "does-not-exist"))
	if nanoCPUs != 0 || memBytes != 0 {
		t.Fatalf("an unreadable cgroup must report no limit, got %d CPUs / %d bytes", nanoCPUs, memBytes)
	}
}

// useCgroupFixture points the package at a fixture root and declares nesting
// for the duration of one test.
func useCgroupFixture(t *testing.T, root string) {
	t.Helper()
	previous := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = previous })
	t.Setenv(nestedDaemonEnv, "true")
}

func TestClampToOwnCgroupIsInertWithoutNesting(t *testing.T) {
	root := v2Root(t, map[string]string{"cpu.max": "100000 100000", "memory.max": "1073741824"})
	previous := cgroupRoot
	cgroupRoot = root
	t.Cleanup(func() { cgroupRoot = previous })
	t.Setenv(nestedDaemonEnv, "")

	// Docker-out-of-docker: sandboxes are siblings on the host, so the runner
	// container's own cgroup must NOT shrink what jobs are allowed.
	nanoCPUs, memBytes := clampToOwnCgroup(16_000_000_000, 64*1024*1024*1024)
	if nanoCPUs != 16_000_000_000 || memBytes != 64*1024*1024*1024 {
		t.Fatalf("expected daemon capacity untouched, got %d CPUs / %d bytes", nanoCPUs, memBytes)
	}
}

func TestClampToOwnCgroupNarrowsNestedCapacity(t *testing.T) {
	useCgroupFixture(t, v2Root(t, map[string]string{
		"cpu.max":    "400000 100000",
		"memory.max": "8589934592",
	}))

	nanoCPUs, memBytes := clampToOwnCgroup(64_000_000_000, 256*1024*1024*1024)
	if nanoCPUs != 4_000_000_000 {
		t.Fatalf("expected the cgroup's 4 CPUs, got %d", nanoCPUs)
	}
	if memBytes != 8*1024*1024*1024 {
		t.Fatalf("expected the cgroup's 8GiB, got %d", memBytes)
	}
}

func TestClampToOwnCgroupKeepsDaemonValueWhenCgroupIsLooser(t *testing.T) {
	useCgroupFixture(t, v2Root(t, map[string]string{
		"cpu.max":    "6400000 100000", // 64 CPUs
		"memory.max": "274877906944",   // 256GiB
	}))

	nanoCPUs, memBytes := clampToOwnCgroup(4_000_000_000, 8*1024*1024*1024)
	if nanoCPUs != 4_000_000_000 || memBytes != 8*1024*1024*1024 {
		t.Fatalf("a looser cgroup must not raise capacity, got %d CPUs / %d bytes", nanoCPUs, memBytes)
	}
}

func TestClampToOwnCgroupKeepsDaemonValueWhenCgroupIsUnlimited(t *testing.T) {
	useCgroupFixture(t, v2Root(t, map[string]string{"cpu.max": "max 100000", "memory.max": "max"}))

	nanoCPUs, memBytes := clampToOwnCgroup(4_000_000_000, 8*1024*1024*1024)
	if nanoCPUs != 4_000_000_000 || memBytes != 8*1024*1024*1024 {
		t.Fatalf("an unlimited cgroup must change nothing, got %d CPUs / %d bytes", nanoCPUs, memBytes)
	}
}
