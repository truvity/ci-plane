#!/usr/bin/env bash
set -euo pipefail

runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/run/nix-worker}
ssh_dir=${NIX_WORKER_SSH_DIR:-/etc/nix-worker/ssh}
trusted_user_ca=${NIX_WORKER_TRUSTED_USER_CA_FILE:-/etc/nix-worker/ca/trusted_user_ca_keys}
principals_src_dir=${NIX_WORKER_PRINCIPALS_SRC_DIR:-/etc/nix-worker/principals-src}
max_jobs=${NIX_WORKER_MAX_JOBS:-2}
cores=${NIX_WORKER_CORES:-4}
min_free=${NIX_WORKER_MIN_FREE_BYTES:-10737418240}
max_free=${NIX_WORKER_MAX_FREE_BYTES:-21474836480}

# INF host-certificates phase 1. Both features are ADDITIVE and default
# to their pre-existing behaviour: an install that sets neither renders
# and runs exactly as before.
#
# `nix` is a second SSH login (ssh-ng via `nix-daemon --stdio`), gated by
# AuthorizedPrincipalsFile content the chart renders from
# nixWorkers.accounts.nix.principals -- an empty (the default) or absent
# file means nobody. AllowUsers only needs to gain `nix` when that file
# is non-empty; sshd_config bakes the Match block unconditionally, so
# whether the account can ever be reached is decided entirely here.
#
# The ConfigMap itself is mounted read-only at $principals_src_dir, NOT
# at the path sshd is told to trust. Kubernetes' ConfigMap volume plugin
# (the "atomic writer") makes the mount directory itself 0777 and every
# file in it a symlink through a timestamped `..data` directory -- both
# of which OpenSSH's `StrictModes yes` refuses for an
# AuthorizedPrincipalsFile path, which it checks component by component
# ("bad ownership or modes for directory ...", then "Certificate does
# not contain an authorized principal" once sshd gives up and treats the
# file as unreadable). So sshd is never pointed at the ConfigMap mount:
# the real content is copied, once, into a root-owned directory with
# strict modes underneath $runtime_dir (an emptyDir sshd already trusts
# for its own PidFile), and AuthorizedPrincipalsFile in image/sshd_config
# names that copy instead. `install` resolves the `..data` symlink chain
# itself (it copies the target's bytes, never a link), which is exactly
# the "content, not the link" ownership/mode reset this needs.
install -d -o root -g root -m 0755 "$runtime_dir"
install -d -o root -g root -m 0755 "$runtime_dir/principals"
[[ -e "$principals_src_dir/nix" ]]
install -o root -g root -m 0644 "$principals_src_dir/nix" "$runtime_dir/principals/nix"
nix_principals_file="$runtime_dir/principals/nix"
allow_nix_login=false
[[ -s "$nix_principals_file" ]] && allow_nix_login=true
nix_trusted=${NIX_WORKER_ACCOUNTS_NIX_TRUSTED:-false}

# Host certificates: sign this worker's OWN SSH host key so a client's
# known_hosts can trust one `@cert-authority` line instead of pinning one
# host key Secret per architecture (charts/ci-builders values.nixWorkers.hostCertificate).
host_cert_enabled=${NIX_WORKER_HOST_CERTIFICATE_ENABLED:-false}
host_cert_renew_every=${NIX_WORKER_HOST_CERTIFICATE_RENEW_EVERY:-12h}
host_pubkey_file="$runtime_dir/ssh_host_ed25519_key.pub"
host_cert_file="$runtime_dir/ssh_host_ed25519_key-cert.pub"

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

# trusted-users grants IMPORT WITHOUT SIGNATURE VERIFICATION -- the whole
# point of Nix's sandboxing model -- so `nix` joins it only when the
# operator opted in (nixWorkers.accounts.nix.trusted); `allowed-users`
# (what may talk to the daemon at all) always includes it, same as
# nixremote, since sshd -- not nix-daemon -- is what actually decides
# whether the account is reachable.
trusted_users="root nixremote"
[[ "$nix_trusted" == "true" ]] && trusted_users="$trusted_users nix"

# Preserve the image's cache-first substituters and add only worker
# daemon policy. The remote account is trusted because Nix requires a
# trusted SSH user for distributed builds; sshd separately restricts it
# to the Nix store protocol.
export NIX_CONFIG="$(cat /home/runner/.config/nix/nix.conf)
 sandbox = true
 build-users-group = nixbld
 allowed-users = root nixremote nix
 trusted-users = $trusted_users
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

