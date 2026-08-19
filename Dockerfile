# DragRace runner image, two runtime variants from one recipe.
#
#   target `dood` (default)  -> ghcr.io/dragrace-dev/runner:X.Y.Z
#       Drives an *existing* Docker daemon through a mounted socket and never
#       starts one of its own. Alpine + git + ca-certificates. See #68 and
#       docs/ENV_CONFIGURATION.md for the bind-path rules that come with that.
#
#   target `dind`            -> ghcr.io/dragrace-dev/runner:X.Y.Z-dind
#       Runs its own dockerd *inside the same container* as the runner, so job
#       sandboxes are children of this container instead of siblings of the
#       rest of the stack. Requires `--privileged`. See #70,
#       docs/RUNNER_THREAT_MODEL.md for the security trade-off, and
#       docs/RUNNER_OPERATIONS.md for the operational cost.
#
# Why one Dockerfile with two targets rather than a second Dockerfile:
#   - both variants must ship the *same* runner binary, so they must share the
#     builder stage; a second file would duplicate it and let it drift;
#   - scripts/check-supply-chain-pins.sh only scans files literally named
#     `Dockerfile` (`find . -name Dockerfile`), so a `Dockerfile.dind` would
#     silently escape the base-image digest pin;
#   - BuildKit only materialises the stages a target actually needs, so
#     building `dood` never pulls the heavy dind base. The DooD variant is not
#     one byte larger for the dind stage existing here.
#
# `dood` is deliberately the LAST stage: an implicit `docker build` with no
# --target must yield the hardened, non-privileged variant. Every caller
# (docker-compose*.yml, .github/workflows/build.yml) nevertheless names its
# target explicitly, so the file order is a safety net, not a contract.
#
# This file lives in runner/ rather than docker/runner/ on purpose:
# .github/workflows/split-runner.yml mirrors this directory to
# dragrace-dev/runner with `rsync -a --delete`, and that public repository is
# where the image is built and published from.

# Build stage
# Must stay >= the go directive in runner/go.mod: the official images set
# GOTOOLCHAIN=local, so an older toolchain here fails the build outright
# rather than downloading the required one. The 1.25 line stopped getting
# alpine3.22 builds at go1.25.11, hence the move to alpine3.23.
FROM golang:1.25.13-alpine3.23@sha256:42fc3368d1c50170a452f2bf4a1dfd292a065870c3f258d799aad4316671cb69 AS builder

WORKDIR /app

# Install git (required for go mod download)
RUN apk add --no-cache git

# Copy all source files first
COPY . .

# Download dependencies pinned by go.mod/go.sum
RUN go mod download

# Version is stamped, never the update-verification key: build arguments end up
# in image history, provenance and SBOM, and docs/RELEASE_SECURITY.md forbids
# putting release key material there. RUNNER_UPDATE_PUBLIC_KEY stays a runtime
# environment variable for this image.
#
# Default matches internal/version.Version's own compiled-in default (0.1.0),
# not an obviously-fake "dev" placeholder: docker-compose.yml/.smoke.yml/.dind.yml
# build this image without passing --build-arg VERSION, and the backend rejects
# runners below MIN_RUNNER_VERSION (0.1.0 by default) — an unstamped build must
# stay compatible with a bare `docker compose up`, not just tagged releases
# (.github/workflows/build-runner.yml passes the real VERSION explicitly).
ARG VERSION=0.1.0

# Same reproducibility flags as the released binaries (.github/workflows/build-runner.yml).
RUN CGO_ENABLED=0 GOOS=linux go build -mod=readonly -trimpath -buildvcs=false \
        -ldflags "-s -w -buildid= -X dragrace/internal/version.Version=${VERSION}" \
        -o /runner ./cmd/runner

