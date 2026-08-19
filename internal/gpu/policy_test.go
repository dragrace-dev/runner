package gpu

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"dragrace/internal/system"
)

// Task #65: RUNNER_GPUS is the operator's GPU exposure ceiling. It is
// resolved once at startup against the GPUs actually detected on the host,
// so a bad value kills the runner with an explicit message instead of
// failing the first job that arrives. These tests are pure: no Docker
// daemon, no GPU, and the AMD device probe is injected.

func nvidiaHost(count int) []system.GPUInfo {
	gpus := make([]system.GPUInfo, 0, count)
	for i := 0; i < count; i++ {
		gpus = append(gpus, system.GPUInfo{DeviceID: i, Vendor: VendorNVIDIA, Model: "NVIDIA Test GPU"})
	}
	return gpus
}

func amdHost(count int) []system.GPUInfo {
	gpus := make([]system.GPUInfo, 0, count)
	for i := 0; i < count; i++ {
		gpus = append(gpus, system.GPUInfo{DeviceID: i, Vendor: VendorAMD, Model: "AMD Test GPU"})
	}
	return gpus
}

func appleHost() []system.GPUInfo {
	return []system.GPUInfo{{DeviceID: 0, Vendor: VendorApple, Model: "Apple M-series GPU"}}
}

// presentDevices answers for a fixed set of host device nodes, so the AMD
// path can be exercised on a machine that has none of them.
func presentDevices(paths map[string]int) DeviceProbe {
	return func(path string) (int, error) {
		gid, ok := paths[path]
		if !ok {
			return 0, errors.New("no such file or directory")
		}
		return gid, nil
	}
}

func TestResolveDefaultsToNoneAndExposesNothing(t *testing.T) {
	for _, raw := range []string{"", "none", "NONE", "  none  "} {
		policy, err := resolve(raw, nvidiaHost(2), presentDevices(nil))
		if err != nil {
			t.Fatalf("%q: unexpected error: %v", raw, err)
		}
		if policy.Enabled() {
			t.Fatalf("%q: expected no GPU exposure, got %#v", raw, policy)
		}
		if policy.Mode != ModeNone {
			t.Fatalf("%q: expected mode none, got %q", raw, policy.Mode)
		}
		if len(policy.Devices) != 0 || len(policy.Indexes) != 0 {
			t.Fatalf("%q: expected an empty policy, got %#v", raw, policy)
		}
	}
}

func TestResolveNoneNeverConsultsTheHardware(t *testing.T) {
	// A CPU-only host must keep working exactly as before: "none" is valid
	// even when no GPU is detected at all.
	policy, err := resolve("none", nil, presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error on a CPU-only host: %v", err)
	}
	if policy.Enabled() {
		t.Fatalf("expected no exposure, got %#v", policy)
	}
}

func TestResolveAllOnNVIDIAHost(t *testing.T) {
	policy, err := resolve("all", nvidiaHost(2), presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy.Mode != ModeAll || policy.Vendor != VendorNVIDIA {
		t.Fatalf("expected an nvidia 'all' policy, got %#v", policy)
	}
	if len(policy.Indexes) != 0 {
		t.Fatalf("'all' is expressed as a count on NVIDIA, not a list; got indexes %v", policy.Indexes)
	}
}

func TestResolveSelectionOnNVIDIAHost(t *testing.T) {
	policy, err := resolve("1,0", nvidiaHost(3), presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy.Mode != ModeSelection || policy.Vendor != VendorNVIDIA {
		t.Fatalf("expected an nvidia selection policy, got %#v", policy)
	}
	if !reflect.DeepEqual(policy.Indexes, []int{0, 1}) {
		t.Fatalf("expected sorted indexes [0 1], got %v", policy.Indexes)
	}
}

func TestResolveRejectsIndexThatDoesNotExist(t *testing.T) {
	_, err := resolve("0,3", nvidiaHost(2), presentDevices(nil))
	if err == nil {
		t.Fatal("expected an error for a GPU index the host does not have")
	}
	for _, want := range []string{"3", "RUNNER_GPUS"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected the error to mention %q, got: %v", want, err)
		}
	}
}

func TestResolveRejectsGPURequestOnHostWithoutGPU(t *testing.T) {
	for _, raw := range []string{"all", "0"} {
		_, err := resolve(raw, nil, presentDevices(nil))
		if err == nil {
			t.Fatalf("%q: expected an error on a host with no GPU", raw)
		}
		if !strings.Contains(err.Error(), "no GPU was detected") {
			t.Fatalf("%q: expected a clear 'no GPU detected' error, got: %v", raw, err)
		}
	}
}

func TestResolveRejectsMalformedValues(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"every", "not a device index"},
		{"0,", "empty device index"},
		{"0,,1", "empty device index"},
		{"-1", "negative"},
		{"0,-2", "negative"},
		{"1,1", "listed twice"},
		{"0.5", "not a device index"},
	}
	for _, tc := range cases {
		_, err := resolve(tc.raw, nvidiaHost(4), presentDevices(nil))
		if err == nil {
			t.Errorf("%q: expected a parse error", tc.raw)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: expected the error to mention %q, got: %v", tc.raw, tc.want, err)
		}
	}
}

