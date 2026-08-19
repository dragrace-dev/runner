// Package gpu turns the operator's RUNNER_GPUS setting into a resolved,
// validated exposure policy for job containers (#65).
//
// The policy is the operator's ceiling: it says which GPUs, if any, a job
// container may see. It is resolved once at runner startup against the GPUs
// actually detected on the host (internal/system), so a bad setting kills
// the process with an explicit message instead of failing the first job that
// happens to arrive.
//
// The default is "none", which is the historical behaviour: no device, no
// device request, nothing added to the HostConfig at all.
package gpu

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"dragrace/internal/system"
)

// Mode is the shape of the operator's request.
type Mode string

const (
	// ModeNone exposes no GPU at all. Default, and the historical behaviour.
	ModeNone Mode = "none"
	// ModeAll exposes every GPU detected on the host.
	ModeAll Mode = "all"
	// ModeSelection exposes only the listed device indexes.
	ModeSelection Mode = "selection"
)

// Vendor names as reported by internal/system's GPU detection.
const (
	VendorNVIDIA = "nvidia"
	VendorAMD    = "amd"
	VendorApple  = "apple"
)

// EnvVar is the environment variable this package reads.
const EnvVar = "RUNNER_GPUS"

// amdKFDDevice is the ROCm compute node. It is not per-GPU: a single node
// arbitrates every AMD GPU, so it is mapped whenever any AMD GPU is exposed.
const amdKFDDevice = "/dev/kfd"

// amdFirstRenderNode is the conventional minor number of the first DRM render
// node (/dev/dri/renderD128 for card 0). Selection relies on that convention.
const amdFirstRenderNode = 128

// DeviceNode is a host device file that must be mapped into the container,
// together with the group that owns it. AMD/ROCm only: NVIDIA passthrough
// goes through the container runtime's device request mechanism instead and
// needs no explicit device mapping.
type DeviceNode struct {
	Path    string
	GroupID int
}

// Policy is the resolved decision handed to the executor. The zero value is
// a valid "expose nothing" policy, so an executor built without one behaves
// exactly as it did before this feature existed.
type Policy struct {
	// Mode is the resolved shape of the exposure.
	Mode Mode
	// Vendor is the GPU vendor this policy addresses ("" when Mode is none).
	Vendor string
	// Indexes are the selected device indexes. Empty for ModeNone, and for
	// ModeAll on NVIDIA (where "all" is expressed as a count, not a list).
	Indexes []int
	// Devices are the host device nodes to map (AMD only).
	Devices []DeviceNode
	// Count is the number of GPUs this policy exposes (0 for ModeNone).
	// It is the ceiling a challenge's declared limits.gpu is validated
	// against (#66) — see config.ClampToRunnerCaps. Populated for every
	// enabled mode, including NVIDIA "all", where Indexes itself stays
	// empty (the device request there is expressed as a count, not a
	// list; see Mode's docs).
	Count int
}

// Enabled reports whether any GPU is exposed by this policy.
func (p Policy) Enabled() bool {
	return p.Mode != "" && p.Mode != ModeNone
}

// Allows reports whether this policy exposes the given device to job
// containers.
//
// Used to attribute GPU metric samples to a job's aggregates only for the
// cards its container was actually granted (#67): the metrics collector
// still samples every GPU visible to the runner process (nvidia-smi /
// rocm-smi / ioreg run on the host, not inside the container, and cannot be
// scoped to one container's view), so filtering happens here, after the
// fact, against the same policy that governs what the container itself was
// allowed to see (#65). A disabled policy (RUNNER_GPUS=none) allows nothing,
// by construction: vendor is "" and matches no real sample.
func (p Policy) Allows(vendor string, deviceID int) bool {
	if !p.Enabled() || vendor != p.Vendor {
		return false
	}
	if p.Mode == ModeAll {
		// "all" is expressed on NVIDIA as a device count, not a list (see
		// Indexes' docs), so a vendor match is the whole test.
		return true
	}
	for _, index := range p.Indexes {
		if index == deviceID {
			return true
		}
	}
	return false
}

