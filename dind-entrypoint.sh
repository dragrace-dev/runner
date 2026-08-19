#!/bin/sh
# DragRace runner — docker-in-docker entrypoint (#70).
#
# Starts dockerd in THIS container, waits for it to answer, then supervises the
# runner. Only used by the `dind` stage of Dockerfile; the DooD variant execs
# the runner directly and never sources this file.
#
# Why supervise instead of `exec runner`:
#   `exec` would make the runner PID 1, and `docker stop` only signals PID 1.
#   dockerd would then never receive SIGTERM and would be SIGKILLed with the
#   rest of the container, leaving /var/lib/docker — a *persistent volume* in
#   this variant — with unflushed containerd state and orphaned overlay
#   mountpoints. So this script stays PID 1, forwards signals to the runner,
#   waits for the runner's real exit status, exits with it, and only then
#   shuts dockerd down in an orderly way.
#
#   Child reaping is covered: this shell reaps the two processes it starts, and
#   dockerd itself runs under `docker-init` (tini), injected by the base image's
#   dockerd-entrypoint.sh, which reaps the containerd shims.
#
# Graceful shutdown needs room: the container's stop timeout must exceed the
# runner's own shutdown plus RUNNER_DIND_STOP_TIMEOUT. Use
# `stop_grace_period: 1m` in compose, or `docker stop -t 60`.
set -eu

log() {
	printf '%s dind-entrypoint: %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*" >&2
}

fatal() {
	log "FATAL: $*"
	exit 78 # EX_CONFIG
}

# Seconds to wait for the daemon to answer before giving up.
DAEMON_TIMEOUT="${RUNNER_DIND_DAEMON_TIMEOUT:-60}"
# Seconds to let dockerd stop cleanly before SIGKILL.
STOP_TIMEOUT="${RUNNER_DIND_STOP_TIMEOUT:-20}"
# Where dockerd's own output goes. Kept out of the container's stdout so the
# runner's job logs stay readable; dumped to stderr if startup fails.
DOCKERD_LOG="${RUNNER_DIND_DOCKERD_LOG:-/var/log/dockerd.log}"

# ── GPU: refuse rather than fail silently ────────────────────────────────────
#
# A `default-runtime: nvidia` on the host configures the HOST daemon. This
# variant's jobs run on a daemon started here, which has no such configuration,
# no nvidia-container-toolkit and no device nodes. GPU passthrough would need
# all three plus the devices handed to this outer container. None of it is done.
#
# On a CUDA host that is exactly the dangerous case: the DooD variant may hand
# jobs a GPU as a side effect of the host daemon's configuration, and switching
# to this image silently takes it away — a measurement platform quietly
# reporting CPU numbers for a GPU benchmark. So: refuse to start.
gpus_lower="$(printf '%s' "${RUNNER_GPUS:-none}" | tr '[:upper:]' '[:lower:]')"
case "$gpus_lower" in
"" | none) ;;
*)
	log "RUNNER_GPUS=${RUNNER_GPUS:-} was requested."
	fatal "this is the docker-in-docker (-dind) runner image and it has NO GPU support.
        The daemon it starts is not the host daemon: the host's nvidia runtime,
        the nvidia-container-toolkit and the device nodes are all absent from
        this container, so job containers would get no GPU whatever RUNNER_GPUS
        says. Refusing to start rather than silently measuring on CPU.
        Use the docker-out-of-docker image (ghcr.io/dragrace-dev/runner:X.Y.Z,
        no -dind suffix) on GPU hosts. See docs/RUNNER_THREAT_MODEL.md ▸
        'Variante docker-in-docker'."
	;;
esac
log "GPU passthrough is NOT available in this -dind image; jobs run CPU-only."

# ── Socket ───────────────────────────────────────────────────────────────────
#
# The daemon and the runner must agree on one socket. Refuse a remote
# DOCKER_HOST outright: it would mean starting a local dockerd nothing uses
# while the runner drives someone else's daemon, and the path coincidence this
# whole variant exists for would be gone.
case "${DOCKER_HOST:-}" in
"") DOCKER_HOST='unix:///var/run/docker.sock' ;;
unix://*) ;;
*) fatal "DOCKER_HOST must be a unix:// socket in the -dind image (got '${DOCKER_HOST}'). Use the DooD image to drive a remote or host daemon." ;;
esac
export DOCKER_HOST
SOCKET_PATH="${DOCKER_HOST#unix://}"

