package docker

import (
	"reflect"
	"strings"
	"testing"

	"dragrace/internal/gpu"

	"github.com/docker/docker/api/types/container"
)

// Task #65: these tests check what the executor actually asks Docker for,
// without a daemon — same approach as resources_test.go. The sandbox
// hardening assertions matter as much as the GPU ones: exposing a GPU must
// not weaken anything baseHostConfig/hardenSandbox put in place.

// sandboxedHostConfig reproduces what RunMeasured builds for a solution
// phase, so a test can assert on the real, fully hardened HostConfig.
func sandboxedHostConfig() (*container.Config, *container.HostConfig) {
	cfg := &container.Config{}
	hostCfg := baseHostConfig(nil, nil, container.Resources{}, false)
	hardenSandbox(cfg, hostCfg)
	return cfg, hostCfg
}

func assertHardeningIntact(t *testing.T, cfg *container.Config, hostCfg *container.HostConfig) {
	t.Helper()
	if len(hostCfg.CapDrop) != 1 || hostCfg.CapDrop[0] != "ALL" {
		t.Errorf("expected CapDrop [ALL] to survive, got %v", hostCfg.CapDrop)
	}
	if len(hostCfg.CapAdd) != 0 {
		t.Errorf("expected no capability added back, got %v", hostCfg.CapAdd)
	}
	if !reflect.DeepEqual(hostCfg.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Errorf("expected SecurityOpt untouched, got %v", hostCfg.SecurityOpt)
	}
	if !hostCfg.ReadonlyRootfs {
		t.Error("expected the read-only rootfs to survive")
	}
	if hostCfg.Privileged {
		t.Error("a GPU must never require a privileged container")
	}
	if cfg.User == "" || strings.HasPrefix(cfg.User, "0:") {
		t.Errorf("expected the non-root sandbox user to survive, got %q", cfg.User)
	}
	if hostCfg.Resources.PidsLimit == nil || *hostCfg.Resources.PidsLimit <= 0 {
		t.Error("expected the PIDs limit to survive")
	}
	if hostCfg.NetworkMode != container.NetworkMode("none") {
		t.Errorf("expected the network policy to survive, got %q", hostCfg.NetworkMode)
	}
}

func TestApplyGPUPolicyNoneChangesNothing(t *testing.T) {
	cfg, hostCfg := sandboxedHostConfig()
	_, reference := sandboxedHostConfig()

	if err := applyGPUPolicy(hostCfg, gpu.Policy{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hostCfg.DeviceRequests) != 0 {
		t.Errorf("expected no device request with the default policy, got %#v", hostCfg.DeviceRequests)
	}
	if len(hostCfg.Devices) != 0 {
		t.Errorf("expected no device mapping with the default policy, got %#v", hostCfg.Devices)
	}
	if len(hostCfg.GroupAdd) != 0 {
		t.Errorf("expected no supplementary group with the default policy, got %v", hostCfg.GroupAdd)
	}
	if hostCfg.Runtime != "" {
		t.Errorf("expected the default container runtime, got %q", hostCfg.Runtime)
	}
	if !reflect.DeepEqual(hostCfg, reference) {
		t.Errorf("the default policy must leave the HostConfig byte-identical:\n got %#v\nwant %#v", hostCfg, reference)
	}
	assertHardeningIntact(t, cfg, hostCfg)
}

func TestApplyGPUPolicyNVIDIAAllRequestsEveryDevice(t *testing.T) {
	cfg, hostCfg := sandboxedHostConfig()

	policy := gpu.Policy{Mode: gpu.ModeAll, Vendor: gpu.VendorNVIDIA}
	if err := applyGPUPolicy(hostCfg, policy); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hostCfg.DeviceRequests) != 1 {
		t.Fatalf("expected exactly one device request, got %#v", hostCfg.DeviceRequests)
	}
	request := hostCfg.DeviceRequests[0]
	if request.Driver != "nvidia" {
		t.Errorf("expected the nvidia driver, got %q", request.Driver)
	}
	if request.Count != -1 {
		t.Errorf("expected Count -1 (all devices), got %d", request.Count)
	}
	if len(request.DeviceIDs) != 0 {
		t.Errorf("Count and DeviceIDs are mutually exclusive; got DeviceIDs %v alongside Count -1", request.DeviceIDs)
	}
	if !reflect.DeepEqual(request.Capabilities, [][]string{{"gpu"}}) {
		t.Errorf("expected the gpu capability, got %v", request.Capabilities)
	}
	if len(hostCfg.Devices) != 0 {
		t.Errorf("NVIDIA passthrough needs no explicit device mapping, got %#v", hostCfg.Devices)
	}
	assertHardeningIntact(t, cfg, hostCfg)
}

