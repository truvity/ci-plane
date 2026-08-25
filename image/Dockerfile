# The Truvity ARC runner image (extracted from truvity/gitops
# ci/runners/default, 2026-08-25 — INF/see the extraction ticket).
#
# Doctrine (Oleg, 2026-08-13): the image contains ONLY what devbox
# cannot deliver — the bootstrap (nix + devbox), pre-devbox shell
# semantics (bash as sh), binaries outside nixpkgs (goreleaser-pro:
# license-gated; go-cache-plugin: must pre-exist before any go
# invocation it caches), and the docker toolchain (client + buildx:
# essential, ruled in-image 2026-08-13 — daemonless, the daemon never
# ships). EVERYTHING else — aws, kubectl, helm, toolchains — arrives
# per job from the repo's own devbox.json through the nix cache.
# Repo-specific OR cluster-specific content in this image is a bug
# (builder registration lives in workflows, not here). The one known
# debt to that rule is the extra-substituters line near the bottom:
# it names Truvity's in-cluster nix cache, harmless elsewhere (dead
# substituter, upstream fallback) but Truvity-shaped.
#
# Multi-arch: built as a manifest list (amd64 + arm64) by
# .github/workflows/release.yaml on a v* tag; consumers pin by DIGEST
# (Truvity's fleet pins in gitops cfg/versions.yaml and pulls through
# each account's ECR pull-through cache, github/ prefix -> ghcr.io).
#
# Releases are TAG-GATED: renovate keeps the pins below current and
# automerges, but nothing publishes until a v* tag (creation restricted
# by ruleset). A patch tag on an unchanged Dockerfile is the legitimate
# "pure refresh" release (base-image/CVE pickup). EVERY version ARG
# here needs a `# renovate:` line directly above it; a pin without one
# is invisible, and invisible is indistinguishable from current.

# PINNED, annotated — the automatic-chain decision (INF-573 follow-up,
# 2026-08-25) that replaced the old `latest` exception: `latest` was
# invisible to renovate, so upstream runner (and its CVE) pickups
# happened only as a side effect of some OTHER image change. Pinned +
# annotated, renovate bumps it, automerge lands it, and the next tag
# ships it. Compatibility with the ARC controller still holds: renovate
# moves this pin within days of upstream, which is faster than the
# controller chart stream moves.
# renovate: datasource=docker depName=ghcr.io/actions/actions-runner
ARG RUNNER_VERSION=2.336.0
# renovate: datasource=github-releases depName=goreleaser/goreleaser-pro
ARG GORELEASER_VERSION=v2.17.1

# ── go-cache-plugin: built from source (upstream publishes no binary
# releases). CROSS-compiled on the build platform: the previous form
# ran the arm64 half under QEMU, and that one emulated `go install`
# was 841s of the 18.5-minute image build (v1.0.1 measurement) vs
# 139s native. CGO is off, so Go cross-compiles for free. Version
# pinned — @latest in a Dockerfile is unrepeatable supply chain.
# renovate: datasource=docker depName=golang
FROM --platform=$BUILDPLATFORM golang:1.27 AS gocache
ARG TARGETARCH
# go install rejects GOBIN for cross-builds and drops the binary in
# /go/bin/linux_$GOARCH/ instead of /go/bin/ — normalize to /out for
# the COPY below (native case: build platform == target platform).
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    # renovate: datasource=github-tags depName=tailscale/go-cache-plugin
    go install github.com/tailscale/go-cache-plugin/cmd/go-cache-plugin@v0.1.1 \
 && mkdir -p /out \
 && cp "/go/bin/linux_${TARGETARCH}/go-cache-plugin" /out/ 2>/dev/null \
 || cp /go/bin/go-cache-plugin /out/

# ── the runner ──
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}
ARG GORELEASER_VERSION

USER root

