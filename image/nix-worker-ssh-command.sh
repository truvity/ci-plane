#!/usr/bin/env bash
set -euo pipefail

runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/run/nix-worker}
nix_store=$(<"$runtime_dir/nix-store-path")
[[ -x "$nix_store" ]]

# The legacy SSH store protocol is intentionally used for the first
# rollout: its command is fixed and can be constrained exactly. No
# shell, forwarding, or arbitrary remote command is available.
case "${SSH_ORIGINAL_COMMAND:-}" in
  "nix-store --serve --write")
    exec env NIX_REMOTE=daemon "$nix_store" --serve --write
    ;;
  "nix-store --serve")
    exec env NIX_REMOTE=daemon "$nix_store" --serve
    ;;
  *)
    echo "nix worker: rejected SSH command" >&2
    exit 126
    ;;
esac
