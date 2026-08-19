package docker

import (
	"context"
	"fmt"

	"dragrace/internal/executor"

	"github.com/docker/docker/api/types/container"
)

const (
	// sandboxUID/sandboxGID is the fixed non-root identity used to run
	// untrusted (solution-controlled) phases. It has no meaning outside the
	// container and owns nothing on the host.
	sandboxUID = 65532
	sandboxGID = 65532

	// sandboxPidsLimit bounds the number of processes a sandboxed container
	// may fork, mitigating fork-bomb style resource exhaustion.
	sandboxPidsLimit = 512

	// sandboxTmpfsBytes is the scratch space granted at /tmp when the rootfs
	// is mounted read-only. It is memory-backed, so it is additionally
	// bounded by the container's own memory limit.
	sandboxTmpfsBytes = 256 * 1024 * 1024

	// sandboxLabel marks every container this executor creates, so leaked
	// containers are trivially discoverable (`docker ps -a --filter
	// label=com.dragrace.sandbox=true`) regardless of which phase created them.
	sandboxLabel = "com.dragrace.sandbox"
)

var sandboxLabels = map[string]string{sandboxLabel: "true"}

// buildBinds constructs the minimal set of bind mounts for a phase:
// the repository (always read-only) and, if requested, the challenge data
// volume (read-only unless the phase explicitly needs to write to it).
func buildBinds(opts *executor.RunOptions) []string {
	binds := []string{
		fmt.Sprintf("%s:/workspace:ro", opts.RepoDir),
	}
	if opts.DataDir != "" {
		mode := "rw"
		if opts.ReadOnlyData {
			mode = "ro"
		}
		binds = append(binds, fmt.Sprintf("%s:/data:%s", opts.DataDir, mode))
	}
	return binds
}

// baseHostConfig builds the sandbox HostConfig shared by every phase:
// network access follows runner/challenge policy rather than being
// hardcoded, and every container drops all Linux capabilities, blocks
// privilege escalation, and caps the number of processes it may run.
func baseHostConfig(binds []string, resources container.Resources, networkEnabled bool) *container.HostConfig {
	pidsLimit := int64(sandboxPidsLimit)
	resources.PidsLimit = &pidsLimit
	netMode := container.NetworkMode("none")
	if networkEnabled {
		netMode = container.NetworkMode("bridge")
	}
	return &container.HostConfig{
		Binds:       binds,
		Resources:   resources,
		NetworkMode: netMode,
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges:true"},
	}
}

// hardenSandbox tightens a container for untrusted (solution-controlled)
// code: it runs as a fixed non-root user with a read-only rootfs, using a
// bounded tmpfs at /tmp for scratch space. Organizer-controlled phases
// (challenge init/validate) do not get this treatment because they may
// need root to install challenge dependencies; they still get the baseline
// hardening from baseHostConfig.
func hardenSandbox(cfg *container.Config, hostCfg *container.HostConfig) {
	cfg.User = fmt.Sprintf("%d:%d", sandboxUID, sandboxGID)
	hostCfg.ReadonlyRootfs = true
	hostCfg.Tmpfs = map[string]string{
		"/tmp": fmt.Sprintf("rw,nosuid,nodev,size=%d", sandboxTmpfsBytes),
	}
}

// chownDataDir grants the sandbox's non-root user write access to a data
// volume. Docker always creates named volumes root-owned, so a phase that
// both runs non-root and needs to write to /data (e.g. a local `runner
// test` run) needs this preparation step first. It reuses the phase's own
// image (already pulled) as a short-lived, capability-scoped helper: no
// network, no capabilities beyond CHOWN, removed immediately after.
func (e *Executor) chownDataDir(ctx context.Context, image, volumeName string) error {
	pidsLimit := int64(sandboxPidsLimit)
	resp, err := e.client.ContainerCreate(ctx, &container.Config{
		Image:  image,
		Cmd:    []string{"/bin/sh", "-c", fmt.Sprintf("chown -R %d:%d /data", sandboxUID, sandboxGID)},
		Tty:    false,
		Labels: sandboxLabels,
	}, &container.HostConfig{
		Binds:       []string{fmt.Sprintf("%s:/data:rw", volumeName)},
		NetworkMode: "none",
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"CHOWN"},
		SecurityOpt: []string{"no-new-privileges:true"},
		Resources:   container.Resources{PidsLimit: &pidsLimit},
	}, nil, nil, "")
	if err != nil {
		return fmt.Errorf("failed to create data dir permission helper: %w", err)
	}
	defer e.client.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})

	if err := e.client.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return fmt.Errorf("failed to start data dir permission helper: %w", err)
	}

	exitCode, _, err := e.waitContainer(ctx, resp.ID)
	if err != nil {
		return fmt.Errorf("data dir permission helper wait error: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("data dir permission helper exited with code %d", exitCode)
	}
	return nil
}