# ── dockerd ──────────────────────────────────────────────────────────────────
#
# Explicit `dockerd --host=…` arguments on purpose. Called with no arguments,
# the base image's dockerd-entrypoint.sh appends a TCP listener — 2376 with a
# generated CA, or 2375 with NO authentication when DOCKER_TLS_CERTDIR is
# empty. Either publishes root-on-this-container to the compose network.
# Passing arguments skips that block entirely while keeping the parts we want
# (stale pid-file cleanup, iptables backend detection, docker-init as parent).
mkdir -p "$(dirname "$DOCKERD_LOG")"
: >"$DOCKERD_LOG"

#
# RUNNER_DIND_DOCKERD_ARGS is deliberately left unquoted below so it splits into
# separate arguments: it exists for daemon flags an operator genuinely needs to
# reach, above all `--mtu=…` when the outer network is encapsulated and the
# inner bridge's default 1500 silently black-holes large packets.
# shellcheck disable=SC2086
log "starting dockerd on ${DOCKER_HOST} (output: ${DOCKERD_LOG})"
dockerd-entrypoint.sh dockerd --host="$DOCKER_HOST" ${RUNNER_DIND_DOCKERD_ARGS:-} >>"$DOCKERD_LOG" 2>&1 &
DOCKERD_PID=$!

dump_dockerd_log() {
	log "last 50 lines of ${DOCKERD_LOG}:"
	tail -n 50 "$DOCKERD_LOG" >&2 || true
}

waited=0
while :; do
	if docker version >/dev/null 2>&1; then
		log "dockerd ready after ${waited}s"
		break
	fi
	if ! kill -0 "$DOCKERD_PID" 2>/dev/null; then
		log "dockerd exited before becoming ready."
		log "The usual cause is a missing --privileged: this image needs it to"
		log "create namespaces, mount overlayfs and program iptables."
		dump_dockerd_log
		exit 1
	fi
	if [ "$waited" -ge "$DAEMON_TIMEOUT" ]; then
		log "dockerd did not answer on ${SOCKET_PATH} within ${DAEMON_TIMEOUT}s"
		log "(raise RUNNER_DIND_DAEMON_TIMEOUT for slow cold starts on a fresh /var/lib/docker)"
		dump_dockerd_log
		exit 1
	fi
	sleep 1
	waited=$((waited + 1))
done

# ── runner ───────────────────────────────────────────────────────────────────
RUNNER_PID=''

forward() {
	if [ -n "$RUNNER_PID" ]; then
		log "forwarding SIG$1 to the runner (pid ${RUNNER_PID})"
		kill -"$1" "$RUNNER_PID" 2>/dev/null || true
	fi
}

trap 'forward TERM' TERM
trap 'forward INT' INT
trap 'forward HUP' HUP

/usr/local/bin/runner "$@" &
RUNNER_PID=$!
log "runner started (pid ${RUNNER_PID})"

# `wait` returns 128+signum when a trap interrupts it while the child is still
# alive, so loop until the child is really gone to get its true status. `if
# wait` rather than a bare `wait` keeps `set -e` from aborting on a non-zero
# exit we want to propagate.
status=0
while kill -0 "$RUNNER_PID" 2>/dev/null; do
	if wait "$RUNNER_PID"; then
		status=0
	else
		status=$?
	fi
done
log "runner exited with status ${status}"

# ── orderly dockerd shutdown ─────────────────────────────────────────────────
if kill -0 "$DOCKERD_PID" 2>/dev/null; then
	log "stopping dockerd (pid ${DOCKERD_PID})"
	kill -TERM "$DOCKERD_PID" 2>/dev/null || true
	stopped=0
	while [ "$stopped" -lt "$STOP_TIMEOUT" ] && kill -0 "$DOCKERD_PID" 2>/dev/null; do
		sleep 1
		stopped=$((stopped + 1))
	done
	if kill -0 "$DOCKERD_PID" 2>/dev/null; then
		log "dockerd still running after ${STOP_TIMEOUT}s, sending SIGKILL"
		log "(/var/lib/docker may need a cleanup pass on the next start)"
		kill -KILL "$DOCKERD_PID" 2>/dev/null || true
	fi
	wait "$DOCKERD_PID" 2>/dev/null || true
fi

exit "$status"