func TestResolveRefusesAppleSiliconExplicitly(t *testing.T) {
	// Docker Desktop runs in a VM with no Metal access: there is nothing to
	// pass through, so this must fail loudly rather than hand back a
	// GPU-less container.
	for _, raw := range []string{"all", "0"} {
		_, err := resolve(raw, appleHost(), presentDevices(nil))
		if err == nil {
			t.Fatalf("%q: expected Apple Silicon GPU exposure to be refused", raw)
		}
		if !strings.Contains(err.Error(), "Apple") || !strings.Contains(err.Error(), "none") {
			t.Fatalf("%q: expected an explicit Apple refusal pointing at RUNNER_GPUS=none, got: %v", raw, err)
		}
	}
}

func TestResolveAppleHostStillAcceptsNone(t *testing.T) {
	policy, err := resolve("none", appleHost(), presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy.Enabled() {
		t.Fatalf("expected no exposure, got %#v", policy)
	}
}

func TestResolveAllOnAMDHostMapsComputeAndRenderNodes(t *testing.T) {
	probe := presentDevices(map[string]int{
		"/dev/kfd":            44,
		"/dev/dri/renderD128": 44,
		"/dev/dri/renderD129": 44,
		"/dev/dri/renderD130": 44,
	})
	policy, err := resolve("all", amdHost(2), probe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if policy.Vendor != VendorAMD {
		t.Fatalf("expected an amd policy, got %#v", policy)
	}
	var paths []string
	for _, device := range policy.Devices {
		paths = append(paths, device.Path)
	}
	want := []string{"/dev/kfd", "/dev/dri/renderD128", "/dev/dri/renderD129"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("expected devices %v, got %v", want, paths)
	}
	if !reflect.DeepEqual(policy.GroupIDs(), []string{"44"}) {
		t.Fatalf("expected the owning group to be carried over once, got %v", policy.GroupIDs())
	}
}

func TestResolveSelectionOnAMDHostMapsOnlyTheSelectedRenderNode(t *testing.T) {
	probe := presentDevices(map[string]int{
		"/dev/kfd":            44,
		"/dev/dri/renderD128": 44,
		"/dev/dri/renderD129": 39,
	})
	policy, err := resolve("1", amdHost(2), probe)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var paths []string
	for _, device := range policy.Devices {
		paths = append(paths, device.Path)
	}
	want := []string{"/dev/kfd", "/dev/dri/renderD129"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("expected only the selected render node, got %v", paths)
	}
	if !reflect.DeepEqual(policy.GroupIDs(), []string{"39", "44"}) {
		t.Fatalf("expected both owning groups, sorted and deduplicated, got %v", policy.GroupIDs())
	}
}

func TestResolveAMDFailsClearlyWhenKernelInterfaceIsMissing(t *testing.T) {
	// An AMD card detected through rocm-smi but no /dev/kfd: exposing
	// nothing would give a container that silently computes on the CPU.
	_, err := resolve("all", amdHost(1), presentDevices(map[string]int{"/dev/dri/renderD128": 44}))
	if err == nil {
		t.Fatal("expected an error when /dev/kfd is missing")
	}
	if !strings.Contains(err.Error(), "/dev/kfd") || !strings.Contains(err.Error(), "amdgpu") {
		t.Fatalf("expected the error to name the missing device and the driver, got: %v", err)
	}
}

func TestResolveRejectsMixedVendorHosts(t *testing.T) {
	mixed := append(nvidiaHost(1), amdHost(1)...)
	_, err := resolve("0", mixed, presentDevices(nil))
	if err == nil {
		t.Fatal("expected mixed-vendor hosts to be refused: device indexes are vendor-specific")
	}
	if !strings.Contains(err.Error(), "mixes GPU vendors") {
		t.Fatalf("expected an explicit mixed-vendor error, got: %v", err)
	}
}

func TestRequestedSkipsDetectionOnlyForNone(t *testing.T) {
	cases := map[string]bool{
		"":        false,
		"none":    false,
		"NONE":    false,
		"all":     true,
		"0,1":     true,
		"garbage": true, // invalid: let Resolve report it rather than ignore it
	}
	for raw, want := range cases {
		if got := Requested(raw); got != want {
			t.Errorf("Requested(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestDescribeIsReadableForEachMode(t *testing.T) {
	none, _ := resolve("none", nvidiaHost(1), presentDevices(nil))
	if !strings.Contains(none.Describe(), "none") {
		t.Errorf("expected the none policy to describe itself as none, got %q", none.Describe())
	}
	all, _ := resolve("all", nvidiaHost(2), presentDevices(nil))
	if !strings.Contains(all.Describe(), "nvidia") {
		t.Errorf("expected the vendor in the description, got %q", all.Describe())
	}
	selection, _ := resolve("0,1", nvidiaHost(2), presentDevices(nil))
	if !strings.Contains(selection.Describe(), "0,1") {
		t.Errorf("expected the selected indexes in the description, got %q", selection.Describe())
	}
}

// Task #67: GPU metrics must be scoped to the cards a job's container was
// actually granted, not the whole host. Policy.Allows is the predicate that
// scoping is built on, so it is exercised directly against every mode.

func TestAllowsRejectsEverythingWhenPolicyIsDisabled(t *testing.T) {
	none, err := resolve("none", nvidiaHost(2), presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if none.Allows(VendorNVIDIA, 0) || none.Allows(VendorNVIDIA, 1) {
		t.Error("a disabled policy must not allow any device")
	}
	// The zero value is also a disabled policy (used by callers, e.g. the
	// process executor, that never resolve one at all).
	var zero Policy
	if zero.Allows(VendorNVIDIA, 0) {
		t.Error("the zero-value policy must not allow any device")
	}
}

func TestAllowsMatchesEveryDeviceOfTheVendorWhenAll(t *testing.T) {
	all, err := resolve("all", nvidiaHost(4), presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, index := range []int{0, 1, 2, 3} {
		if !all.Allows(VendorNVIDIA, index) {
			t.Errorf("expected index %d to be allowed under an 'all' policy", index)
		}
	}
	if all.Allows(VendorAMD, 0) {
		t.Error("an 'all' NVIDIA policy must not allow an AMD device")
	}
}

func TestAllowsMatchesOnlySelectedIndexes(t *testing.T) {
	selection, err := resolve("0,2", nvidiaHost(4), presentDevices(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cases := map[int]bool{0: true, 1: false, 2: true, 3: false}
	for index, want := range cases {
		if got := selection.Allows(VendorNVIDIA, index); got != want {
			t.Errorf("Allows(nvidia, %d) = %v, want %v", index, got, want)
		}
	}
}

func TestAllowsRejectsAMismatchedVendor(t *testing.T) {
	selection, err := resolve("0", amdHost(1), presentDevices(map[string]int{
		amdKFDDevice:          44,
		"/dev/dri/renderD128": 44,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selection.Allows(VendorNVIDIA, 0) {
		t.Error("an AMD policy must not allow an NVIDIA sample even at a matching index")
	}
	if !selection.Allows(VendorAMD, 0) {
		t.Error("expected the AMD policy to allow its own selected index")
	}
}
