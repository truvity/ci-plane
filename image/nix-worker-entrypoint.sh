#!/usr/bin/env bash
set -euo pipefail

runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/run/nix-worker}
ssh_dir=${NIX_WORKER_SSH_DIR:-/etc/nix-worker/ssh}
trusted_user_ca=${NIX_WORKER_TRUSTED_USER_CA_FILE:-/etc/nix-worker/ca/trusted_user_ca_keys}
principals_src_dir=${NIX_WORKER_PRINCIPALS_SRC_DIR:-/etc/nix-worker/principals-src}
opkssh_src_dir=${NIX_WORKER_OPKSSH_SRC_DIR:-/etc/nix-worker/opkssh-src}
opk_dir=${NIX_WORKER_OPK_DIR:-/etc/opk}
max_jobs=${NIX_WORKER_MAX_JOBS:-2}
cores=${NIX_WORKER_CORES:-4}
min_free=${NIX_WORKER_MIN_FREE_BYTES:-10737418240}
max_free=${NIX_WORKER_MAX_FREE_BYTES:-21474836480}

# `nix` is the machine login (ssh-ng via `nix-daemon --stdio`), gated by
# AuthorizedPrincipalsFile content the chart renders from
# nixWorkers.accounts.nix.principals -- an empty or absent file means
# nobody. AllowUsers only gains `nix` when that file is non-empty;
# sshd_config bakes the Match block unconditionally, so whether the
# account can ever be reached is decided entirely here.
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

# opkssh (the people pilot): OIDC sign-in with no CA, onto the
# SAME `nix` account as the OpenBao-certificate machine login above (the
# shared account the pilot's design calls for) and a SEPARATE `admin`
# shell account no certificate ever reaches. false (the default) leaves
# both untouched by anything below: `admin` never joins AllowUsers, and
# `nix`'s reachability is still decided by the principals file alone,
# exactly as before this feature existed.
#
# true forces allow_nix_login regardless of the principals file's own
# content: an opkssh-only worker (no OpenBao SSH-sign role wired up yet)
# must still be able to reach `nix` -- sshd's AllowUsers, not the
# principals file, is what actually decides whether either credential
# mechanism ever gets a chance to run.
opkssh_enabled=${NIX_WORKER_OPKSSH_ENABLED:-false}
[[ "$opkssh_enabled" == "true" ]] && allow_nix_login=true

# /etc/opk is an emptyDir in the chart (shadowing the image's own
# read-only placeholder directory): readOnlyRootFilesystem: true means
# opkssh's own hardcoded /etc/opk/{providers,auth_id} paths (there is no
# flag to relocate them) can only ever be writable if something backs
# them with a real volume, the same reason $runtime_dir under
# /run/nix-worker exists for the CA-certificate files above. Rebuilt
# from scratch every start, same content-addressed ConfigMap pattern as
# the principals file: enabled false (or the ConfigMap rendering empty
# content) leaves both files present but empty, which is what
# `opkssh verify` reads as "nobody" -- it never fails to find the
# files, it fails every identity check against them.
install -d -o root -g opksshuser -m 0750 "$opk_dir"
if [[ "$opkssh_enabled" == "true" ]]; then
  [[ -e "$opkssh_src_dir/providers" && -e "$opkssh_src_dir/auth_id" ]]
  install -o root -g opksshuser -m 0640 "$opkssh_src_dir/providers" "$opk_dir/providers"
  install -o root -g opksshuser -m 0640 "$opkssh_src_dir/auth_id" "$opk_dir/auth_id"
else
  install -o root -g opksshuser -m 0640 /dev/null "$opk_dir/providers"
  install -o root -g opksshuser -m 0640 /dev/null "$opk_dir/auth_id"
fi

# v5.0.0 removed the legacy `nixremote` login (`nix-store --serve`) and
# its shim from this image. An old chart still rendering the toggle on
# would otherwise start a worker whose only machine login silently no
# longer exists; refuse instead, naming the fix.
if [[ "${NIX_WORKER_ACCOUNTS_NIXREMOTE_ENABLED:-false}" == "true" ]]; then
  echo "nix worker: NIX_WORKER_ACCOUNTS_NIXREMOTE_ENABLED=true, but this image (ci-plane >= 5.0.0) has no nixremote login; use a ci-builders chart of the same version as the image" >&2
  exit 1
fi

# Host certificates: this worker's SSH host identity is certified by an
# OpenBao SSH host CA, so a client's known_hosts trusts one
# `@cert-authority` line instead of pinning a host key
# (charts/ci-builders values.nixWorkers.hostCertificate).
#
# With a certificate, the host KEY is EPHEMERAL: generated here, per pod
# (freshly on every container start), in the runtime emptyDir, and never
# stored anywhere else -- no Kubernetes Secret, no secret manager row.
# Nothing has to trust the key itself, only the CA's signature on it, so
# a key that dies with the pod is strictly less to protect and nothing to
# rotate. Without a certificate (hostCertificate.enabled: false) clients
# can only pin the key, so it has to be stable: the static key the chart
# mounts from nixWorkers.ssh.existingSecret, the one path that still
# reads $ssh_dir.
host_cert_enabled=${NIX_WORKER_HOST_CERTIFICATE_ENABLED:-false}
host_cert_renew_every=${NIX_WORKER_HOST_CERTIFICATE_RENEW_EVERY:-12h}
if [[ "$host_cert_enabled" == "true" ]]; then
  host_key_file="$runtime_dir/ssh_host_ed25519_key"