// GroupIDs returns the deduplicated, sorted supplementary group ids the
// container needs in order to open the mapped device nodes (AMD only).
func (p Policy) GroupIDs() []string {
	seen := map[int]bool{}
	ids := []int{}
	for _, device := range p.Devices {
		if seen[device.GroupID] {
			continue
		}
		seen[device.GroupID] = true
		ids = append(ids, device.GroupID)
	}
	sort.Ints(ids)
	groups := make([]string, 0, len(ids))
	for _, id := range ids {
		groups = append(groups, strconv.Itoa(id))
	}
	return groups
}

// Describe renders the policy for the startup log.
func (p Policy) Describe() string {
	if !p.Enabled() {
		return "none (no GPU exposed to job containers)"
	}
	if p.Mode == ModeAll {
		return fmt.Sprintf("all %s GPUs", p.Vendor)
	}
	indexes := make([]string, 0, len(p.Indexes))
	for _, index := range p.Indexes {
		indexes = append(indexes, strconv.Itoa(index))
	}
	return fmt.Sprintf("%s GPU(s) %s", p.Vendor, strings.Join(indexes, ","))
}

// DeviceProbe reports the group owning a host device node, or an error when
// the node does not exist. Injected so the AMD path is unit-testable without
// an AMD host.
type DeviceProbe func(path string) (groupID int, err error)

// request is the parsed but not yet validated RUNNER_GPUS value.
type request struct {
	mode    Mode
	indexes []int
}

// parseRequest reads the raw RUNNER_GPUS value. It validates syntax only;
// whether the requested devices exist is decided in Resolve.
func parseRequest(raw string) (request, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "", "none":
		return request{mode: ModeNone}, nil
	case "all":
		return request{mode: ModeAll}, nil
	}

	seen := map[int]bool{}
	indexes := []int{}
	for _, field := range strings.Split(value, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			return request{}, fmt.Errorf(
				"%s=%q: empty device index in the list (expected \"none\", \"all\", or a comma-separated list like \"0,1\")", EnvVar, raw)
		}
		index, err := strconv.Atoi(field)
		if err != nil {
			return request{}, fmt.Errorf(
				"%s=%q: %q is not a device index (expected \"none\", \"all\", or a comma-separated list like \"0,1\")", EnvVar, raw, field)
		}
		if index < 0 {
			return request{}, fmt.Errorf("%s=%q: device index %d is negative", EnvVar, raw, index)
		}
		if seen[index] {
			return request{}, fmt.Errorf("%s=%q: device index %d is listed twice", EnvVar, raw, index)
		}
		seen[index] = true
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return request{mode: ModeSelection, indexes: indexes}, nil
}

// Requested reports whether a raw RUNNER_GPUS value asks for anything other
// than "none". Callers use it to skip hardware detection entirely on the
// default path. A syntactically invalid value counts as requested, so that
// Resolve gets to report the error rather than silently ignoring it.
func Requested(raw string) bool {
	parsed, err := parseRequest(raw)
	if err != nil {
		return true
	}
	return parsed.mode != ModeNone
}

// Resolve validates the operator's request against the GPUs actually
// detected on this host and returns the policy the executor will apply.
//
// Called at startup: any error here is fatal by design (#65). A runner that
// was told to expose GPU 3 on a two-GPU box must not start and then fail the
// first job it is handed.
func Resolve(raw string, gpus []system.GPUInfo) (Policy, error) {
	return resolve(raw, gpus, statDeviceGroup)
}

func resolve(raw string, gpus []system.GPUInfo, probe DeviceProbe) (Policy, error) {
	parsed, err := parseRequest(raw)
	if err != nil {
		return Policy{}, err
	}
	if parsed.mode == ModeNone {
		return Policy{Mode: ModeNone}, nil
	}

	vendor, vendorGPUs, err := singleVendor(gpus)
	if err != nil {
		return Policy{}, fmt.Errorf("%s=%q: %w", EnvVar, raw, err)
	}

	switch vendor {
	case VendorNVIDIA:
		return resolveNVIDIA(raw, parsed, vendorGPUs)
	case VendorAMD:
		return resolveAMD(raw, parsed, vendorGPUs, probe)
	case VendorApple:
		// Docker Desktop runs the daemon inside a VM with no Metal access:
		// there is no passthrough mechanism to offer. Refusing loudly beats
		// starting containers that quietly compute on the CPU.
		return Policy{}, fmt.Errorf(
			"%s=%q: Apple Silicon GPUs cannot be exposed to containers (Docker runs in a VM with no Metal access); "+
				"use %s=none, or run the challenge natively", EnvVar, raw, EnvVar)
	default:
		return Policy{}, fmt.Errorf("%s=%q: unsupported GPU vendor %q", EnvVar, raw, vendor)
	}
}

