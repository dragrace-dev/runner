package docker

import (
	"strings"
	"testing"

	"dragrace/internal/executor"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// These are the threat-model checks for task #20: every sandbox container
// must drop all capabilities, block privilege escalation, cap its process
// count, follow the network policy it was given (never hardcoded), and
// mount only the two paths a phase actually needs.

func TestBaseHostConfigDropsAllCapabilitiesAndBlocksPrivilegeEscalation(t *testing.T) {
	hostCfg := baseHostConfig(nil, nil, container.Resources{}, false)

	if len(hostCfg.CapDrop) != 1 || hostCfg.CapDrop[0] != "ALL" {
		t.Fatalf("expected CapDrop: [ALL], got %v", hostCfg.CapDrop)
	}
	if len(hostCfg.CapAdd) != 0 {
		t.Fatalf("expected no capabilities added back, got %v", hostCfg.CapAdd)
	}
	found := false
	for _, opt := range hostCfg.SecurityOpt {
		if opt == "no-new-privileges:true" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected no-new-privileges:true in SecurityOpt, got %v", hostCfg.SecurityOpt)
	}
}

func TestBaseHostConfigCapsProcessCount(t *testing.T) {
	hostCfg := baseHostConfig(nil, nil, container.Resources{}, false)

	if hostCfg.Resources.PidsLimit == nil {
		t.Fatal("expected a PIDs limit to be set")
	}
	if *hostCfg.Resources.PidsLimit <= 0 {
		t.Fatalf("expected a positive PIDs limit, got %d", *hostCfg.Resources.PidsLimit)
	}
}

func TestBaseHostConfigNetworkModeFollowsPolicy(t *testing.T) {
	cases := []struct {
		networkEnabled bool
		want           container.NetworkMode
	}{
		{networkEnabled: false, want: container.NetworkMode("none")},
		{networkEnabled: true, want: container.NetworkMode("bridge")},
	}
	for _, tc := range cases {
		hostCfg := baseHostConfig(nil, nil, container.Resources{}, tc.networkEnabled)
		if hostCfg.NetworkMode != tc.want {
			t.Errorf("networkEnabled=%v: expected NetworkMode %q, got %q", tc.networkEnabled, tc.want, hostCfg.NetworkMode)
		}
	}
}

func TestBaseHostConfigPreservesCallerResourceLimits(t *testing.T) {
	resources := container.Resources{Memory: 123, NanoCPUs: 456}
	hostCfg := baseHostConfig(nil, nil, resources, false)

	if hostCfg.Resources.Memory != 123 || hostCfg.Resources.NanoCPUs != 456 {
		t.Fatalf("expected caller resource limits to survive, got %#v", hostCfg.Resources)
	}
}

func TestHardenSandboxSetsNonRootUserAndReadOnlyRootfs(t *testing.T) {
	cfg := &container.Config{}
	hostCfg := &container.HostConfig{}
	hardenSandbox(cfg, hostCfg)

	if cfg.User == "" || cfg.User == "0" || cfg.User == "0:0" || strings.HasPrefix(cfg.User, "root") {
		t.Fatalf("expected a non-root user, got %q", cfg.User)
	}
	if !hostCfg.ReadonlyRootfs {
		t.Fatal("expected ReadonlyRootfs to be true")
	}
	tmpfsOpts, ok := hostCfg.Tmpfs["/tmp"]
	if !ok {
		t.Fatal("expected a tmpfs mount at /tmp so scripts have scratch space under a read-only rootfs")
	}
	if !strings.Contains(tmpfsOpts, "size=") {
		t.Fatalf("expected the /tmp tmpfs to have a bounded size, got %q", tmpfsOpts)
	}
}

func TestBuildMountsWorkspaceIsAlwaysReadOnly(t *testing.T) {
	mounts := buildMounts(&executor.RunOptions{RepoDir: "/host/repo"})
	if len(mounts) != 1 {
		t.Fatalf("expected a single workspace mount, got %v", mounts)
	}
	got := mounts[0]
	if got.Type != mount.TypeBind || got.Source != "/host/repo" || got.Target != "/workspace" {
		t.Fatalf("expected /host/repo bind-mounted at /workspace, got %+v", got)
	}
	if !got.ReadOnly {
		t.Fatalf("expected the workspace mount to be read-only, got %+v", got)
	}
}

// The repository mount must never ask the daemon to conjure its source
// directory. When the runner is containerized and drives the host daemon
// over a mounted socket (docker-out-of-docker), the repo path exists only
// inside the runner container; letting the daemon create it there yields an
// empty /workspace and a misleading exit 126 from the "script is not
// executable" pre-check. Refusing to create it turns that into an explicit
// "bind source path does not exist" at container creation. See #68.
func TestBuildMountsNeverCreatesMissingSourceOnTheDaemonHost(t *testing.T) {
	mounts := buildMounts(&executor.RunOptions{RepoDir: "/host/repo"})
	opts := mounts[0].BindOptions
	if opts != nil && opts.CreateMountpoint {
		t.Fatal("workspace mount must not set CreateMountpoint: a source the daemon cannot see has to be an error, not an empty directory")
	}
}

func TestBuildBindsDataDirModeFollowsReadOnlyFlag(t *testing.T) {
	rw := buildBinds(&executor.RunOptions{RepoDir: "/host/repo", DataDir: "vol", ReadOnlyData: false})
	if len(rw) != 1 || rw[0] != "vol:/data:rw" {
		t.Fatalf("expected a writable data bind, got %v", rw)
	}

	ro := buildBinds(&executor.RunOptions{RepoDir: "/host/repo", DataDir: "vol", ReadOnlyData: true})
	if len(ro) != 1 || ro[0] != "vol:/data:ro" {
		t.Fatalf("expected a read-only data bind, got %v", ro)
	}
}

func TestBuildBindsOmitsDataMountWhenNoDataDir(t *testing.T) {
	binds := buildBinds(&executor.RunOptions{RepoDir: "/host/repo"})
	for _, b := range binds {
		if strings.Contains(b, "/data") {
			t.Fatalf("expected no /data mount when DataDir is unset, got %v", binds)
		}
	}
}

// The repository travels as a path and the data dir as a named volume, so
// only the repository is exposed to daemon-side path resolution.
func TestBuildBindsCarriesNoWorkspacePath(t *testing.T) {
	binds := buildBinds(&executor.RunOptions{RepoDir: "/host/repo", DataDir: "vol"})
	for _, b := range binds {
		if strings.Contains(b, "/workspace") || strings.Contains(b, "/host/repo") {
			t.Fatalf("the workspace must travel as a Mount, not a Bind, got %v", binds)
		}
	}
}
