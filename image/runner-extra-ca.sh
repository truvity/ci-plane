#!/usr/bin/env bash
# Trust extra CA certificates in a runner whose user is not root.
#
# The runner container's entrypoint prefix (arc-runners
# `extraCACertificates`): it runs once at container start, makes the
# certificates in $CI_EXTRA_CA_DIR trusted, then execs the rest of the
# command line, so the runner (or runner-watchdog) is still PID 1 and every
# job step inherits the environment set here.
#
#   runner-extra-ca [--] COMMAND [ARG...]
#
# CI_EXTRA_CA_DIR unset or empty: nothing happens and COMMAND runs as is.
# Otherwise every *.pem and *.crt file in that directory holding a PEM
# certificate is trusted IN ADDITION to the system roots (never instead of
# them: github.com has to keep verifying).
#
# The image's `runner` user cannot write /etc/ssl, and under the Pod
# Security `restricted` profile nothing may run as root, so
# update-ca-certificates is not an option. Instead the script builds, in a
# directory the runner owns, a bundle = system roots + the extras, and
# exports it where each client looks:
#
#   SSL_CERT_FILE, NIX_SSL_CERT_FILE  Go, OpenSSL, .NET (the runner itself), nix
#   CURL_CA_BUNDLE                    curl
#   GIT_SSL_CAINFO                    git (libcurl ignores SSL_CERT_FILE)
#   REQUESTS_CA_BUNDLE                Python requests
#   NODE_EXTRA_CA_CERTS               JavaScript actions: ADDS to Node's
#                                     built-in roots, so it names the
#                                     extras alone
#
# When it does run as root and update-ca-certificates exists, it installs
# the extras into the system store instead, and only NODE_EXTRA_CA_CERTS
# (Node never reads the system store) is exported.
#
# A trust problem is reported on stderr and the container still starts:
# like the caches this exists for, a missing CA makes a job slower or its
# TLS step fail, it does not take the runner fleet down.
#
# Variables (all optional): CI_EXTRA_CA_DIR; CI_EXTRA_CA_OUT, where the
# bundles go (default $HOME/.ci-extra-ca, else /tmp/ci-extra-ca).
set -euo pipefail

log() { printf 'runner-extra-ca: %s\n' "$*" >&2; }

# Prints the system bundle's path, or nothing.
system_bundle() {
  local f
  for f in "${SSL_CERT_FILE:-}" /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt; do
    if [[ -n $f && -s $f ]]; then
      printf '%s\n' "$f"
      return 0
    fi
  done
}

setup() {
  local dir=${CI_EXTRA_CA_DIR:-}
  [[ -n $dir ]] || return 0
  if [[ ! -d $dir ]]; then
    log "CI_EXTRA_CA_DIR=$dir is not a directory; extra CAs NOT trusted"
    return 0
  fi

  # A ConfigMap/Secret volume lists its keys as symlinks into a ..data
  # directory; -f follows them, and the globs skip the dot-entries.
  local -a certs=()
  local f
  shopt -s nullglob
  for f in "$dir"/*.pem "$dir"/*.crt; do
    [[ -f $f ]] || continue
    if ! grep -q -- '-----BEGIN CERTIFICATE-----' "$f"; then
      log "skipping $f: no PEM certificate in it"
      continue
    fi
    certs+=("$f")
  done
  shopt -u nullglob
  if ((${#certs[@]} == 0)); then
    log "no *.pem or *.crt certificate in $dir; extra CAs NOT trusted"
    return 0
  fi

  local out=${CI_EXTRA_CA_OUT:-${HOME:-/tmp}/.ci-extra-ca}
  if [[ -z ${CI_EXTRA_CA_OUT:-} && -z ${HOME:-} ]]; then out=/tmp/ci-extra-ca; fi
  if ! mkdir -p "$out" || [[ ! -w $out ]]; then
    log "$out is not writable; extra CAs NOT trusted"
    return 0
  fi

  # The extras alone. A cat with an explicit newline between files, so a
  # file without a trailing newline cannot fuse two PEM blocks. A
  # NODE_EXTRA_CA_CERTS the caller already set is kept, not replaced.
  local extra="$out/extra-ca.pem"
  {
    if [[ -n ${NODE_EXTRA_CA_CERTS:-} && -s ${NODE_EXTRA_CA_CERTS} ]]; then
      cat -- "$NODE_EXTRA_CA_CERTS"
      echo
    fi
    for f in "${certs[@]}"; do
      cat -- "$f"
      echo
    done
  } > "$extra"
  export NODE_EXTRA_CA_CERTS="$extra"

  if [[ $(id -u) == 0 ]] && command -v update-ca-certificates > /dev/null; then
    local n=0
    for f in "${certs[@]}"; do
      n=$((n + 1))
      install -m 0644 -- "$f" "/usr/local/share/ca-certificates/ci-extra-ca-$n.crt"
    done
    update-ca-certificates > /dev/null
    log "installed ${#certs[@]} certificate(s) into the system trust store"
    return 0
  fi

  local base
  base=$(system_bundle || true)
  if [[ -z $base ]]; then
    # An extras-only SSL_CERT_FILE would make github.com unverifiable.
    log "no system CA bundle found; only NODE_EXTRA_CA_CERTS is set"
    return 0
  fi
  local bundle="$out/ca-bundle.crt"
  # Into a temp name first: SSL_CERT_FILE may name $bundle itself.
  cat -- "$base" "$extra" > "$bundle.tmp"
  mv -f -- "$bundle.tmp" "$bundle"
  export SSL_CERT_FILE="$bundle" NIX_SSL_CERT_FILE="$bundle" CURL_CA_BUNDLE="$bundle" \
    GIT_SSL_CAINFO="$bundle" REQUESTS_CA_BUNDLE="$bundle"
  log "trusting ${#certs[@]} extra certificate(s) from $dir (bundle $bundle)"
}

[[ ${1:-} == -- ]] && shift
if (($# == 0)); then
  echo "usage: runner-extra-ca [--] COMMAND [ARG...]" >&2
  exit 2
fi

setup
exec "$@"
