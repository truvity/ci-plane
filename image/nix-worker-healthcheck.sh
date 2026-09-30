#!/usr/bin/env bash
set -euo pipefail

runtime_dir=${NIX_WORKER_RUNTIME_DIR:-/run/nix-worker}
ssh_dir=${NIX_WORKER_SSH_DIR:-/etc/nix-worker/ssh}
trusted_user_ca=${NIX_WORKER_TRUSTED_USER_CA_FILE:-/etc/nix-worker/ca/trusted_user_ca_keys}

# Host-certificate freshness (host certificates, phase 1): READINESS
# only, never startup or liveness. A worker whose renewal loop
# (nix-worker-entrypoint.sh) has stalled still serves every session
# opened under its still-valid certificate -- killing the pod would drop
# those for no reason, so only the readinessProbe's command line carries
# the "--readiness" argument below (charts/ci-builders/templates/
# nix-workers.yaml); the identical startupProbe/livenessProbe omit it
# and this whole check never runs for them.
#
# A function, not straight-line code like the checks in main() below: it
# is the one part of this script worth testing without a running
# nix-daemon/sshd/port 2222, so hack/nix-worker-healthcheck-cases.sh
# sources this file (which, thanks to the BASH_SOURCE guard at the
# bottom, does nothing on `source` beyond defining functions) and calls
# it directly with a hand-built certificate file.
#
# Every failure prints WHY to stderr: Kubernetes records an exec probe's
# own stdout+stderr on the readiness-failure event it reports, and with
# the 12h renewEvery / 24h certificateTTL defaults, a bare "not ready"
# would not be a lead for a renewal loop that can already have been
# silently failing for close to 10h.
#
# The optional third argument is the public key the certificate must
# certify -- the worker's ephemeral host key (v5.0.0), generated per pod
# by nix-worker-entrypoint.sh. A valid, fresh certificate for some OTHER
# key (a leftover of an earlier container, a signing mix-up) is exactly
# as useless to a client as no certificate at all: sshd would present a
# certificate that does not match the key it proves possession of.
check_host_certificate() {
  local cert_file=$1 min_remaining=$2 pubkey_file=${3:-}
  local info valid_to valid_to_epoch now_epoch remaining min_remaining_seconds
  local cert_fp key_fp

  if [[ ! -s "$cert_file" ]]; then
    echo "not ready: host certificate $cert_file is missing" >&2
    return 1
  fi

  if ! info=$(ssh-keygen -L -f "$cert_file" 2>&1); then
    echo "not ready: host certificate is not parseable: $info" >&2
    return 1
  fi

  # ssh-keygen -L prints e.g.
  #   Valid: from 2026-09-28T00:00:00 to 2026-09-29T00:00:00
  # in the image's OWN local time, which is UTC: this image installs no
  # tzdata (image/nix-worker/Dockerfile), so glibc's default "Etc/UTC"
  # applies. `date -u` below both interprets that string as UTC and
  # returns a UTC epoch, so no separate timezone conversion is needed.
  valid_to=$(sed -n 's/^[[:space:]]*Valid: from [^[:space:]]* to \([^[:space:]]*\).*$/\1/p' <<<"$info" | head -n1)
  if [[ -z "$valid_to" ]]; then
    echo "not ready: host certificate is not parseable: no 'Valid: from ... to ...' line in ssh-keygen -L output" >&2
    return 1
  fi

  if [[ -n "$pubkey_file" ]]; then
    # ssh-keygen -L prints the certified key as
    #   Public key: ED25519-CERT SHA256:<fingerprint>
    # and -l on the plain public key prints "<bits> SHA256:<fp> ...".
    cert_fp=$(sed -n 's/^[[:space:]]*Public key: [^[:space:]]* \([^[:space:]]*\).*$/\1/p' <<<"$info" | head -n1)
    if ! key_fp=$(ssh-keygen -l -f "$pubkey_file" 2>/dev/null | awk '{print $2}') || [[ -z "$key_fp" ]]; then
      echo "not ready: host public key $pubkey_file is missing or unreadable" >&2
      return 1
    fi
    if [[ -z "$cert_fp" || "$cert_fp" != "$key_fp" ]]; then
      echo "not ready: host certificate certifies ${cert_fp:-no key}, not this worker's host key $key_fp" >&2
      return 1
    fi
  fi

  if ! valid_to_epoch=$(date -u -d "$valid_to" +%s 2>/dev/null); then
    echo "not ready: host certificate is not parseable: unparseable expiry timestamp '$valid_to'" >&2
    return 1
  fi

  now_epoch=$(date -u +%s)
  remaining=$(( valid_to_epoch - now_epoch ))
  if (( remaining <= 0 )); then
    echo "not ready: host certificate expired at ${valid_to}Z" >&2
    return 1
  fi

  case "$min_remaining" in
    *s) min_remaining_seconds=${min_remaining%s} ;;
    *m) min_remaining_seconds=$(( ${min_remaining%m} * 60 )) ;;
    *h) min_remaining_seconds=$(( ${min_remaining%h} * 3600 )) ;;
    *)
      echo "not ready: NIX_WORKER_HOST_CERTIFICATE_MIN_REMAINING '$min_remaining' must be a whole number of s, m or h" >&2
      return 1
      ;;
  esac

  if (( remaining < min_remaining_seconds )); then
    echo "not ready: host certificate has ${remaining}s of validity left, below the ${min_remaining} minimum (expires ${valid_to}Z)" >&2
    return 1
  fi

  return 0
}

# Whether this invocation should even look at the host certificate at
# all: only the readiness probe's own "--readiness" argument, and only
# when the feature is on -- disabled (the static-key path), this always
# returns false and every probe behaves exactly as it did before this
# check existed.
host_certificate_check_applies() {
  [[ "${1:-}" == "--readiness" && "${NIX_WORKER_HOST_CERTIFICATE_ENABLED:-false}" == "true" ]]
}

# The host private key sshd was started with: the ephemeral per-pod key
# in the runtime emptyDir when host certificates are on (v5.0.0, the
# chart default), the static Secret mount otherwise. Mirrors
# nix-worker-entrypoint.sh, which is what decides.
host_key_path() {
  if [[ "${NIX_WORKER_HOST_CERTIFICATE_ENABLED:-false}" == "true" ]]; then
    printf '%s\n' "$runtime_dir/ssh_host_ed25519_key"
  else
    printf '%s\n' "$ssh_dir/ssh_host_ed25519_key"
  fi
}

main() {
  [[ -S /nix/var/nix/daemon-socket/socket ]]
  kill -0 "$(<"$runtime_dir/nix-daemon.pid")"
  kill -0 "$(<"$runtime_dir/sshd.pid")"
  [[ -s "$(host_key_path)" && -s "$trusted_user_ca" ]]
  ssh-keygen -l -f "$trusted_user_ca" >/dev/null
  exec 3<>/dev/tcp/127.0.0.1/2222

  if host_certificate_check_applies "${1:-}"; then
    check_host_certificate "$runtime_dir/ssh_host_ed25519_key-cert.pub" "${NIX_WORKER_HOST_CERTIFICATE_MIN_REMAINING:-2h}" "$runtime_dir/ssh_host_ed25519_key.pub"
  fi
}

# Executed directly (every real probe) runs main; sourced (the test
# harness above) only defines the functions, so it can drive
# check_host_certificate/host_certificate_check_applies without a real
# nix-daemon, sshd or port 2222 behind them.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
