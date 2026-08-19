package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// Task #70: the Docker client is lazy, so a runner started before its daemon
// used to fail on the first job instead of at startup. These tests exercise
// the readiness wait without a daemon: a fake pinger for the decision logic,
// and a real client aimed at a socket that cannot exist for the end-to-end
// error message.

type fakePinger struct {
	calls        int
	succeedAfter int // number of failures before the first success; -1 never succeeds
}

func (f *fakePinger) Ping(ctx context.Context) (types.Ping, error) {
	f.calls++
	if f.succeedAfter >= 0 && f.calls > f.succeedAfter {
		return types.Ping{APIVersion: "1.47"}, nil
	}
	return types.Ping{}, errors.New("cannot connect to the Docker daemon")
}

func TestWaitForDaemonReturnsImmediatelyWhenDaemonAnswers(t *testing.T) {
	pinger := &fakePinger{succeedAfter: 0}

	started := time.Now()
	if err := waitForDaemon(context.Background(), pinger, "unix:///var/run/docker.sock", 30*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a ready daemon must not delay startup, took %s", elapsed)
	}
	if pinger.calls != 1 {
		t.Fatalf("expected a single ping, got %d", pinger.calls)
	}
}

func TestWaitForDaemonRetriesUntilTheDaemonComesUp(t *testing.T) {
	// The dind case: dockerd is still starting when the runner boots.
	pinger := &fakePinger{succeedAfter: 2}

	if err := waitForDaemon(context.Background(), pinger, "unix:///var/run/docker.sock", 30*time.Second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pinger.calls != 3 {
		t.Fatalf("expected 3 pings (2 failures then success), got %d", pinger.calls)
	}
}

func TestWaitForDaemonFailsExplicitlyOnTimeout(t *testing.T) {
	pinger := &fakePinger{succeedAfter: -1}

	err := waitForDaemon(context.Background(), pinger, "unix:///var/run/docker.sock", 750*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the daemon never answers")
	}
	for _, want := range []string{"unix:///var/run/docker.sock", "did not answer", daemonWaitTimeoutEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error message must mention %q, got: %v", want, err)
		}
	}
	if pinger.calls < 2 {
		t.Fatalf("expected the wait to retry before giving up, got %d ping(s)", pinger.calls)
	}
}

func TestWaitForDaemonHonoursCancellation(t *testing.T) {
	pinger := &fakePinger{succeedAfter: -1}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := waitForDaemon(ctx, pinger, "unix:///var/run/docker.sock", time.Minute); err == nil {
		t.Fatal("expected an error once the context is cancelled")
	}
}

func TestWaitForDaemonDisabledByZeroTimeout(t *testing.T) {
	pinger := &fakePinger{succeedAfter: -1}

	if err := waitForDaemon(context.Background(), pinger, "unix:///var/run/docker.sock", 0); err != nil {
		t.Fatalf("a zero timeout must skip the check, got: %v", err)
	}
	if pinger.calls != 0 {
		t.Fatalf("a zero timeout must not ping at all, got %d", pinger.calls)
	}
}

// End-to-end against a real client and a socket that cannot exist: this is what
// an operator sees when the daemon is missing or DOCKER_HOST is wrong. It runs
// anywhere, daemon or not.
func TestWaitForDaemonWithoutAnyDaemonReportsTheHost(t *testing.T) {
	// A short, deterministic path: unix socket paths are capped near 104 bytes
	// on macOS/BSD, which t.TempDir() alone can exceed.
	socket := "unix:///nonexistent/dragrace-absent-docker.sock"
	cli, err := client.NewClientWithOpts(client.WithHost(socket))
	if err != nil {
		t.Fatalf("failed to build a client for %s: %v", socket, err)
	}
	defer cli.Close()

	err = waitForDaemon(context.Background(), cli, cli.DaemonHost(), 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected an error against a socket that does not exist")
	}
	if !strings.Contains(err.Error(), "dragrace-absent-docker.sock") {
		t.Fatalf("error must name the socket it tried, got: %v", err)
	}
}

func TestDaemonWaitTimeoutFallsBackOnMalformedValue(t *testing.T) {
	t.Setenv(daemonWaitTimeoutEnv, "not-a-duration")
	if got := daemonWaitTimeout(); got != defaultDaemonWaitTimeout {
		t.Fatalf("expected the default %s on a malformed value, got %s", defaultDaemonWaitTimeout, got)
	}

	t.Setenv(daemonWaitTimeoutEnv, "90s")
	if got := daemonWaitTimeout(); got != 90*time.Second {
		t.Fatalf("expected 90s, got %s", got)
	}

	t.Setenv(daemonWaitTimeoutEnv, "0")
	if got := daemonWaitTimeout(); got != 0 {
		t.Fatalf("expected the opt-out to survive, got %s", got)
	}
}
