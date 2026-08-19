package docker

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// `docker info` describes the machine the daemon runs on, never the cgroup the
// caller lives in. In the docker-out-of-docker layout that is exactly right:
// job sandboxes are siblings of the runner container on the host, so the host's
// capacity is the real ceiling and the runner container's own limits do not
// constrain them at all.
//
// Under docker-in-docker (#70) it is wrong. The daemon runs inside the runner
// container and reads the *host's* /proc/meminfo and CPU list, but every job
// container it creates is nested in the runner container's cgroup and can
// never exceed it. An operator who caps the runner at, say, 4 GB still sees
// MemTotal for the whole machine, so clampResourcesToHost (#58) sizes a phase
// far above what the cgroup allows and the job dies by OOM-kill — the silent
// failure mode #58 exists to replace with an explicit error.
//
// So under nesting the effective capacity is min(daemon-reported, own cgroup).
// The mode is declared, not guessed: the dind image sets RUNNER_DOCKER_NESTED,
// and nothing else does.
const (
	nestedDaemonEnv = "RUNNER_DOCKER_NESTED"

	// cgroupRoot is where a container's own cgroup files are mounted. It is a
	// variable only so tests can point it at a fixture directory.
	defaultCgroupRoot = "/sys/fs/cgroup"
)

var (
	cgroupRoot = defaultCgroupRoot

	// The clamp is evaluated once per phase; log it once per process.
	cgroupClampLogged sync.Once
)

// nestedDaemon reports whether the Docker daemon runs inside this container,
// making this container's cgroup limits a hard ceiling for job sandboxes.
func nestedDaemon() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(nestedDaemonEnv))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

// clampToOwnCgroup narrows the daemon-reported capacity to this process's own
// cgroup limits when the daemon is nested. Outside nesting, and for any limit
// the cgroup does not set, the daemon's numbers are returned untouched.
func clampToOwnCgroup(nanoCPUs, memBytes int64) (int64, int64) {
	if !nestedDaemon() {
		return nanoCPUs, memBytes
	}

	cgroupCPU, cgroupMem := cgroupCapacity(cgroupRoot)

	clampedCPU, clampedMem := nanoCPUs, memBytes
	if cgroupCPU > 0 && cgroupCPU < clampedCPU {
		clampedCPU = cgroupCPU
	}
	if cgroupMem > 0 && (clampedMem <= 0 || cgroupMem < clampedMem) {
		clampedMem = cgroupMem
	}

	if clampedCPU != nanoCPUs || clampedMem != memBytes {
		cgroupClampLogged.Do(func() {
			log.Printf("ℹ️  Nested Docker daemon: capping job resources at this container's cgroup limits "+
				"(%.2f CPUs / %d bytes) instead of the daemon-reported machine capacity (%.2f CPUs / %d bytes)",
				float64(clampedCPU)/1e9, clampedMem, float64(nanoCPUs)/1e9, memBytes)
		})
	}

	return clampedCPU, clampedMem
}

// cgroupCapacity reads the CPU and memory ceilings of the current cgroup.
// Either value is 0 when the cgroup sets no limit or the files cannot be read:
// an unreadable cgroup must not shrink capacity to nothing, it must simply not
// be taken into account.
func cgroupCapacity(root string) (nanoCPUs int64, memBytes int64) {
	// cgroup v2 (unified): all controllers under one directory.
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err == nil {
		nanoCPUs = cgroupV2CPU(root)
		memBytes = readCgroupBytes(filepath.Join(root, "memory.max"))
		if cpuset := readCPUSetCount(filepath.Join(root, "cpuset.cpus.effective")); cpuset > 0 {
			if nanoCPUs == 0 || cpuset < nanoCPUs {
				nanoCPUs = cpuset
			}
		}
		return nanoCPUs, memBytes
	}

	// cgroup v1: one directory per controller.
	nanoCPUs = cgroupV1CPU(root)
	memBytes = readCgroupBytes(filepath.Join(root, "memory", "memory.limit_in_bytes"))
	if cpuset := readCPUSetCount(filepath.Join(root, "cpuset", "cpuset.cpus")); cpuset > 0 {
		if nanoCPUs == 0 || cpuset < nanoCPUs {
			nanoCPUs = cpuset
		}
	}
	return nanoCPUs, memBytes
}

// cgroupV2CPU reads cpu.max, whose format is "<quota|max> <period>" in
// microseconds. "max 100000" means unlimited.
func cgroupV2CPU(root string) int64 {
	fields := strings.Fields(readTrimmed(filepath.Join(root, "cpu.max")))
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || quota <= 0 {
		return 0
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return 0
	}
	return quota * 1_000_000_000 / period
}

// cgroupV1CPU reads cpu.cfs_quota_us / cpu.cfs_period_us. A quota of -1 means
// unlimited.
func cgroupV1CPU(root string) int64 {
	quota, err := strconv.ParseInt(readTrimmed(filepath.Join(root, "cpu", "cpu.cfs_quota_us")), 10, 64)
	if err != nil || quota <= 0 {
		return 0
	}
	period, err := strconv.ParseInt(readTrimmed(filepath.Join(root, "cpu", "cpu.cfs_period_us")), 10, 64)
	if err != nil || period <= 0 {
		return 0
	}
	return quota * 1_000_000_000 / period
}

// readCgroupBytes reads a byte limit. "max" (v2) and the sentinel values
// cgroup v1 uses for "no limit" (a page-aligned int64 maximum) both read as
// unlimited, i.e. 0.
func readCgroupBytes(path string) int64 {
	raw := readTrimmed(path)
	if raw == "" || raw == "max" {
		return 0
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	// cgroup v1 reports "unlimited" as an enormous page-aligned number
	// (0x7FFFFFFFFFFFF000 on 64-bit). Anything at or above a petabyte is that
	// sentinel, not a real machine.
	if value >= 1<<50 {
		return 0
	}
	return value
}

// readCPUSetCount counts the CPUs in a cpuset list such as "0-3,8" and returns
// the equivalent NanoCPUs. A cpuset pin is a ceiling Docker's own --cpus check
// does not know about.
func readCPUSetCount(path string) int64 {
	raw := readTrimmed(path)
	if raw == "" {
		return 0
	}
	var count int64
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		low, err := strconv.ParseInt(lo, 10, 64)
		if err != nil {
			return 0
		}
		if !isRange {
			count++
			continue
		}
		high, err := strconv.ParseInt(hi, 10, 64)
		if err != nil || high < low {
			return 0
		}
		count += high - low + 1
	}
	return count * 1_000_000_000
}

func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
