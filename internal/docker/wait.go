package docker

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/docker/docker/api/types"
)

// The Docker client is lazy: client.NewClientWithOpts opens no connection, so
// a runner whose daemon is not up yet starts perfectly happily and only finds
// out when it tries to run its first job — which then fails, and fails as a
// job failure attributed to the submitted solution rather than as a runner
// problem.
//
// That is guaranteed to happen in the docker-in-docker variant (#70), where
// dockerd is starting inside the same container at the same moment, and it
// happens in practice in the docker-out-of-docker one too whenever the runner
// container starts alongside the daemon rather than after it. So the executor
// blocks on a bounded readiness check at construction and fails with an
// explicit message instead.
const (
	// daemonWaitTimeoutEnv overrides how long to wait. "0" disables the wait
	// entirely and restores the previous lazy behaviour.
	daemonWaitTimeoutEnv = "RUNNER_DOCKER_WAIT_TIMEOUT"

	defaultDaemonWaitTimeout = 60 * time.Second
	daemonPingTimeout        = 5 * time.Second
	daemonPingInterval       = 500 * time.Millisecond
)

// daemonPinger is the one client method the readiness check needs, so the
// check can be unit-tested without a daemon.
type daemonPinger interface {
	Ping(ctx context.Context) (types.Ping, error)
}

// daemonWaitTimeout reads RUNNER_DOCKER_WAIT_TIMEOUT as a Go duration
// ("90s", "2m"). A malformed value is reported and ignored rather than
// silently turning into zero, which would disable the check.
func daemonWaitTimeout() time.Duration {
	raw := os.Getenv(daemonWaitTimeoutEnv)
	if raw == "" {
		return defaultDaemonWaitTimeout
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout < 0 {
		log.Printf("⚠️  Ignoring invalid %s=%q (want a duration such as 90s); using %s",
			daemonWaitTimeoutEnv, raw, defaultDaemonWaitTimeout)
		return defaultDaemonWaitTimeout
	}
	return timeout
}

// waitForDaemon polls the daemon until it answers or the timeout expires.
// A zero timeout skips the check. The returned error names the host and the
// knob to turn, because the two realistic causes — daemon not started, wrong
// DOCKER_HOST — are indistinguishable from the raw dial error alone.
func waitForDaemon(ctx context.Context, pinger daemonPinger, host string, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}

	started := time.Now()
	deadline := started.Add(timeout)

	var lastErr error
	for attempt := 0; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, daemonPingTimeout)
		_, err := pinger.Ping(pingCtx)
		cancel()
		if err == nil {
			if attempt > 0 {
				log.Printf("✅ Docker daemon ready after %s", time.Since(started).Round(time.Millisecond))
			}
			return nil
		}
		lastErr = err

		if attempt == 0 {
			log.Printf("⏳ Waiting up to %s for the Docker daemon at %s...", timeout, host)
		}

		if err := ctx.Err(); err != nil {
			return fmt.Errorf("gave up waiting for the Docker daemon at %s: %w (last error: %v)", host, err, lastErr)
		}
		if !time.Now().Add(daemonPingInterval).Before(deadline) {
			break
		}

		select {
		case <-time.After(daemonPingInterval):
		case <-ctx.Done():
			return fmt.Errorf("gave up waiting for the Docker daemon at %s: %w (last error: %v)", host, ctx.Err(), lastErr)
		}
	}

	return fmt.Errorf(
		"Docker daemon at %s did not answer within %s: %w "+
			"(is the daemon running, and is DOCKER_HOST correct? set %s to change the wait)",
		host, timeout, lastErr, daemonWaitTimeoutEnv)
}
