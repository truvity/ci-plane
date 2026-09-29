# The Truvity Nix remote-worker image.
#
# SPLIT OUT OF THE RUNNER IMAGE (2026-09-24). Until now one manifest
# served both roles: the runner image carried sshd, an /etc/nix-worker
# directory, 32 nixbld accounts and a `nixremote` login, all inert in an
# ordinary job pod, and Kubernetes chose the role by overriding the
# command. That was a deliberate arrangement and it worked -- what it
# could not do is let either half be tuned for what it actually is. A
# runner wants a toolchain; a worker wants a store and an sshd, and
# carries a runner's docker client, buildx, devbox, goreleaser and two
# cache clients for nothing.
#
# This image is the worker half, alone: nix, sshd, and the four scripts
# the chart's containers and probes invoke. It is NOT built from the
# actions-runner base, because none of that base is a worker's business.
#
# THE ONE THING THE SPLIT PUTS AT RISK, and how it is held:
# remote building is a protocol between two nix installations. While
# both halves came out of one `RUN` line they matched by construction,
# but nothing PINNED them -- the installer was fetched from
# nixos.org/nix/install, whatever that served that day. Two images built
# at different times would drift silently, and a drift shows up as a
# confusing build failure rather than as a version error. So both
# Dockerfiles now pin the SAME NIX_VERSION, and hack/nix-parity.sh
# refuses a release where the two disagree. The pin is the thing that
# makes the split safe; it is not an optimisation.

# renovate: datasource=github-releases depName=NixOS/nix
ARG NIX_VERSION=2.35.2

# opkssh (the people pilot): OIDC sign-in with no CA, additive to
# the OpenBao-signed certificate logins above (AuthorizedKeysCommand and
# TrustedUserCAKeys/AuthorizedPrincipalsFile are independent sshd
# mechanisms -- sshd tries each configured one in turn for pubkey auth,
# exactly as truvity/tailscale's awsrouter package already relies on for
# the EC2 subnet routers' own opkssh rollout). Hand-maintained, not
# renovate-tracked: a version bump here needs BOTH sha256 digests below
# updated in the same commit (curl -fsSL
# https://github.com/openpubkey/opkssh/releases/download/v<X>/checksums.txt),
# and an automated PR that bumped only the version would fail the build
# on the very next line rather than silently drift -- an acceptable
# trade against the risk of a half-updated pin.
#
# The OWN steps below (never opkssh's upstream scripts/install-linux.sh)
# for the same reason truvity/tailscale v1.11.0 stopped calling it: that
# script's determine_linux_type() does not recognize every base image
# family, and owning the four steps it would have taken (create the
# service account, install the verified binary, write the sshd
# AuthorizedKeysCommand drop-in) is small enough that depending on an
# upstream shell script buys nothing here either.
ARG OPKSSH_VERSION=0.16.0

# The in-cluster read-through cache of cache.nixos.org first, then nix's
# default upstream as automatic fallback: cache down = slower, never
# broken. `substituters`, NOT `extra-substituters` -- the extra- form
# APPENDS to nix's default, so the list reads upstream-first and the
# cache is never chosen. arc-runners' `nixCache.url` (v3.1.0) overrides
# this without a rebuild, same as image/runner/Dockerfile.
ARG NIX_SUBSTITUTERS="http://nix-cache.ci-cache.svc.cluster.local https://cache.nixos.org/"

# nix-worker-host-cert (host certificates, phase 1): signs the worker's
# OWN SSH host key with an OpenBao SSH secrets engine, so a client's
# known_hosts can trust one `@cert-authority` line instead of pinning one
# host key per architecture. A SIBLING binary to image/nix-worker-client
# (which signs an ephemeral USER key for runner pods), not a mode flag on
# it -- see that binary's own top-of-file comment for why: the two have
# unrelated validation policies (host vs. user certificates), unrelated
# failure contracts (fail-closed on the first sign here, fail-open on
# renewal; fail-open always there) and unrelated deployment targets. Built
# the same way: static stdlib-plus-x/crypto/ssh, cross-compiled on the
# build platform, no third-party HTTP/TLS/JSON.
# renovate: datasource=docker depName=golang
FROM --platform=$BUILDPLATFORM golang:1.27 AS hostcert
ARG TARGETARCH
COPY nix-worker-host-cert/ /src/nix-worker-host-cert/
RUN cd /src/nix-worker-host-cert \
 && go mod download \
 && CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' \
      -o /out/nix-worker-host-cert .

FROM ubuntu:24.04

ARG NIX_VERSION
ARG NIX_SUBSTITUTERS
ARG OPKSSH_VERSION
# Set by buildx for the platform this stage is building FOR (not the
# builder's own platform -- this stage has no --platform=$BUILDPLATFORM,
# unlike the hostcert stage above, because opkssh ships prebuilt
# per-arch binaries, nothing to cross-compile).
ARG TARGETARCH