# Ledger lesson 9: devbox/moon hooks assume bash semantics; upstream sh
# is dash. xz: the nix installer unpacks an .xz tarball and the minimal
# base ships without it.
RUN ln -sf /bin/bash /bin/sh \
 && apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends xz-utils \
 && rm -rf /var/lib/apt/lists/*

# goreleaser-pro (the binary; the LICENSE KEY is an org secret and never
# ships in an image).
# Download-then-extract (no pipe): a curl failure must fail the step,
# not feed tar garbage — v1's pipe hid a 404 as a tar error.
RUN ARCH=$(dpkg --print-architecture) \
 && GR_ARCH=$([ "$ARCH" = "arm64" ] && echo arm64 || echo x86_64) \
 && curl -fsSL -o /tmp/goreleaser.tgz "https://github.com/goreleaser/goreleaser-pro/releases/download/${GORELEASER_VERSION}/goreleaser-pro_Linux_${GR_ARCH}.tar.gz" \
 && tar -xz -C /usr/local/bin -f /tmp/goreleaser.tgz goreleaser \
 && rm /tmp/goreleaser.tgz \
 && goreleaser --version

COPY --from=gocache /out/go-cache-plugin /usr/local/bin/go-cache-plugin

# docker client + buildx plugin (daemonless: CLIENT ONLY, builds go to
# the shared rootless builders over --driver remote). Static binaries,
# arch-explicit; buildx lands on the system-wide plugin path so no
# per-job symlink into ~/.docker is needed.
# The Docker Hub `docker` image is versioned identically to the CLI, so
# it tracks these static tarballs exactly; moby/moby publishes no
# matching release tag. semver versioning so the -dind / -windowsservercore
# tags are read as prereleases and ignored.
# renovate: datasource=docker depName=docker versioning=semver
ARG DOCKER_VERSION=29.7.2
# renovate: datasource=github-releases depName=docker/buildx
ARG BUILDX_VERSION=v0.36.1
RUN ARCH=$(dpkg --print-architecture) \
 && DOCKER_ARCH=$([ "$ARCH" = "arm64" ] && echo aarch64 || echo x86_64) \
 && curl -fsSL -o /tmp/docker.tgz "https://download.docker.com/linux/static/stable/${DOCKER_ARCH}/docker-${DOCKER_VERSION}.tgz" \
 && tar -xz -C /tmp -f /tmp/docker.tgz docker/docker \
 && mv /tmp/docker/docker /usr/local/bin/docker \
 && rm -rf /tmp/docker /tmp/docker.tgz \
 && mkdir -p /usr/local/lib/docker/cli-plugins \
 && curl -fsSL -o /usr/local/lib/docker/cli-plugins/docker-buildx "https://github.com/docker/buildx/releases/download/${BUILDX_VERSION}/buildx-${BUILDX_VERSION}.linux-${ARCH}" \
 && chmod +x /usr/local/lib/docker/cli-plugins/docker-buildx \
 && docker --version \
 && docker buildx version

# devbox binary (its packages come per job, from the repo's devbox.json).
# Straight from the GitHub release with explicit arch: the official
# installer both insists on sudo (which the base's sudoers config
# refuses even for root) and downloads an arch-opaque URL — useless
# under a QEMU multi-arch build.
# renovate: datasource=github-releases depName=jetify-com/devbox
ARG DEVBOX_VERSION=0.18.0
RUN ARCH=$(dpkg --print-architecture) \
 && curl -fsSL -o /tmp/devbox.tgz "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/devbox_${DEVBOX_VERSION}_linux_${ARCH}.tar.gz" \
 && tar -xz -C /usr/local/bin -f /tmp/devbox.tgz devbox \
 && rm /tmp/devbox.tgz \
 && devbox version

USER runner
# Docker's USER directive sets the uid, not the environment — nix's
# profile install resolves the profile dir from $USER/$HOME and dies
# with "unable to install Nix into your default profile" without them.
ENV USER=runner \
    HOME=/home/runner

# Single-user nix owned by the runner user (no daemon — containers have
# no systemd; the official installer's --no-daemon mode is the proven
# in-image shape). Substituters: the in-cluster read-through cache of
# cache.nixos.org first (nginx, ci-cache namespace — see gitops
# arc-nix-cache.yaml), then nix's default upstream as automatic
# fallback: cache down = slower, never broken. The S3 push cache is
# retired with this — and with it the require-sigs=false it forced:
# everything now originates upstream, whose narinfo signatures pass
# through the proxy intact and verify against nix's default trusted
# key.
# filter-syscalls off FOR THE BUILD ONLY: the arm64 half runs under
# QEMU user emulation, where nix's seccomp BPF load fails ("Invalid
# argument"); real arm64 hardware at runtime is unaffected and the
# baked nix.conf stays clean.
RUN NIX_CONFIG="filter-syscalls = false" \
    sh <(curl -fsSL https://nixos.org/nix/install) --no-daemon \
 && mkdir -p /home/runner/.config/nix \
 && printf '%s\n' \
      'experimental-features = nix-command flakes' \
      'extra-substituters = http://nix-cache.ci-cache.svc.cluster.local' \
      > /home/runner/.config/nix/nix.conf

ENV PATH="/home/runner/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"
