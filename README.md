# DragRace Runner

A runner picks up jobs from a DragRace backend, executes a challenge's
`init` / `build` / `run` / `validate` phases, and reports scores back. This
repository is everything you need to build one, run it against
[dragrace.dev](https://dragrace.dev) (or your own DragRace backend), and
operate it long-term.

It is a single Go binary (`cmd/runner`) with no required runtime
dependencies of its own. Whether it needs Docker on the host depends entirely
on which of the four configurations below you pick.

## Which configuration do you want?

The runner can execute job code two ways (the **executor**), and the runner
process itself can run two ways (**bare** on the host, or **containerized**).
Only three of the four combinations are meaningful in practice — a
containerized runner always uses the Docker executor, since the whole point
of containerizing it is to hand jobs a Docker sandbox.

| Configuration | Runner process runs... | Jobs execute in... | Needs Docker on the host? | Extra privilege | GPU jobs |
|---|---|---|---|---|---|
| **1. Native — Docker executor** *(recommended default)* | directly on the host (binary, systemd, launchd) | a container, via the host's Docker daemon | Yes | none beyond Docker group membership | Yes |
| **2. Native — process executor** | directly on the host | directly on the host OS, no container | No | none, but see the risk note below | Yes (jobs already see every host GPU) |
| **3. Container — Docker-out-of-Docker (DooD)** | inside a container | a sibling container, via the **host's** Docker daemon (mounted socket) | Yes (on the host) | mounts `/var/run/docker.sock` — root-equivalent on the host | Yes |
| **4. Container — Docker-in-Docker (DinD)** | inside a `--privileged` container | a child container of a `dockerd` running **inside that same container** | No | `--privileged` — a different, broader path to the host | No |

If you're not sure: config 1 (native, Docker executor) is what
`scripts/install-runner.sh` sets up, is the best-isolated option that still
needs no extra host privilege, and is what most people running a runner of
their own actually want. Reach for DooD when you want the runner itself
containerized (fleet management, Kubernetes, whatever already runs your other
containers) but still trust it with the host socket. Reach for DinD only when
the runner has to share a machine with other things you don't want its job
sandboxes to see, and you've read [Security model](#security-model) below.
Reach for the native process executor only when Docker genuinely isn't
available and your organization accepts running arbitrary submitted code
directly on that machine's OS — see the warning in config 2.

## Prerequisites

- **To build from source:** Go 1.25.13 or newer (`go.mod` pins the exact
  patch; older patches of 1.25 are missing stdlib fixes the supply-chain gate
  requires) and `git`.
- **To run with the Docker executor** (configs 1 and 3): a working Docker
  Engine the runner user can reach — either locally installed (config 1) or
  as the socket you mount in (config 3).
- **To run DinD** (config 4): a Linux host. `--privileged` containers are a
  Linux concept; there is no macOS/Windows equivalent.
- **For GPU jobs:** the vendor tooling the runner shells out to at startup —
  `nvidia-smi` (NVIDIA) or `rocm-smi`/`amd-smi` (AMD) — reachable from
  wherever the runner process actually runs. Configs 1 and 3 use the host's
  GPUs through the host daemon; config 2 sees the host's GPUs directly;
  config 4 has none (see the table above).

## Getting the runner

Pick one:

**Build from source**

```bash
git clone https://github.com/dragrace-dev/runner.git
cd runner
go build -o dragrace-runner ./cmd/runner
./dragrace-runner --version
```

A self-built binary has no update-signing key embedded, so `runner update`
will refuse to run — that's intentional (see
[Updating](#updating)). Pull and rebuild instead.

**Download a signed release binary**

```bash
curl -sSL https://get.dragrace.dev | sh
```

Runs interactively and asks whether you want test mode only (no account,
installs to `~/.local/bin`) or a full service install (systemd on Linux,
launchd on macOS, to `/usr/local/bin`). Non-interactively:
`curl -sSL https://get.dragrace.dev | sh -s -- --test-only` or `--full`.
Every binary the installer fetches is SHA-256 verified against the checksum
published alongside it before it's ever executed.

Prefer to fetch and verify it yourself: releases are at
`https://github.com/dragrace-dev/runner/releases`, each asset
`dragrace-runner-<os>-<arch>` ships with a matching `.sha256` file.

**Pull the container image** (configs 3 and 4 only — see below)

```bash
docker pull ghcr.io/dragrace-dev/runner:v1.2.3        # DooD
docker pull ghcr.io/dragrace-dev/runner:v1.2.3-dind    # DinD
```

Published images are multi-arch (`linux/amd64` and `linux/arm64`) — Docker
picks the right one for the host automatically, Apple Silicon included.
`ghcr.io/dragrace-dev/runner:latest` and `:latest-dind` track the most
recent release of each variant, for a quick pull with nothing else in mind.

Every tag is signed with Sigstore/Cosign and carries an SBOM and
`mode=max` provenance attestation. Verify before you deploy:

```bash
docker pull ghcr.io/dragrace-dev/runner:v1.2.3
docker image inspect --format '{{index .RepoDigests 0}}' ghcr.io/dragrace-dev/runner:v1.2.3

cosign verify ghcr.io/dragrace-dev/runner@sha256:<digest-from-above> \
  --certificate-identity-regexp '^https://github.com/dragrace-dev/runner/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Then deploy by that verified digest, not by a tag — `latest` included: a
name that can move after you've verified it defeats the point of verifying
it. Reach for `latest`/`latest-dind` only for a throwaway `docker run` you're
about to inspect yourself, never for something you deploy and walk away
from.

Or build the image yourself from this repository — `docker build` with no
`--target` yields `dood`, the hardened default; every caller below still
names its target explicitly:

```bash
docker build --target dood -t dragrace-runner:dood .
docker build --target dind -t dragrace-runner:dind .
```

## Try it without an account — test mode

Before wiring anything up to a backend, run a challenge locally. This works
identically in every configuration below and needs no login, no `RUNNER_ID`,
nothing:

```bash
runner test --challenge ./my-challenge --executor process
runner test -c ./challenges/1brc -s ./solutions/baseline --executor docker
runner test -c ./challenges/1brc --executor process -E ROW_COUNT=1000
runner test -c ./challenges/1brc --phase init --no-cache
runner test -c ./challenges/1brc -s ./sol -- --small
```

`--executor` here defaults to `docker` and is independent of how you'll run
the runner for real later — pass `process` to try a challenge with no Docker
installed at all. Run `runner test --help` for every flag (`--phase`,
`--data-dir`, `--verbose`, ...).

## Connecting to a backend — login

Every configuration below needs this once. It's a device-code flow: the
runner prints a URL and a code, you approve it in the browser, and it saves
NATS credentials locally (`~/.dragrace` by default, or wherever
`DRAGRACE_CREDS_DIR` points).

```bash
runner login --backend-url https://dragrace.dev
# or: BACKEND_URL=https://dragrace.dev runner login
```

Point `--backend-url` at your own backend instead if you're not connecting to
the hosted platform.

## Configuration 1 — Native, Docker executor (recommended)

The runner is a plain host process; it talks to your local Docker daemon the
normal way, no socket-mounting tricks needed because there's no container
boundary between them.

```bash
export RUNNER_ID=my-runner-01
runner login --backend-url https://dragrace.dev
runner \
  --runner-id "$RUNNER_ID" \
  --ws-backend-url wss://ws.dragrace.dev \
  --backend-url https://dragrace.dev
curl --fail http://127.0.0.1:8081/healthz
```

`scripts/install-runner.sh --full` sets this configuration up as a systemd
(Linux) or launchd (macOS) service for you, including the env file and log
locations — see its output for the exact service commands.

## Configuration 2 — Native, process executor (no Docker)

Job code runs directly under the runner's own OS user — no container
sandbox at all. **Read this before using it against a real backend:** an
untrusted submission now runs with whatever access that OS user has. The
runner refuses to start this way against a live backend unless you
explicitly acknowledge that:

```bash
export RUNNER_EXECUTOR=process
export RUNNER_NATIVE_RISK_ACCEPTED=true   # required — the runner exits without it
export RUNNER_ID=my-runner-01
runner login --backend-url https://dragrace.dev
runner
curl --fail http://127.0.0.1:8081/healthz
```

(`RUNNER_NATIVE_RISK_ACCEPTED` is not required for `runner test` — that's
local-only and always your own call.) `RUNNER_GPUS` must stay `none` here:
process-executor jobs already see every GPU on the host directly, so there's
nothing for that setting to scope. Process isolation still applies —
per-job process groups, RSS/CPU/process-count limits, `RLIMIT_FSIZE` for disk
— just not a container boundary.

## Configuration 3 — Container, Docker-out-of-Docker (DooD)

The runner runs in a container but drives the *host's* Docker daemon through
a mounted socket. Job sandboxes end up as siblings of the runner container on
the host, not children of it — which is exactly why the job checkout
directory has to exist at the **same absolute path** inside the container and
on the host: the runner names a path, the *host* daemon resolves it, and if
the two disagree you get `bind source path does not exist` at container
creation.

```bash
docker volume create dragrace-runner-creds

docker run --rm -it \
  -v dragrace-runner-creds:/root/.dragrace \
  -e BACKEND_URL=https://dragrace.dev \
  ghcr.io/dragrace-dev/runner:v1.2.3 login

export RUNNER_JOB_DIR=/var/lib/dragrace/jobs
docker run -d --name dragrace-runner --restart unless-stopped \
  -p 127.0.0.1:8081:8081 \
  --security-opt no-new-privileges:true --cap-drop ALL \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v "$RUNNER_JOB_DIR:$RUNNER_JOB_DIR" \
  -v dragrace-runner-creds:/root/.dragrace \
  -e DRAGRACE_WORK_DIR="$RUNNER_JOB_DIR" \
  -e WS_BACKEND_URL=wss://ws.dragrace.dev \
  -e BACKEND_URL=https://dragrace.dev \
  -e RUNNER_ID=my-runner-01 \
  ghcr.io/dragrace-dev/runner:v1.2.3
curl --fail http://127.0.0.1:8081/healthz
```

Or with the Compose file in this repository, which wires the same-path bind
up for you from one `RUNNER_JOB_DIR` variable:

```bash
cp .env.example .env   # then edit it — RUNNER_ID has no default, Compose
                        # refuses to start without one
docker compose build runner
docker compose run --rm runner login
docker compose up -d runner
```

Compose reads `.env` from the same directory automatically; exporting the
variables in your shell instead of copying the file works too.

The socket mount is root-equivalent on the host — keep it to a machine you'd
otherwise dedicate to running a Docker daemon anyway.

## Configuration 4 — Container, Docker-in-Docker (DinD)

The runner and its own `dockerd` share one container, so job sandboxes are
children of the runner container instead of siblings of everything else on
the host — better lateral isolation than DooD, at the cost of `--privileged`,
which is its own (different) path to the host. This is a trade, not an
upgrade over DooD; pick it when the runner has to sit next to other stacks on
a shared machine, not by default.

```bash
docker volume create dragrace-runner-creds
docker volume create dragrace-runner-docker   # the inner /var/lib/docker

docker run --rm -it \
  -v dragrace-runner-creds:/root/.dragrace \
  -e BACKEND_URL=https://dragrace.dev \
  ghcr.io/dragrace-dev/runner:v1.2.3-dind login

docker run -d --name dragrace-runner --restart unless-stopped \
  --privileged \
  --stop-timeout 60 \
  -p 127.0.0.1:8081:8081 \
  -v dragrace-runner-docker:/var/lib/docker \
  -v dragrace-runner-creds:/root/.dragrace \
  -e WS_BACKEND_URL=wss://ws.dragrace.dev \
  -e BACKEND_URL=https://dragrace.dev \
  -e RUNNER_ID=my-runner-01 \
  ghcr.io/dragrace-dev/runner:v1.2.3-dind
curl --fail http://127.0.0.1:8081/healthz
```

Or with Compose — the base stack in this repository stays DooD, so DinD is an
explicit override, never the default:

```bash
cp .env.example .env   # then edit it — RUNNER_ID has no default
docker compose -f docker-compose.yml -f docker-compose.dind.yml build runner
docker compose -f docker-compose.yml -f docker-compose.dind.yml run --rm runner login
docker compose -f docker-compose.yml -f docker-compose.dind.yml up -d runner
```

No socket mount and no `RUNNER_JOB_DIR` bind here: the runner and its daemon
already share a filesystem, so job checkouts just stay inside the container.

`--stop-timeout 60` / `stop_grace_period: 1m` in Compose is load-bearing, not
cosmetic — `docker stop` only signals PID 1 (the entrypoint), which stops the
runner first and then gives the inner `dockerd` up to
`RUNNER_DIND_STOP_TIMEOUT` (20s default) to flush containerd state and
unmount overlays before SIGKILL. The 10-second Docker default guarantees a
mid-shutdown kill on a volume you meant to keep intact.

**What this variant costs, concretely:**

- **Cold image cache.** The inner daemon starts with nothing pulled. Persist
  `/var/lib/docker` as a volume (as above) or every recreate re-pulls every
  challenge base image from scratch.
- **Slower cold start.** `dockerd` has to come up before the runner
  registers — budget tens of seconds on a fresh volume. Raise
  `RUNNER_DIND_DAEMON_TIMEOUT` (default `60`, seconds) if the entrypoint gives
  up first.
- **No GPU support.** The inner daemon isn't the host's; no device reaches
  it. The entrypoint refuses to start if `RUNNER_GPUS` asks for a card rather
  than silently falling back to CPU. Use config 1 or 3 on GPU hosts.
- **Nested cgroup.** Job containers run under the runner container's own
  cgroup, so a memory/CPU cap on the runner container is a hard ceiling for
  every job it runs too — set it above what your challenges legitimately
  need, or they'll be rejected outright with an "exceeds host capacity"
  error instead of running.
- **Inner bridge MTU/DNS.** The inner `docker0` defaults to MTU 1500. If the
  outer network is itself encapsulated (VPN, VXLAN, most clouds), large
  packets get silently dropped — `git clone` or `pip install` hangs
  mid-transfer. Fix with `RUNNER_DIND_DOCKERD_ARGS=--mtu=1400`. DNS inside job
  containers is the inner daemon's own resolver, not the host's or Compose's.
- **Disk.** Image store, build cache and every job's container layers all
  live in that one volume — size and prune it on a schedule.

**Troubleshooting:** `dockerd`'s own log is kept out of the container log so
job output stays readable — read it directly, and check the inner daemon:

```bash
docker exec dragrace-runner tail -f /var/log/dockerd.log
docker exec dragrace-runner docker info    # the daemon INSIDE the container
```

An immediate `dockerd exited before becoming ready` almost always means the
container was started without `--privileged`.

## Configuration reference

The full list is in `runner --help` and in `internal/config/config.go`; the
ones that matter across configurations:

| Variable | Meaning | Default |
|---|---|---|
| `RUNNER_ID` | Unique name for this runner | `runner-default` |
| `WS_BACKEND_URL` | NATS endpoint the runner connects to | `wss://ws.dragrace.dev` |
| `BACKEND_URL` | HTTP endpoint used by `runner login` | `https://dragrace.dev` |
| `RUNNER_EXECUTOR` | `docker` or `process` | `docker` |
| `RUNNER_NATIVE_RISK_ACCEPTED` | Required `true` to run `process` against a live backend | `false` |
| `DOCKER_HOST` | Docker socket the executor talks to | `unix:///var/run/docker.sock` |
| `RUNNER_HEALTH_ADDR` | Local health listener | `127.0.0.1:8081` |
| `RUNNER_GPUS` | GPU exposure ceiling for job containers: `none`, `all`, or `0,1` | `none` |
| `RUNNER_AIRGAPPED` | Force network egress off for every job phase, overriding challenge policy | `false` |
| `DRAGRACE_CREDS_DIR` | Where `runner login` writes credentials | `~/.dragrace` |

Every flag has a matching env var and takes priority when both are set — run
`runner --help` for the complete, current list including DinD-only entrypoint
variables (`RUNNER_DIND_*`).

## Operating it

- **Health:** `GET /healthz` on `RUNNER_HEALTH_ADDR` (`127.0.0.1:8081` by
  default) returns 200 while `idle`/`busy`, 503 while starting, registering,
  stopping, or offline. The JSON body carries runner ID, version, current
  job, and last heartbeat result.
- **Heartbeats:** every 30s; the backend treats one as stale after 60s and
  requeues an in-flight assignment after 90s of silence.
- **Stopping:** send `SIGTERM` (or `docker stop` / `systemctl stop`). The
  runner finishes its shutdown sequence and sends a final `offline`
  heartbeat before exiting — give it time rather than `SIGKILL`ing it,
  especially on DinD (see `--stop-timeout` above).
- **Revoking:** revoking a runner's API key on the backend marks it offline
  immediately and rejects further events from it. Issue a new key and run
  `runner login` again to bring it back.

### Updating

- **Release binary:** `runner update` self-updates in place, verifying an
  Ed25519 signature against the key embedded at build time
  (`RUNNER_UPDATE_PUBLIC_KEY`).
- **Self-built binary:** no key is embedded unless you passed one at build
  time, so `runner update` fails closed rather than applying an unverified
  binary. Update by pulling and rebuilding instead.
- **Container image:** don't run `runner update` inside it — even supplying
  the key at runtime, the replacement binary would land in the container's
  writable layer and vanish on the next recreate. Pull the next tag, verify
  its digest (see [Getting the runner](#getting-the-runner)), and recreate
  the container. Roll back the same way, onto the previous known-good
  digest.

## Security model

None of these configurations sandbox hostile code on a machine you actually
care about — they trade *which* privilege you grant, not whether you grant
one:

- **Native, Docker executor** (config 1): jobs run in a container with no
  extra capabilities and `no-new-privileges`; the runner process itself runs
  as whatever host user starts it.
- **Native, process executor** (config 2): no container boundary at all —
  jobs run as the runner's own OS user, contained only by per-job process
  groups, resource limits, and (on POSIX) `RLIMIT_FSIZE`. This is why it's
  gated behind an explicit `RUNNER_NATIVE_RISK_ACCEPTED=true`.
- **DooD** (config 3): the mounted host socket is root-equivalent on the
  host — anything that can reach it can do anything the host's Docker daemon
  can. Job sandboxes are siblings of every other container on that host.
- **DinD** (config 4): `--privileged` is a different, broader grant to the
  container itself. In exchange, job sandboxes can't see anything else on
  the host — better lateral isolation, worse vertical isolation than DooD.

Runner identity never rests on a client-embedded secret: authority comes from
server-issued NATS credentials plus subject-level ACLs, and the backend
derives identity from the authenticated subject, never from a payload field a
compromised runner could forge. A compromised runner can still lie about its
own metrics — result integrity ultimately depends on the execution isolation
above plus challenge-side controls, not on trusting the runner's self-reports.
