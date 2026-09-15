#!/usr/bin/env bash
set -euo pipefail

runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/run/nix-worker}
ssh_dir=${NIX_WORKER_SSH_DIR:-/etc/nix-worker/ssh}
trusted_user_ca=${NIX_WORKER_TRUSTED_USER_CA_FILE:-/etc/nix-worker/ca/trusted_user_ca_keys}
max_jobs=${NIX_WORKER_MAX_JOBS:-2}
cores=${NIX_WORKER_CORES:-4}
min_free=${NIX_WORKER_MIN_FREE_BYTES:-10737418240}
max_free=${NIX_WORKER_MAX_FREE_BYTES:-21474836480}

nix_daemon=$(<"$runtime_dir/nix-daemon-path")
[[ -x "$nix_daemon" ]]
[[ -s "$ssh_dir/ssh_host_ed25519_key" && -s "$trusted_user_ca" ]]
ssh-keygen -l -f "$trusted_user_ca" >/dev/null

mkdir -p /build /nix/store /nix/var/nix/daemon-socket
chown root:nixbld /build
chmod 0775 /build
chown root:nixbld /nix/store
chmod 1775 /nix/store
rm -f /nix/var/nix/daemon-socket/socket

# Preserve the image's cache-first substituters and add only worker
# daemon policy. The remote account is trusted because Nix requires a
# trusted SSH user for distributed builds; sshd separately restricts it
# to the Nix store protocol.
export NIX_CONFIG="$(cat /home/runner/.config/nix/nix.conf)
 sandbox = true
 build-users-group = nixbld
 allowed-users = root nixremote
 trusted-users = root nixremote
 max-jobs = $max_jobs
 cores = $cores
 build-dir = /build
 auto-optimise-store = true
 min-free = $min_free
 max-free = $max_free"

"$nix_daemon" &
daemon_pid=$!
printf '%s\n' "$daemon_pid" > "$runtime_dir/nix-daemon.pid"

for _ in $(seq 1 120); do
  [[ -S /nix/var/nix/daemon-socket/socket ]] && break
  kill -0 "$daemon_pid"
  sleep 0.5
done
[[ -S /nix/var/nix/daemon-socket/socket ]]

/usr/sbin/sshd -t -f /etc/nix-worker/sshd_config
/usr/sbin/sshd -D -e -f /etc/nix-worker/sshd_config &
sshd_pid=$!

terminate() {
  kill "$sshd_pid" "$daemon_pid" 2>/dev/null || true
  wait "$sshd_pid" "$daemon_pid" 2>/dev/null || true
}
trap terminate TERM INT EXIT

wait -n "$daemon_pid" "$sshd_pid"
exit 1
