package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dragrace/internal/executor"
	"dragrace/internal/gpu"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// These tests exercise the sandbox against a real Docker daemon. They cover
// the two acceptance criteria that can only be verified end-to-end: network
// egress is actually forbidden when a phase's policy says "no network", and
// containers never survive past their run (success, failure, or otherwise).
//
// They're skipped automatically when no Docker daemon is reachable.

const sandboxTestImage = "alpine:3.20"

func requireDocker(t *testing.T) *Executor {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		t.Skipf("docker unavailable: %v", err)
	}
	if _, err := cli.Ping(context.Background()); err != nil {
		t.Skipf("docker daemon unreachable: %v", err)
	}
	cli.Close()

	exec, err := NewExecutor("", gpu.Policy{})
	if err != nil {
		t.Skipf("failed to build docker executor: %v", err)
	}
	t.Cleanup(func() { exec.Close() })
	return exec
}

// writeRepo creates a minimal repo directory containing one executable script.
func writeRepo(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}
	return dir
}

// sandboxContainerCount returns how many containers (running or not) still
// carry the sandbox label — i.e. how many the executor has leaked.
func sandboxContainerCount(t *testing.T, cli *client.Client) int {
	t.Helper()
	args := filters.NewArgs()
	args.Add("label", sandboxLabel+"=true")
	list, err := cli.ContainerList(context.Background(), container.ListOptions{All: true, Filters: args})
	if err != nil {
		t.Fatalf("failed to list containers: %v", err)
	}
	return len(list)
}

func TestRunMeasuredForbidsNetworkEgressWhenDisabled(t *testing.T) {
	exec := requireDocker(t)
	dir := writeRepo(t, "#!/bin/sh\nset -eu\nroutes=$(ip route 2>/dev/null | grep -c default || true)\nif [ \"$routes\" != \"0\" ]; then\n  echo \"unexpected default route: network is reachable\" >&2\n  exit 1\nfi\nexit 0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := exec.RunMeasured(ctx, &executor.RunOptions{
		Image:          sandboxTestImage,
		ScriptPath:     "run.sh",
		RepoDir:        dir,
		NetworkEnabled: false,
	})
	if err != nil {
		t.Fatalf("expected the air-gapped run to succeed (i.e. confirm no route), got error: %v", err)
	}
}

func TestRunMeasuredGrantsNetworkEgressWhenEnabled(t *testing.T) {
	exec := requireDocker(t)
	dir := writeRepo(t, "#!/bin/sh\nset -eu\nroutes=$(ip route 2>/dev/null | grep -c default || true)\nif [ \"$routes\" = \"0\" ]; then\n  echo \"expected a default route when network is enabled\" >&2\n  exit 1\nfi\nexit 0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := exec.RunMeasured(ctx, &executor.RunOptions{
		Image:          sandboxTestImage,
		ScriptPath:     "run.sh",
		RepoDir:        dir,
		NetworkEnabled: true,
	})
	if err != nil {
		t.Fatalf("expected the networked run to succeed (i.e. confirm a route exists), got error: %v", err)
	}
}

func TestRunMeasuredLeavesNoContainerBehindOnSuccess(t *testing.T) {
	exec := requireDocker(t)
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		t.Fatalf("failed to build inspection client: %v", err)
	}
	defer cli.Close()

	dir := writeRepo(t, "#!/bin/sh\nexit 0\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	before := sandboxContainerCount(t, cli)
	if _, _, err := exec.RunMeasured(ctx, &executor.RunOptions{Image: sandboxTestImage, ScriptPath: "run.sh", RepoDir: dir}); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	after := sandboxContainerCount(t, cli)
	if after != before {
		t.Fatalf("expected no leaked sandbox containers after a successful run, before=%d after=%d", before, after)
	}
}

func TestRunMeasuredLeavesNoContainerBehindOnFailure(t *testing.T) {
	exec := requireDocker(t)
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		t.Fatalf("failed to build inspection client: %v", err)
	}
	defer cli.Close()

	dir := writeRepo(t, "#!/bin/sh\nexit 7\n")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	before := sandboxContainerCount(t, cli)
	_, _, err = exec.RunMeasured(ctx, &executor.RunOptions{Image: sandboxTestImage, ScriptPath: "run.sh", RepoDir: dir})
	if err == nil {
		t.Fatal("expected the run to fail (script exits 7)")
	}
	after := sandboxContainerCount(t, cli)
	if after != before {
		t.Fatalf("expected no leaked sandbox containers after a failed run, before=%d after=%d", before, after)
	}
}