# ─────────────────────────────────────────────────────────────────────────────
# Runtime stage: docker-in-docker variant (`-dind` tag).
#
# dockerd runs in THIS container, not in a sidecar. That is the whole point:
# a sidecar daemon resolves bind-mount paths in its own filesystem, which
# reproduces the #68 path-aliasing bug exactly (the runner would still name
# paths the daemon cannot see). Sharing one container means one filesystem and
# one set of paths, so no aliasing is possible.
#
# Sizing note for whoever looks at the trivy report: this base *does* carry a
# full dockerd, containerd, runc, iptables and the docker CLI. That surface is
# the price of the variant, not an oversight — see the DooD stage below for the
# opposite trade-off.
FROM docker:29.7.2-dind@sha256:12e683a161823b2a839aeea999b9d960e6e1f9a97b1679ad6b441982e2d9cf07 AS dind

# ca-certificates and git for the same reasons as the DooD stage; the docker
# base ships neither reliably, and `apk add` is a no-op when it already has one.
RUN apk add --no-cache ca-certificates git

COPY --from=builder /runner /usr/local/bin/runner
COPY dind-entrypoint.sh /usr/local/bin/dragrace-dind-entrypoint.sh
RUN chmod 0755 /usr/local/bin/dragrace-dind-entrypoint.sh

ENV DOCKER_HOST=unix:///var/run/docker.sock
ENV RUNNER_ID=runner-default
ENV RUNNER_HEALTH_ADDR=0.0.0.0:8081

# Emptied on purpose. With DOCKER_TLS_CERTDIR set, `dockerd-entrypoint.sh`
# generates a CA and publishes the daemon on tcp://0.0.0.0:2376; with it empty
# it publishes tcp://0.0.0.0:2375 *unauthenticated*. Either would hand root on
# this container to anything that can reach the compose network. The entrypoint
# therefore passes explicit `dockerd --host=unix://…` arguments, which skips
# that whole default-argument block: this daemon listens on a unix socket only.
ENV DOCKER_TLS_CERTDIR=""

# Tells runner/internal/docker that `docker info` describes the machine, not
# this container: see clampToOwnCgroup in internal/docker/capacity.go.
ENV RUNNER_DOCKER_NESTED=true

# Inherited from the base, restated because it is load-bearing here: without a
# volume, /var/lib/docker lands in the container's writable overlay layer, which
# means overlayfs-on-overlayfs and a full image re-pull on every recreate.
VOLUME /var/lib/docker

EXPOSE 8081
# start-period is longer than the DooD variant's: a cold start pays for dockerd
# coming up before the runner's health endpoint exists at all.
HEALTHCHECK --interval=30s --timeout=3s --start-period=90s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8081/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/dragrace-dind-entrypoint.sh"]

# ─────────────────────────────────────────────────────────────────────────────
# Runtime stage: docker-out-of-docker variant (default target).
#
# Deliberately NOT docker:*-dind and not even docker:*-cli. The runner talks to
# the daemon through the Go client library (runner/internal/docker), never by
# shelling out to a `docker` binary — grep for exec.Command under runner/ and
# the only external commands are git, ps and the GPU probes. A dind base ships a
# whole dockerd that this image never starts; a cli base ships a client nothing
# calls. Both are pure CVE surface for the trivy gate and scripts/check-runner-vulns.mjs.
#
# What genuinely has to be here:
#   - ca-certificates: HTTPS to the backend and to challenge/solution remotes
#   - git:             runner/internal/git shells out to `git clone`/`checkout`
#   - busybox wget:    the HEALTHCHECK below
FROM alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40 AS dood

RUN apk add --no-cache ca-certificates git

# Copy the runner binary
COPY --from=builder /runner /usr/local/bin/runner

# Set environment variables
ENV DOCKER_HOST=unix:///var/run/docker.sock
ENV RUNNER_ID=runner-default
ENV RUNNER_HEALTH_ADDR=0.0.0.0:8081

EXPOSE 8081
HEALTHCHECK --interval=30s --timeout=3s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8081/healthz || exit 1

# Run the runner
ENTRYPOINT ["/usr/local/bin/runner"]