func TestApplyGPUPolicyNVIDIASelectionRequestsOnlyThoseDevices(t *testing.T) {
	cfg, hostCfg := sandboxedHostConfig()

	policy := gpu.Policy{Mode: gpu.ModeSelection, Vendor: gpu.VendorNVIDIA, Indexes: []int{0, 2}}
	if err := applyGPUPolicy(hostCfg, policy); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hostCfg.DeviceRequests) != 1 {
		t.Fatalf("expected exactly one device request, got %#v", hostCfg.DeviceRequests)
	}
	request := hostCfg.DeviceRequests[0]
	if !reflect.DeepEqual(request.DeviceIDs, []string{"0", "2"}) {
		t.Errorf("expected DeviceIDs [0 2], got %v", request.DeviceIDs)
	}
	if request.Count != 0 {
		t.Errorf("expected Count 0 when DeviceIDs are listed, got %d", request.Count)
	}
	assertHardeningIntact(t, cfg, hostCfg)
}

func TestApplyGPUPolicyAMDMapsDevicesAndJoinsOwningGroup(t *testing.T) {
	cfg, hostCfg := sandboxedHostConfig()

	policy := gpu.Policy{
		Mode:    gpu.ModeSelection,
		Vendor:  gpu.VendorAMD,
		Indexes: []int{0},
		Devices: []gpu.DeviceNode{
			{Path: "/dev/kfd", GroupID: 44},
			{Path: "/dev/dri/renderD128", GroupID: 44},
		},
	}
	if err := applyGPUPolicy(hostCfg, policy); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(hostCfg.DeviceRequests) != 0 {
		t.Errorf("ROCm has no device driver plugin; expected no device request, got %#v", hostCfg.DeviceRequests)
	}
	want := []container.DeviceMapping{
		{PathOnHost: "/dev/kfd", PathInContainer: "/dev/kfd", CgroupPermissions: "rw"},
		{PathOnHost: "/dev/dri/renderD128", PathInContainer: "/dev/dri/renderD128", CgroupPermissions: "rw"},
	}
	if !reflect.DeepEqual(hostCfg.Devices, want) {
		t.Errorf("expected devices %#v, got %#v", want, hostCfg.Devices)
	}
	for _, device := range hostCfg.Devices {
		if strings.Contains(device.CgroupPermissions, "m") {
			t.Errorf("device %s must not be granted mknod, got %q", device.PathOnHost, device.CgroupPermissions)
		}
	}
	if !reflect.DeepEqual(hostCfg.GroupAdd, []string{"44"}) {
		t.Errorf("expected the container to join the owning group once, got %v", hostCfg.GroupAdd)
	}
	assertHardeningIntact(t, cfg, hostCfg)
}

func TestApplyGPUPolicyRejectsUnknownVendor(t *testing.T) {
	_, hostCfg := sandboxedHostConfig()
	err := applyGPUPolicy(hostCfg, gpu.Policy{Mode: gpu.ModeAll, Vendor: "apple"})
	if err == nil {
		t.Fatal("expected an unsupported vendor to be refused rather than silently ignored")
	}
	if !strings.Contains(err.Error(), "apple") {
		t.Fatalf("expected the error to name the vendor, got: %v", err)
	}
}

func TestApplyGPUPolicyIsAppliedByTrustedAndUntrustedPhasesAlike(t *testing.T) {
	// Trusted (challenge init/validate) containers skip hardenSandbox but
	// must still get the GPUs the operator allowed, or a challenge could not
	// prepare GPU data.
	hostCfg := baseHostConfig(nil, nil, container.Resources{}, false)
	if err := applyGPUPolicy(hostCfg, gpu.Policy{Mode: gpu.ModeAll, Vendor: gpu.VendorNVIDIA}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(hostCfg.DeviceRequests) != 1 {
		t.Fatalf("expected the GPU request on a trusted phase too, got %#v", hostCfg.DeviceRequests)
	}
}

func TestApplyGPUPolicyRefusesEmptySelections(t *testing.T) {
	// Defensive: an "expose something" policy that names nothing would ask
	// Docker for no device in particular and quietly start a GPU-less
	// container. Resolve never builds one, so this guards the invariant.
	cases := map[string]gpu.Policy{
		"nvidia selection without indexes": {Mode: gpu.ModeSelection, Vendor: gpu.VendorNVIDIA},
		"amd policy without device nodes":  {Mode: gpu.ModeAll, Vendor: gpu.VendorAMD},
	}
	for name, policy := range cases {
		_, hostCfg := sandboxedHostConfig()
		if err := applyGPUPolicy(hostCfg, policy); err == nil {
			t.Errorf("%s: expected an error rather than a GPU-less container", name)
		}
	}
}
