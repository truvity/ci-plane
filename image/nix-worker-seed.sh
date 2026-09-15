#!/usr/bin/env bash
set -euo pipefail

# The worker mounts its persistent volume at /nix, which would hide the
# Nix closure baked into the image. The init container instead mounts
# that volume at /seed/nix and imports the current closure through Nix's
# alternate-root store. This updates both store objects and the validity
# database, and is safe to repeat after image upgrades.
target_root=${NIX_WORKER_SEED_ROOT:-/seed}
runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/runtime}
nix_bin=/home/runner/.nix-profile/bin/nix
# These aliases live outside /nix so they survive the worker PVC mount.
# Keep their multicall names: resolving them to the underlying
# `.../bin/nix` binary loses argv[0], so `nix-daemon`/`nix-store`
# would behave as the generic `nix` CLI instead of their legacy modes.
nix_daemon=/usr/local/libexec/nix-worker/nix-daemon
nix_store=/usr/local/libexec/nix-worker/nix-store
nix_real=$(readlink -f "$nix_daemon")
nix_root=${nix_real%/bin/nix}

[[ -x "$nix_bin" && -x "$nix_daemon" && -x "$nix_store" ]]
mkdir -p "$target_root/nix" "$runtime_dir"

export NIX_CONFIG=$'experimental-features = nix-command flakes\nfilter-syscalls = false'
# The source and destination are two local stores in this same trusted
# image. Parts of Nix's own installer closure are locally built and
# therefore unsigned; signature enforcement is for untrusted binary
# caches, not this image-to-PVC bootstrap.
"$nix_bin" copy --no-check-sigs --to "local?root=$target_root" "$nix_root"

[[ -x "$target_root$nix_real" ]]
mkdir -p "$target_root/nix/var/nix/gcroots/ci-plane"
ln -sfn "$nix_root" "$target_root/nix/var/nix/gcroots/ci-plane/worker-runtime"
printf '%s\n' "$nix_daemon" > "$runtime_dir/nix-daemon-path"
printf '%s\n' "$nix_store" > "$runtime_dir/nix-store-path"
