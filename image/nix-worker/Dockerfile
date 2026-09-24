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

# The in-cluster read-through cache of cache.nixos.org first, then nix's
# default upstream as automatic fallback: cache down = slower, never
# broken. `substituters`, NOT `extra-substituters` -- the extra- form
# APPENDS to nix's default, so the list reads upstream-first and the
# cache is never chosen.
ARG NIX_SUBSTITUTERS="http://nix-cache.ci-cache.svc.cluster.local https://cache.nixos.org/"

FROM ubuntu:24.04

ARG NIX_VERSION
ARG NIX_SUBSTITUTERS

# tini is the chart's entrypoint (PID 1 reaps the daemon and sshd);
# openssh-server IS the worker's job; openssh-client is ssh-keygen,
# which the health check runs against the trusted CA file; xz-utils and
# curl are the nix installer's; bash is what every script asks for by
# shebang and what the health check's /dev/tcp probe needs.
RUN apt-get update -qq \
 && apt-get install -y -qq --no-install-recommends \
      bash ca-certificates curl openssh-server openssh-client tini xz-utils \
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
 && useradd --create-home --home-dir /home/runner --shell /bin/bash runner \
 && mkdir -p /etc/nix-worker /run/sshd \
 && chmod 0755 /run/sshd \
 && mkdir -m 0755 /nix \
 && chown runner:runner /nix

COPY --chmod=0755 nix-worker-seed.sh /usr/local/bin/nix-worker-seed
COPY --chmod=0755 nix-worker-entrypoint.sh /usr/local/bin/nix-worker-entrypoint
COPY --chmod=0755 nix-worker-ssh-command.sh /usr/local/bin/nix-worker-ssh-command
COPY --chmod=0755 nix-worker-healthcheck.sh /usr/local/bin/nix-worker-healthcheck
COPY --chmod=0644 sshd_config /etc/nix-worker/sshd_config

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

# The chart runs both containers as uid 0 (the daemon needs it) and
# names the command itself, so no ENTRYPOINT/CMD here: an image that
# guesses its own role is the thing this split removes.
ENV PATH="/home/runner/.nix-profile/bin:/nix/var/nix/profiles/default/bin:${PATH}"
