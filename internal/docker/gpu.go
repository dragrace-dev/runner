package docker

import (
	"fmt"
	"strconv"

	"dragrace/internal/gpu"

	"github.com/docker/docker/api/types/container"
)

// applyGPUPolicy adds the operator's GPU exposure (#65) to a job container's
// HostConfig. It is called after baseHostConfig/hardenSandbox and must never
// undo any of it: no capability is added back, no security option is
// removed, the rootfs stays read-only and the process keeps running as the
// fixed non-root sandbox user.
//
// With the default policy (none) this is a no-op, so a CPU-only host builds
// exactly the HostConfig it built before this feature existed.
func applyGPUPolicy(hostCfg *container.HostConfig, policy gpu.Policy) error {
	if hostCfg == nil {
		return fmt.Errorf("cannot apply the GPU policy to a nil host config")
	}
	if !policy.Enabled() {
		return nil
	}

	switch policy.Vendor {
	case gpu.VendorNVIDIA:
		// Same mechanism as `docker run --gpus`: the daemon hands the
		// request to the nvidia device driver plugin, which injects the
		// driver libraries and device nodes through an OCI prestart hook.
		// The hook runs before runc remounts the rootfs read-only, so
		// strict sandboxing and GPU access coexist.
		request := container.DeviceRequest{
			Driver:       "nvidia",
			Capabilities: [][]string{{"gpu"}},
		}
		if policy.Mode == gpu.ModeAll {
			request.Count = -1
		} else {
			if len(policy.Indexes) == 0 {
				// Would ask Docker for "no device in particular" and start a
				// GPU-less container. Refuse instead of guessing.
				return fmt.Errorf("GPU policy selects no device index; nothing to expose")
			}
			ids := make([]string, 0, len(policy.Indexes))
			for _, index := range policy.Indexes {
				ids = append(ids, strconv.Itoa(index))
			}
			request.DeviceIDs = ids
		}
		hostCfg.DeviceRequests = append(hostCfg.DeviceRequests, request)

	case gpu.VendorAMD:
		if len(policy.Devices) == 0 {
			return fmt.Errorf("AMD GPU policy maps no device node; nothing to expose")
		}
		// ROCm has no device driver plugin: the compute node and the render
		// nodes are mapped explicitly, and the sandbox user joins the group
		// that owns them (it is non-root, so it cannot open them otherwise).
		//
		// Cgroup permissions are "rw", not Docker's usual "rwm": the
		// container may use the devices it was given but may not create new
		// device nodes. CAP_MKNOD is dropped anyway, so "m" would only widen
		// the policy on paper.
		for _, device := range policy.Devices {
			hostCfg.Devices = append(hostCfg.Devices, container.DeviceMapping{
				PathOnHost:        device.Path,
				PathInContainer:   device.Path,
				CgroupPermissions: "rw",
			})
		}
		hostCfg.GroupAdd = append(hostCfg.GroupAdd, policy.GroupIDs()...)

	default:
		return fmt.Errorf("unsupported GPU vendor %q in the resolved policy", policy.Vendor)
	}

	return nil
}
