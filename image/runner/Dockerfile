# The ARC runner image (extracted from a private estate repository,
# 2026-08-25).
#
# Doctrine (2026-08-13): the image contains ONLY what devbox
# cannot deliver — the bootstrap (nix + devbox), pre-devbox shell
# semantics (bash as sh), binaries outside nixpkgs (runner,
# license-gated; the Go cache agent: must pre-exist before any go
# invocation it caches), and the docker toolchain (client + buildx:
# essential, ruled in-image 2026-08-13 — daemonless, the daemon never
# ships). EVERYTHING else — aws, kubectl, helm, toolchains — arrives
# per job from the repo's own devbox.json through the nix cache.
# Repo-specific OR cluster-specific content in this image is a bug
# (builder registration lives in workflows, not here). The one known
# debt to that rule is the substituters line near the bottom: it names
# an in-cluster nix cache, which is cluster-shaped. It is a build ARG
# (NIX_SUBSTITUTERS) so a fleet building its own image sets its own; the
# default is unchanged.
#
# IT IS NOT FREE ON A CLUSTER WITHOUT THAT CACHE, which an earlier
# version of this comment claimed ("harmless elsewhere"). If the name
# does not resolve, nix retries before falling through -- measured on a
# consumer cluster, eight attempts with the runners chart's backoff:
#
#   Could not resolve host: nix-cache.<namespace>.svc.cluster.local;
#   retrying in 4281 ms (attempt 1/8) ... 11382 ... 36723 ... 58570
#
# Minutes per job, on every nix invocation. Consumers of the PUBLISHED
# image override it without rebuilding: arc-runners delivers nixConfig
# as NIX_CONFIG, which beats this file. v4.1.0 adds a dedicated chart
# value for exactly that override, `nixCache.url` -- unset by default,
# so this baked default (and every existing consumer's behaviour) is
# unchanged; see CHANGELOG.md and docs/day-1-install.md. Un-baking this
# default itself is a SEPARATE, coordinated change for a later release,
# once every consumer that relies on it (the maintainers' own estate
# included) has set `nixCache.url` explicitly first.
#
# Multi-arch: built as a manifest list (amd64 + arm64) by
# .github/workflows/release.yaml on a v* tag; consumers pin by DIGEST
# (the maintainers' fleet pins it in its own estate repository and pulls
# through each account's ECR pull-through cache, github/ -> ghcr.io).
#
# Releases are TAG-GATED: renovate keeps the pins below current and
# automerges, but nothing publishes until a v* tag (creation restricted
# by ruleset). A patch tag on an unchanged Dockerfile is the legitimate
# "pure refresh" release (base-image/CVE pickup). EVERY version ARG
# here needs a `# renovate:` line directly above it; a pin without one
# is invisible, and invisible is indistinguishable from current.

# PINNED, annotated — the automatic-chain decision (2026-08-25) that
# replaced the old `latest` exception: `latest` was
# invisible to renovate, so upstream runner (and its CVE) pickups
# happened only as a side effect of some OTHER image change. Pinned +
# annotated, renovate bumps it, automerge lands it, and the next tag
# ships it. Compatibility with the ARC controller still holds: renovate
# moves this pin within days of upstream, which is faster than the
# controller chart stream moves.
# renovate: datasource=docker depName=ghcr.io/actions/actions-runner
ARG RUNNER_VERSION=2.337.0

# The nix substituter list baked into the image. Default unchanged, so a
# rebuild produces what it always did; a fleet whose cache lives
# elsewhere overrides it here rather than patching the file, OR, without
# a rebuild, through arc-runners' `nixCache.url` (v4.1.0). Name the
# FULL list -- the `extra-` form appends to nix's default and the cache
# is then never chosen (155 GB/day from upstream, measured).
ARG NIX_SUBSTITUTERS="http://nix-cache.ci-cache.svc.cluster.local https://cache.nixos.org/"

# PINNED, and it must MATCH image/nix-worker/Dockerfile exactly.
#
# Remote building is a protocol between two nix installations. While one
# manifest served both the runner and the worker they matched by
# construction -- but nothing pinned them: the installer was fetched from
# nixos.org/nix/install, whatever that served that day. Now that the
# worker is its own image, two builds on different days would drift
# silently, and a drift shows up as a confusing build failure rather than
# as a version error. hack/nix-parity.sh refuses a release where the two
# disagree.
# renovate: datasource=github-releases depName=NixOS/nix
ARG NIX_VERSION=2.35.2

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