func TestRunMeasuredRunsAsNonRootUser(t *testing.T) {
	exec := requireDocker(t)
	dir := writeRepo(t, "#!/bin/sh\nset -eu\nuid=$(id -u)\nif [ \"$uid\" = \"0\" ]; then\n  echo \"expected non-root, got uid 0\" >&2\n  exit 1\nfi\nexit 0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := exec.RunMeasured(ctx, &executor.RunOptions{Image: sandboxTestImage, ScriptPath: "run.sh", RepoDir: dir}); err != nil {
		t.Fatalf("expected the run phase to execute as a non-root user, got error: %v", err)
	}
}

func TestRunMeasuredHasReadOnlyRootfs(t *testing.T) {
	exec := requireDocker(t)
	dir := writeRepo(t, "#!/bin/sh\nif touch /root-write-test 2>/dev/null; then\n  echo 'unexpected write to read-only rootfs succeeded' >&2\n  exit 1\nfi\nexit 0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, _, err := exec.RunMeasured(ctx, &executor.RunOptions{Image: sandboxTestImage, ScriptPath: "run.sh", RepoDir: dir}); err != nil {
		t.Fatalf("expected the run phase rootfs to be read-only, got error: %v", err)
	}
}

func TestRunMeasuredWithWritableDataDirGrantsAccessToNonRootUser(t *testing.T) {
	// Local `runner test` runs (unlike production jobs) mount /data writable
	// for the run phase; Docker always creates named volumes root-owned, so
	// this only works if the chown-data-dir preparation step ran first.
	exec := requireDocker(t)
	volumeName := "dragrace-test-sandbox-chown"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := exec.EnsureDataDir(ctx, volumeName); err != nil {
		t.Fatalf("failed to create data dir: %v", err)
	}
	t.Cleanup(func() { exec.RemoveDataDir(context.Background(), volumeName) })

	dir := writeRepo(t, "#!/bin/sh\nset -eu\necho hello > /data/output.txt\n")

	if _, _, err := exec.RunMeasured(ctx, &executor.RunOptions{
		Image:        sandboxTestImage,
		ScriptPath:   "run.sh",
		RepoDir:      dir,
		DataDir:      volumeName,
		ReadOnlyData: false,
	}); err != nil {
		t.Fatalf("expected the non-root run phase to write to a prepared data dir, got error: %v", err)
	}
}

func TestRunScriptTrustedPhaseKeepsWritableRootfsAndRoot(t *testing.T) {
	exec := requireDocker(t)
	dir := writeRepo(t, "#!/bin/sh\nset -eu\nif ! touch /root-write-test 2>/dev/null; then\n  echo 'expected a trusted phase to keep a writable rootfs' >&2\n  exit 1\nfi\nexit 0\n")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logs, err := exec.RunScript(ctx, &executor.RunOptions{Image: sandboxTestImage, ScriptPath: "run.sh", RepoDir: dir, Trusted: true})
	if err != nil {
		t.Fatalf("expected the trusted phase to keep a writable rootfs, got error: %v (%s)", err, strings.TrimSpace(logs))
	}
}

// A repository path the daemon cannot resolve must fail loudly at container
// creation instead of producing an empty /workspace.
//
// This is the regression guard for #68. When the runner itself runs in a
// container and drives the host daemon through a mounted socket
// (docker-out-of-docker), the repo path it hands over exists only inside the
// runner container. Expressed as a legacy Bind, the daemon would helpfully
// create that directory on its own host, mount the empty result, and the
// phase would die on the "script is not executable" pre-check with exit 126
// — blaming the submitted solution for a deployment problem. Expressed as a
// bind Mount without CreateMountpoint, the daemon refuses outright and names
// the path it could not find.
//
// The unresolvable path is simulated here with a directory that exists on
// neither side, which is the same condition the daemon sees in the DooD case.
func TestRunMeasuredRejectsARepoPathTheDaemonCannotResolve(t *testing.T) {
	exec := requireDocker(t)

	missing := filepath.Join(t.TempDir(), "never-created", "solution")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _, err := exec.RunMeasured(ctx, &executor.RunOptions{
		Image:      sandboxTestImage,
		ScriptPath: "run.sh",
		RepoDir:    missing,
	})
	if err == nil {
		t.Fatal("expected an unresolvable repo path to fail container creation, got success — the daemon silently mounted an empty /workspace")
	}
	if strings.Contains(err.Error(), "exit") && strings.Contains(err.Error(), "126") {
		t.Fatalf("expected a mount error naming the missing path, got the misleading exit-126 symptom instead: %v", err)
	}
	if _, statErr := os.Stat(missing); statErr == nil {
		t.Fatalf("the daemon created %s instead of refusing the mount; missing bind sources must never be conjured", missing)
	}
}