// singleVendor rejects hosts the policy cannot describe unambiguously. Device
// indexes are per-vendor (nvidia-smi index 0 and rocm-smi card0 are different
// cards), so a mixed-vendor host has no single meaning for "0,1" or "all".
func singleVendor(gpus []system.GPUInfo) (string, []system.GPUInfo, error) {
	if len(gpus) == 0 {
		return "", nil, fmt.Errorf("no GPU was detected on this host (use %s=none on CPU-only hosts)", EnvVar)
	}
	vendors := []string{}
	seen := map[string]bool{}
	for _, gpu := range gpus {
		if !seen[gpu.Vendor] {
			seen[gpu.Vendor] = true
			vendors = append(vendors, gpu.Vendor)
		}
	}
	if len(vendors) > 1 {
		sort.Strings(vendors)
		return "", nil, fmt.Errorf(
			"this host mixes GPU vendors (%s) and device indexes are vendor-specific; "+
				"GPU exposure is not supported on mixed-vendor hosts, use %s=none",
			strings.Join(vendors, ", "), EnvVar)
	}
	return vendors[0], gpus, nil
}

func resolveNVIDIA(raw string, parsed request, gpus []system.GPUInfo) (Policy, error) {
	if parsed.mode == ModeAll {
		return Policy{Mode: ModeAll, Vendor: VendorNVIDIA, Count: len(gpus)}, nil
	}
	if err := checkIndexes(raw, parsed.indexes, gpus); err != nil {
		return Policy{}, err
	}
	return Policy{Mode: ModeSelection, Vendor: VendorNVIDIA, Indexes: parsed.indexes, Count: len(parsed.indexes)}, nil
}

// resolveAMD builds the ROCm exposure. AMD has no device-request driver: the
// container needs the compute node (/dev/kfd) plus the DRM render node of
// every exposed card, and the group that owns them.
func resolveAMD(raw string, parsed request, gpus []system.GPUInfo, probe DeviceProbe) (Policy, error) {
	indexes := parsed.indexes
	if parsed.mode == ModeAll {
		indexes = nil
		for _, gpu := range gpus {
			indexes = append(indexes, gpu.DeviceID)
		}
		sort.Ints(indexes)
	} else if err := checkIndexes(raw, indexes, gpus); err != nil {
		return Policy{}, err
	}

	paths := []string{amdKFDDevice}
	for _, index := range indexes {
		paths = append(paths, fmt.Sprintf("/dev/dri/renderD%d", amdFirstRenderNode+index))
	}

	devices := make([]DeviceNode, 0, len(paths))
	for _, path := range paths {
		groupID, err := probe(path)
		if err != nil {
			// Detected an AMD card but the kernel interface it needs is
			// missing: say so instead of starting a container that would
			// find no GPU inside.
			return Policy{}, fmt.Errorf(
				"%s=%q: AMD GPU passthrough needs the host device %s, which is unavailable (%v); "+
					"check that the amdgpu kernel driver and ROCm are installed, or use %s=none",
				EnvVar, raw, path, err, EnvVar)
		}
		devices = append(devices, DeviceNode{Path: path, GroupID: groupID})
	}

	mode := parsed.mode
	return Policy{Mode: mode, Vendor: VendorAMD, Indexes: indexes, Devices: devices, Count: len(indexes)}, nil
}

func checkIndexes(raw string, indexes []int, gpus []system.GPUInfo) error {
	available := map[int]bool{}
	present := make([]string, 0, len(gpus))
	for _, gpu := range gpus {
		available[gpu.DeviceID] = true
		present = append(present, strconv.Itoa(gpu.DeviceID))
	}
	for _, index := range indexes {
		if !available[index] {
			return fmt.Errorf(
				"%s=%q: no GPU with index %d on this host (detected indexes: %s)",
				EnvVar, raw, index, strings.Join(present, ", "))
		}
	}
	return nil
}