# The OpenBao bootstrap is a static standard-library binary: JSON and TLS
# handling are security boundaries, not shell parsing. It invokes only the
# image's pinned OpenSSH client to create and inspect ephemeral keys.
COPY nix-worker-client/ /src/nix-worker-client/
RUN cd /src/nix-worker-client \
 && go mod download \
 && CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' \
      -o /out/nix-worker-client-setup .

# ── the runner ──
FROM ghcr.io/actions/actions-runner:${RUNNER_VERSION}
ARG GORELEASER_VERSION

USER root

# Ledger lesson 9: devbox/moon hooks assume bash semantics; upstream sh
# is dash. xz: the nix installer unpacks an .xz tarball and the minimal
# base ships without it.
RUN ln -sf /bin/bash /bin/sh \
 && apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends openssh-server tini xz-utils \
 && rm -rf /var/lib/apt/lists/*

# goreleaser-pro is deliberately ABSENT: the Pro EULA's no-distribution
# clause covers the binary even without the key, and this image is
# public. Consumers install it job-time on their own runners — through
# their own tool pins, or release-private's install-goreleaser-pro
# input (ci-workflows v2.13.1).

# The ci-cache agent used to be copied in here beside the plugin, and is
# gone. The image carried two Go cache clients while the estate decided
# between them, and measurement decided against the agent: it costs
# about a second over Go's own GOCACHE, and what actually caps a build's
# fetch rate is `-p` -- how many packages compile at once, hence how many
# lookups are in flight -- at roughly 16 MB/s per CPU. No cache client
# moves that number. The maintainers' estate retired the server it
# talked to as well.
#
# Dropping it is worth more than the ~39MB: every agent bump cost a
# ci-plane release and an ARC rollout of the whole fleet (v2.3.0 and
# v2.4.0 in one night) for a binary no job was using.
#
# Safe because the refusal that used to greet a missing binary is gone.
# ci-actions' setup-devbox now WARNS on `go-cache-server` and falls
# through to the bucket (released before this) rather than
# failing with "ci-cache not on PATH -- runner image too old". That
# ordering is the whole risk here: pins move on their own schedule, not
# this repository's, so a caller on an old pin has to degrade, not break.
#
# go-cache-plugin below is now the only Go cache client in the image,
# and it stays until setup-devbox fetches it from the ci-cache release
# -- after which the image carries no cache client at all.
COPY --from=gocache /out/go-cache-plugin /usr/local/bin/go-cache-plugin
COPY --from=gocache /out/nix-worker-client-setup /usr/local/bin/nix-worker-client-setup

# docker client + buildx plugin (daemonless: CLIENT ONLY, builds go to
# the shared rootless builders over --driver remote). Static binaries,
# arch-explicit; buildx lands on the system-wide plugin path so no
# per-job symlink into ~/.docker is needed.
# The Docker Hub `docker` image is versioned identically to the CLI, so
# it tracks these static tarballs exactly; moby/moby publishes no
# matching release tag. semver versioning so the -dind / -windowsservercore
# tags are read as prereleases and ignored.
# renovate: datasource=docker depName=docker versioning=semver
ARG DOCKER_VERSION=29.8.0
# renovate: datasource=github-releases depName=docker/buildx
ARG BUILDX_VERSION=v0.37.1
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
ARG DEVBOX_VERSION=0.18.4
RUN ARCH=$(dpkg --print-architecture) \
 && curl -fsSL -o /tmp/devbox.tgz "https://github.com/jetify-com/devbox/releases/download/${DEVBOX_VERSION}/devbox_${DEVBOX_VERSION}_linux_${ARCH}.tar.gz" \
 && tar -xz -C /usr/local/bin -f /tmp/devbox.tgz devbox \
 && rm /tmp/devbox.tgz \
 && devbox version

# accessctl (truvity/access-roster's CLI) and r2broker (truvity/
# cloudflare's R2 temporary-credentials broker CLI). `accessctl r2`
# authenticates against the estate's access-roster issuer and execs the
# real `r2broker` binary unchanged
# (access-roster docs/reference/accessctl.md#r2) -- accessctl alone has
# nothing to run, so the two ship together or not at all. Together they
# are what an `awsConfig.credentialProcess` line (arc-runners v4.2.0)
# names: `accessctl r2 --service-url <broker> -- credentials --bucket
# <bucket> --prefix <prefix>`, so the Go build cache's S3-compatible
# client can read a short-lived, prefix-scoped credential instead of a
# static key in a Secret.
#
# BAKED, same reasoning as go-cache-plugin above: the credential_process
# the AWS SDK execs is invoked BY that client, which already has to
# pre-exist the first `go` invocation of a job -- so whatever it in turn
# execs has to pre-exist it too. Neither binary is required by a job that
# never sets AWS_CONFIG_FILE at a line naming `accessctl r2`: idle
# binaries on PATH, invoked by nothing.
#
# Verified against that release's own checksums.txt rather than a
# hand-pinned digest (contrast image/nix-worker/Dockerfile's opkssh,
# hand-maintained because ITS upstream ships no per-release checksums
# file): a Renovate version bump here downloads the NEW release's
# checksums.txt in the same build, so the version pin and its checksum
# can never desync the way two separately hand-maintained values could.
# renovate: datasource=github-releases depName=truvity/access-roster
ARG ACCESSCTL_VERSION=1.39.2
# renovate: datasource=github-releases depName=truvity/cloudflare
ARG R2BROKER_VERSION=2.7.1
RUN ARCH=$(dpkg --print-architecture) \
 && cd /tmp \
 && curl -fsSLO "https://github.com/truvity/access-roster/releases/download/v${ACCESSCTL_VERSION}/access-roster_${ACCESSCTL_VERSION}_checksums.txt" \
 && curl -fsSLO "https://github.com/truvity/access-roster/releases/download/v${ACCESSCTL_VERSION}/accessctl_${ACCESSCTL_VERSION}_linux_${ARCH}.tar.gz" \
 && grep -F "  accessctl_${ACCESSCTL_VERSION}_linux_${ARCH}.tar.gz" "access-roster_${ACCESSCTL_VERSION}_checksums.txt" | sha256sum -c - \
 && tar -xzf "accessctl_${ACCESSCTL_VERSION}_linux_${ARCH}.tar.gz" accessctl \
 && install -m 0755 accessctl /usr/local/bin/accessctl \
 && rm -f "access-roster_${ACCESSCTL_VERSION}_checksums.txt" "accessctl_${ACCESSCTL_VERSION}_linux_${ARCH}.tar.gz" accessctl \
 && curl -fsSLO "https://github.com/truvity/cloudflare/releases/download/v${R2BROKER_VERSION}/cloudflare_${R2BROKER_VERSION}_checksums.txt" \
 && curl -fsSLO "https://github.com/truvity/cloudflare/releases/download/v${R2BROKER_VERSION}/r2broker_${R2BROKER_VERSION}_linux_${ARCH}.tar.gz" \
 && grep -F "  r2broker_${R2BROKER_VERSION}_linux_${ARCH}.tar.gz" "cloudflare_${R2BROKER_VERSION}_checksums.txt" | sha256sum -c - \
 && tar -xzf "r2broker_${R2BROKER_VERSION}_linux_${ARCH}.tar.gz" r2broker \
 && install -m 0755 r2broker /usr/local/bin/r2broker \
 && rm -f "cloudflare_${R2BROKER_VERSION}_checksums.txt" "r2broker_${R2BROKER_VERSION}_linux_${ARCH}.tar.gz" r2broker \
 && accessctl --version \
 && r2broker --version

# sluisctl (truvity/sluis's CLI). `sluisctl r2` takes the same flags as
# `accessctl r2` (--audience, --role, --issuer, --service-url) and is the
# successor for an `awsConfig.credentialProcess` line. accessctl and
# r2broker stay above, so a manifest that still names `accessctl r2`
# keeps working and moving a consumer back is a manifest-only rollback.
# Verified against the release's own checksums.txt, as above.
# renovate: datasource=github-releases depName=truvity/sluis
ARG SLUISCTL_VERSION=1.71.0
RUN ARCH=$(dpkg --print-architecture) \
 && cd /tmp \
 && curl -fsSLO "https://github.com/truvity/sluis/releases/download/v${SLUISCTL_VERSION}/checksums.txt" \
 && curl -fsSLO "https://github.com/truvity/sluis/releases/download/v${SLUISCTL_VERSION}/sluisctl_${SLUISCTL_VERSION}_linux_${ARCH}.tar.gz" \
 && grep -F "  sluisctl_${SLUISCTL_VERSION}_linux_${ARCH}.tar.gz" checksums.txt | sha256sum -c - \
 && tar -xzf "sluisctl_${SLUISCTL_VERSION}_linux_${ARCH}.tar.gz" sluisctl \
 && install -m 0755 sluisctl /usr/local/bin/sluisctl \
 && rm -f checksums.txt "sluisctl_${SLUISCTL_VERSION}_linux_${ARCH}.tar.gz" sluisctl \
 && sluisctl --version

# The nixbld accounts are what a multi-user nix daemon builds as; kept
# from the days this manifest also served the Nix workers (split into
# image/nix-worker on 2026-09-24), because the single-user install below
# has always run with them present. v5.0.0 removed the rest of that
# worker role from this image -- the `nixremote` login, sshd's config and
# the worker scripts -- together with the chart's fallback that could
# still run a worker on it.
RUN groupadd --system nixbld \
 && for i in $(seq 1 32); do \
      useradd --system --gid nixbld --home-dir /var/empty \
        --shell /usr/sbin/nologin "nixbld${i}"; \
    done \
 && nixbld_members=$(for i in $(seq 1 32); do printf 'nixbld%s,' "$i"; done) \
 && gpasswd -M "${nixbld_members%,}" nixbld

USER runner
# Docker's USER directive sets the uid, not the environment — nix's
# profile install resolves the profile dir from $USER/$HOME and dies
# with "unable to install Nix into your default profile" without them.
ENV USER=runner \
    HOME=/home/runner

# Single-user nix owned by the runner user (no daemon — containers have
# no systemd; the official installer's --no-daemon mode is the proven
# in-image shape). Substituters: the in-cluster read-through cache of
# cache.nixos.org first (nginx, ci-builders' nixCache), then nix's
# default upstream as automatic
# fallback: cache down = slower, never broken.
#
# `substituters`, NOT `extra-substituters`: the extra- form APPENDS to
# nix's default (cache.nixos.org), so the list read upstream-first and
# the cache was never chosen — runners pulled ~155 GB/day straight
# from Fastly while the nginx cache sat idle. Naming the
# full list is what puts the cache first. Order alone is not enough
# either: nix ranks substituters by the Priority in each
# nix-cache-info, so the proxy also advertises Priority 10 against
# upstream's 40 (ci-builders chart).
#
# The S3 push cache is retired with this — and with it the
# require-sigs=false it forced: everything now originates upstream,
# whose narinfo signatures pass through the proxy intact and verify
# against nix's default trusted key.
# filter-syscalls off FOR THE BUILD ONLY: the arm64 half runs under
# QEMU user emulation, where nix's seccomp BPF load fails ("Invalid
# argument"); real arm64 hardware at runtime is unaffected and the
# baked nix.conf stays clean.
# Re-declared INSIDE the stage: an ARG before the first FROM is global and
# is NOT in scope in a build stage until named again. Without this line
# ${NIX_SUBSTITUTERS} expands to empty and the image ships
# `substituters = `, which is not a fallback to upstream -- it is NO
# substituter at all, and every job builds the world from source. Same
# pattern as ARG GORELEASER_VERSION after the FROM above. No default here,
# so the value comes from the global ARG or the caller's --build-arg.
ARG NIX_SUBSTITUTERS
ARG NIX_VERSION
RUN NIX_CONFIG="filter-syscalls = false" \
    sh <(curl -fsSL "https://releases.nixos.org/nix/nix-${NIX_VERSION}/install") --no-daemon \
 && mkdir -p /home/runner/.config/nix \
 && printf '%s\n' \
      'experimental-features = nix-command flakes' \
      "substituters = ${NIX_SUBSTITUTERS}" \
      > /home/runner/.config/nix/nix.conf

ENV PATH="/home/runner/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"