else
  host_key_file="$ssh_dir/ssh_host_ed25519_key"
fi
host_pubkey_file="$runtime_dir/ssh_host_ed25519_key.pub"
host_cert_file="$runtime_dir/ssh_host_ed25519_key-cert.pub"

nix_daemon=$(<"$runtime_dir/nix-daemon-path")
[[ -x "$nix_daemon" ]]
[[ -s "$trusted_user_ca" ]]
ssh-keygen -l -f "$trusted_user_ca" >/dev/null

if [[ "$host_cert_enabled" == "true" ]]; then
  # Whatever a previous container of this same pod left in the emptyDir
  # (key, public half, certificate) is discarded first: a certificate
  # for a key this process did not generate must never be the one the
  # readiness probe finds.
  rm -f "$host_key_file" "$host_pubkey_file" "$host_cert_file"
  ssh-keygen -q -t ed25519 -N '' -C "nix-worker-${HOSTNAME:-unknown}" -f "$host_key_file"
  chmod 0600 "$host_key_file"
  echo "nix worker: generated an ephemeral host key $(ssh-keygen -l -f "$host_key_file.pub" | awk '{print $2}')"
else
  [[ -s "$host_key_file" ]]
fi

mkdir -p /build /nix/store /nix/var/nix/daemon-socket
chown root:nixbld /build
chmod 0775 /build
chown root:nixbld /nix/store
chmod 1775 /nix/store
rm -f /nix/var/nix/daemon-socket/socket

# trusted-users grants IMPORT WITHOUT SIGNATURE VERIFICATION -- the whole
# point of Nix's sandboxing model -- so `nix` joins it only when the
# operator asked (nixWorkers.accounts.nix.trusted, on by default since
# v5.0.0 because a remote build worker needs it, exactly as the removed
# `nixremote` login always had it); `allowed-users` (what may talk to
# the daemon at all) always includes `nix`, whether or not sshd will ever
# let anyone reach it, since sshd -- not nix-daemon -- is what actually
# decides whether an account is reachable.
trusted_users="root"
allowed_users="root nix"
[[ "$nix_trusted" == "true" ]] && trusted_users="$trusted_users nix"

# Preserve the image's cache-first substituters and add only worker
# daemon policy. sshd separately restricts `nix` to the Nix daemon
# protocol (its ForceCommand).
export NIX_CONFIG="$(cat /home/runner/.config/nix/nix.conf)
 sandbox = true
 build-users-group = nixbld
 allowed-users = $allowed_users
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
  ssh-keygen -y -f "$host_key_file" > "$host_pubkey_file"
  NIX_WORKER_HOST_PUBLIC_KEY_FILE="$host_pubkey_file" \
    NIX_WORKER_HOST_CERT_FILE="$host_cert_file" \
    /usr/local/bin/nix-worker-host-cert
  echo "nix worker: host certificate installed"
fi

# A runtime copy: the baked /etc/nix-worker/sshd_config never changes.
# `sed` targets the exact baked `AllowUsers nix` line, not a merge of
# repeated AllowUsers directives -- OpenSSH does not document those as
# additive, and this is the one line that decides who can be reached at
# all, so it is worth being exact rather than convenient.
#
# `admin` (opkssh, the people pilot) is an independent second account;
# with neither it nor `nix` reachable this worker would admit nobody.
# The chart refuses that shape at render time (an empty
# nixWorkers.accounts.nix.principals); refuse it here too, so this
# script stays correct standalone (hack/nix-worker-principals-e2e.sh
# drives it without the chart).
allow_users=()
[[ "$allow_nix_login" == "true" ]] && allow_users+=(nix)
[[ "$opkssh_enabled" == "true" ]] && allow_users+=(admin)
if (( ${#allow_users[@]} == 0 )); then
  echo "nix worker: no login is configured (the nix principals file is empty and opkssh is off); refusing to start a worker nobody can reach" >&2
  exit 1
fi
runtime_sshd_config="$runtime_dir/sshd_config"
cp /etc/nix-worker/sshd_config "$runtime_sshd_config"
sed -i "s/^AllowUsers nix\$/AllowUsers ${allow_users[*]}/" "$runtime_sshd_config"
grep -qx "AllowUsers ${allow_users[*]}" "$runtime_sshd_config"
# HostKey: the baked line names the static Secret mount; with a
# certificate it is the ephemeral key generated above instead.
sed -i "s|^HostKey .*\$|HostKey $host_key_file|" "$runtime_sshd_config"
grep -qx "HostKey $host_key_file" "$runtime_sshd_config"
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