# tini is the chart's entrypoint (PID 1 reaps the daemon and sshd);
# openssh-server IS the worker's job; openssh-client is ssh-keygen,
# which the health check runs against the trusted CA file; xz-utils and
# curl are the nix installer's; bash is what every script asks for by
# shebang and what the health check's /dev/tcp probe needs; sudo is the
# `admin` account's whole reason to exist (the people pilot).
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      bash ca-certificates curl openssh-server openssh-client sudo tini xz-utils \
 && rm -rf /var/lib/apt/lists/*

# `runner` is not a leftover of the old base: the worker scripts resolve
# nix through /home/runner/.nix-profile and the chart runs the containers
# as uid 0, so the profile's OWNER is what has to match, not the process.
# Renaming it is a change to nix-worker-seed and the entrypoint, not to
# this file alone.
#
# /nix is created HERE, by root, because the single-user installer
# otherwise tries `sudo mkdir` and this image has no sudo -- the
# actions-runner base it used to inherit did. Installing sudo to satisfy
# an installer would be the wrong trade in an image whose whole point is
# carrying less.
#
# 32 nixbld accounts are what a multi-user nix daemon builds as.
# `nixremote` is the SSH login the runners present a CA-signed
# certificate for; it has no password and sshd restricts what it may run.
#
# `nix` is a SECOND login (host certificates, phase 1): the modern
# ssh-ng protocol via `nix-daemon --stdio`, alongside `nixremote`'s legacy
# `nix-store --serve`. It needs a real shell, not nologin: sshd's
# ForceCommand execs the forced command through the account's shell
# (`$SHELL -c command`), and nologin ignores that argument and just
# refuses the session -- the same reason nixremote is bash, not nologin.
# Nobody can actually reach it yet: sshd_config's `Match User nix` block
# gates on AuthorizedPrincipalsFile /run/nix-worker/principals/%u (a copy
# the entrypoint makes at startup out of the ConfigMap mounted at
# /etc/nix-worker/principals-src -- StrictModes refuses that mount
# directly), and the chart renders that content EMPTY unless
# nixWorkers.accounts.nix.principals
# is set (values.yaml).
RUN groupadd --system nixbld \
 && for i in $(seq 1 32); do \
      useradd --system --gid nixbld --home-dir /var/empty \
        --shell /usr/sbin/nologin "nixbld${i}"; \
    done \
 && nixbld_members=$(for i in $(seq 1 32); do printf 'nixbld%s,' "$i"; done) \
 && gpasswd -M "${nixbld_members%,}" nixbld \
 && useradd --system --create-home --home-dir /home/nixremote \
      --shell /bin/bash nixremote \
 && passwd -d nixremote \
 && useradd --system --create-home --home-dir /home/nix \
      --shell /bin/bash nix \
 && passwd -d nix \
 && useradd --create-home --home-dir /home/runner --shell /bin/bash runner \
 && mkdir -p /etc/nix-worker /etc/nix-worker/principals-src /etc/nix-worker/opkssh-src /run/sshd \
 && chmod 0755 /run/sshd \
 && mkdir -m 0755 /nix \
 && chown runner:runner /nix

# `admin` (the people pilot): the ONE account on this worker that
# gets an interactive shell and root, both by design -- opkssh only, no
# OpenBao certificate reaches it (image/sshd_config's `Match User admin`
# block: `ForceCommand none`, `PermitTTY yes`). No password either (same
# `passwd -d` as every other account here): PasswordAuthentication and
# KbdInteractiveAuthentication are both `no`, so a password could never
# be entered anyway, and sudo below is passwordless for the same reason.
#
# `opksshuser` (upstream opkssh's own convention, the same account name
# truvity/tailscale's awsrouter package uses for the EC2 subnet
# routers): a system account with no shell, whose only job is to be the
# uid sshd's AuthorizedKeysCommand execs `opkssh verify` as -- it never
# logs in itself.
RUN useradd --create-home --home-dir /home/admin --shell /bin/bash admin \
 && passwd -d admin \
 && install -d -m 0750 /home/admin/.ssh \
 && chown admin:admin /home/admin/.ssh \
 && printf 'admin ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/admin \
 && chmod 0440 /etc/sudoers.d/admin \
 && visudo -cf /etc/sudoers.d/admin \
 && useradd --system --no-create-home --shell /usr/sbin/nologin opksshuser

COPY --chmod=0755 nix-worker-seed.sh /usr/local/bin/nix-worker-seed
COPY --chmod=0755 nix-worker-entrypoint.sh /usr/local/bin/nix-worker-entrypoint
COPY --chmod=0755 nix-worker-ssh-command.sh /usr/local/bin/nix-worker-ssh-command
COPY --chmod=0755 nix-worker-healthcheck.sh /usr/local/bin/nix-worker-healthcheck
COPY --chmod=0644 sshd_config /etc/nix-worker/sshd_config
COPY --from=hostcert --chmod=0755 /out/nix-worker-host-cert /usr/local/bin/nix-worker-host-cert

USER runner
# Docker's USER sets the uid, not the environment -- nix's profile
# install resolves the profile directory from $USER/$HOME and dies with
# "unable to install Nix into your default profile" without them.
ENV USER=runner \
    HOME=/home/runner

# Single-user nix owned by `runner` (no daemon at BUILD time -- a
# container has no systemd, and the installer's --no-daemon mode is the
# proven in-image shape). The worker runs nix-daemon itself at runtime,
# from the multicall alias below.
#
# PINNED, unlike the unversioned nixos.org/nix/install the shared image
# used: see the note at the top. filter-syscalls off FOR THE BUILD ONLY,
# because the arm64 half builds under QEMU user emulation where nix's
# seccomp BPF load fails with "Invalid argument"; real arm64 hardware at
# runtime is unaffected and the baked nix.conf stays clean.
# Downloaded to a file rather than `sh <(curl ...)`: process
# substitution is bash, and /bin/sh here is dash. The runner image gets
# away with the <() form only because it symlinks sh to bash on its
# first layer -- a dependency this image deliberately does not inherit.
RUN NIX_CONFIG="filter-syscalls = false" \
    curl -fsSL -o /tmp/nix-install "https://releases.nixos.org/nix/nix-${NIX_VERSION}/install" \
 && sh /tmp/nix-install --no-daemon \
 && rm -f /tmp/nix-install \
 && mkdir -p /home/runner/.config/nix \
 && printf '%s\n' \
      'experimental-features = nix-command flakes' \
      "substituters = ${NIX_SUBSTITUTERS}" \
      > /home/runner/.config/nix/nix.conf \
 && /home/runner/.nix-profile/bin/nix --version

# The worker mounts its PVC at /nix, hiding the profile links the
# installer made. These aliases live OUTSIDE /nix so they survive that
# mount; their targets are in the closure nix-worker-seed copies onto the
# PVC. Keep the multicall names -- resolving them to the underlying
# `.../bin/nix` loses argv[0], and nix-daemon/nix-store would then behave
# as the generic CLI instead of their legacy modes.
USER root
RUN mkdir -p /usr/local/libexec/nix-worker \
 && ln -s "$(readlink -f /home/runner/.nix-profile/bin/nix-daemon)" \
      /usr/local/libexec/nix-worker/nix-daemon \
 && ln -s "$(readlink -f /home/runner/.nix-profile/bin/nix-store)" \
      /usr/local/libexec/nix-worker/nix-store

# opkssh's own prebuilt per-arch binaries (linux-amd64, linux-arm64;
# checksums from that release's own checksums.txt, verified above). No
# `arm64|amd64` case for anything else in this Dockerfile -- these are
# the only two TARGETARCH values ci-plane releases
# (charts/ci-builders default nixWorkers.archs), and an unrecognized one
# fails the build rather than installing an unverified binary.
#
# /usr/local/bin/opkssh itself does not need to be writable at runtime
# (readOnlyRootFilesystem: true in the chart is fine for a baked
# binary); /etc/opk does -- see image/nix-worker-entrypoint.sh and the
# chart's own `opk` emptyDir mount, which shadows the placeholder
# directory below at container start.
RUN case "$TARGETARCH" in \
      amd64) opkssh_sha256=c018c3e7baf98612b923e742dd87be38650bf61e3b755fb2bc90de177568b1bf ;; \
      arm64) opkssh_sha256=9dd10c2b6ce99cde18e52c054877ca014134b291fd82afe71741c68db4f83d44 ;; \
      *) echo "opkssh: unsupported TARGETARCH $TARGETARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/opkssh \
      "https://github.com/openpubkey/opkssh/releases/download/v${OPKSSH_VERSION}/opkssh-linux-${TARGETARCH}" \
 && echo "${opkssh_sha256}  /tmp/opkssh" | sha256sum -c - \
 && install -m 0755 -o root -g opksshuser /tmp/opkssh /usr/local/bin/opkssh \
 && rm -f /tmp/opkssh \
 && mkdir -m 0750 /etc/opk \
 && chown root:opksshuser /etc/opk

# The chart runs both containers as uid 0 (the daemon needs it) and
# names the command itself, so no ENTRYPOINT/CMD here: an image that
# guesses its own role is the thing this split removes.
ENV PATH="/home/runner/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"
