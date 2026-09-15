#!/usr/bin/env bash
set -euo pipefail

runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/run/nix-worker}
ssh_dir=${NIX_WORKER_SSH_DIR:-/etc/nix-worker/ssh}
trusted_user_ca=${NIX_WORKER_TRUSTED_USER_CA_FILE:-/etc/nix-worker/ca/trusted_user_ca_keys}

[[ -S /nix/var/nix/daemon-socket/socket ]]
kill -0 "$(<"$runtime_dir/nix-daemon.pid")"
kill -0 "$(<"$runtime_dir/sshd.pid")"
[[ -s "$ssh_dir/ssh_host_ed25519_key" && -s "$trusted_user_ca" ]]
ssh-keygen -l -f "$trusted_user_ca" >/dev/null
exec 3<>/dev/tcp/127.0.0.1/2222