# The FIRST sign is fail-closed: `set -e` means a failure here (a bad
# OpenBao route, a role that refuses this pod's token, a network partition
# at boot) exits the whole entrypoint before sshd ever starts, rather than
# let sshd come up presenting no certificate at all under a policy that
# assumed one. This is the opposite contract from nix-worker-client, which
# fails OPEN (a runner degrades to local builds); a worker's host identity
# has no "degraded" mode a client can fall back to mid-connection.
if [[ "$host_cert_enabled" == "true" ]]; then
  ssh-keygen -y -f "$ssh_dir/ssh_host_ed25519_key" > "$host_pubkey_file"
  NIX_WORKER_HOST_PUBLIC_KEY_FILE="$host_pubkey_file" \
    NIX_WORKER_HOST_CERT_FILE="$host_cert_file" \
    /usr/local/bin/nix-worker-host-cert
  echo "nix worker: host certificate installed"
fi

# A runtime copy: the baked /etc/nix-worker/sshd_config never changes, so
# an install using neither feature above runs the byte-identical file it
# always did. `sed` targets the exact baked line, not a merge of repeated
# AllowUsers directives -- OpenSSH does not document those as additive,
# and this is the one line that decides whether `nix` can be reached at
# all, so it is worth being exact rather than convenient.
runtime_sshd_config="$runtime_dir/sshd_config"
cp /etc/nix-worker/sshd_config "$runtime_sshd_config"
if [[ "$allow_nix_login" == "true" ]]; then
  sed -i 's/^AllowUsers nixremote$/AllowUsers nixremote nix/' "$runtime_sshd_config"
fi
if [[ "$host_cert_enabled" == "true" ]]; then
  # Inserted right after the `HostKey` line, NOT appended at the end of
  # the file: sshd_config's trailing `Match User nix` block extends to
  # EOF, and a HostCertificate appended there lands INSIDE that Match --
  # which OpenSSH refuses outright ("Directive 'HostCertificate' is not
  # allowed within a Match block"). Verified against this exact file.
  sed -i "/^HostKey /a HostCertificate $host_cert_file" "$runtime_sshd_config"
fi

/usr/sbin/sshd -t -f "$runtime_sshd_config"
/usr/sbin/sshd -D -e -f "$runtime_sshd_config" &
sshd_pid=$!

renew_pid=""
if [[ "$host_cert_enabled" == "true" ]]; then
  # Renewal is fail-OPEN in the sense that matters here: it never tears
  # down a running sshd whose current certificate is still valid. A
  # renewal failure logs and retries with exponential backoff (capped at
  # 15m) instead. `sleep` takes the value's own suffix (s/m/h/d) directly
  # -- no duration parsing needed on the bash side, only on the Go side
  # and the chart's render-time guard (renewEvery must stay under
  # certificateTTL, checked in charts/ci-builders/templates/nix-workers.yaml).
  #
  # SIGHUP, not a restart: OpenSSH's sshd re-executes itself on SIGHUP,
  # which re-reads sshd_config and re-loads HostCertificate from disk
  # without dropping any already-established session (verified against
  # this image -- see the PR description for the evidence).
  (
    backoff=30
    # `next_sleep` is what actually decides the pace: renewEvery after a
    # success, the (growing) backoff after a failure. A single `sleep
    # renewEvery` as the loop condition would wait a FULL renewEvery on
    # top of the backoff after every failure, which is backoff in name
    # only -- a retry that always lands late is not a retry.
    next_sleep="$host_cert_renew_every"
    while sleep "$next_sleep"; do
      if NIX_WORKER_HOST_PUBLIC_KEY_FILE="$host_pubkey_file" \
           NIX_WORKER_HOST_CERT_FILE="$host_cert_file" \
           /usr/local/bin/nix-worker-host-cert; then
        backoff=30
        next_sleep="$host_cert_renew_every"
        kill -HUP "$sshd_pid" 2>/dev/null || true
        echo "nix worker: host certificate renewed"
      else
        echo "nix worker: host certificate renewal failed; retrying in ${backoff}s" >&2
        next_sleep="$backoff"
        backoff=$(( backoff < 900 ? backoff * 2 : 900 ))
      fi
    done
  ) &
  renew_pid=$!
fi

terminate() {
  kill "$sshd_pid" "$daemon_pid" ${renew_pid:+"$renew_pid"} 2>/dev/null || true
  wait "$sshd_pid" "$daemon_pid" ${renew_pid:+"$renew_pid"} 2>/dev/null || true
}
trap terminate TERM INT EXIT

wait -n "$daemon_pid" "$sshd_pid"
exit 1
