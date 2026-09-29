#!/usr/bin/env bash
# Host-certificate freshness in the nix-worker readiness probe (host
# certificates, phase 1), asserted directly against
# image/nix-worker-healthcheck.sh -- no cluster, no running nix-daemon or
# sshd needed, because that file's own checks live in functions the
# script only calls from main() (see its BASH_SOURCE guard); sourcing it
# here just defines them.
#
# What has to hold: fresh cert -> ready; a cert with less than
# minRemaining left -> not ready, naming the cause; missing -> not
# ready; not parseable -> not ready; expired -> not ready; and the whole
# check is a no-op unless BOTH the readiness probe's own "--readiness"
# argument and NIX_WORKER_HOST_CERTIFICATE_ENABLED=true are present --
# proving host certificates disabled (the chart default) leaves every
# probe unaffected.
set -uo pipefail

# The parser (image/nix-worker-healthcheck.sh) assumes ssh-keygen's own
# "Valid: from ... to ..." dates are already UTC -- true in the built
# image (no tzdata is installed there) but not necessarily true on
# whatever machine runs this test. Pin TZ so ssh-keygen -V/-L and this
# script's own `date` agree with that assumption regardless.
export TZ=UTC

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fail=0

command -v ssh-keygen >/dev/null || { echo "ssh-keygen is required" >&2; exit 2; }

# shellcheck source=/dev/null
source "$here/image/nix-worker-healthcheck.sh"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

ca_key="$work/ca_key"
host_key="$work/host_key"
ssh-keygen -q -t ed25519 -N '' -f "$ca_key"
ssh-keygen -q -t ed25519 -N '' -f "$host_key"

# make_cert VALIDITY -> writes $work/cert-<label>.pub, a host certificate
# for $host_key signed by $ca_key with ssh-keygen's own -V window
# (e.g. "-10m:+2h", "-2d:-1d" for an already-expired one).
make_cert() {
  local out="$1" validity="$2"
  cp "$host_key.pub" "$work/tmp.pub"
  ssh-keygen -q -s "$ca_key" -I test-host -h -n worker.example -V "$validity" "$work/tmp.pub" >/dev/null
  mv "$work/tmp-cert.pub" "$out"
}

check() {
  local desc="$1" expect="$2"; shift 2
  local out rc
  # if/then, not a bare assignment: image/nix-worker-healthcheck.sh
  # itself sets -e, which `source` applies to THIS shell too, so a bare
  # `out=$(check_host_certificate ...)` would abort this whole script
  # the moment a case under test is expected to fail.
  if out=$(check_host_certificate "$@" 2>&1); then rc=0; else rc=$?; fi
  if [ "$expect" = pass ]; then
    if [ "$rc" -ne 0 ]; then
      echo "FAIL  $desc: expected pass, got exit $rc: $out"; fail=1
    else
      echo "ok    $desc"
    fi
  else
    # expect is the substring the failure message must contain.
    if [ "$rc" -eq 0 ]; then
      echo "FAIL  $desc: expected a readiness failure, it passed"; fail=1
    elif ! grep -qF -- "$expect" <<<"$out"; then
      echo "FAIL  $desc: failure message did not name the cause -- got: $out"; fail=1
    else
      echo "ok    $desc"
    fi
  fi
}

make_cert "$work/cert-fresh.pub" "-5m:+23h"
check "fresh certificate (23h left, 2h minimum) is ready" pass \
  "$work/cert-fresh.pub" "2h"

make_cert "$work/cert-low.pub" "-59m:+1h"
check "certificate with ~1h left, 2h minimum, is not ready" "below the 2h minimum" \
  "$work/cert-low.pub" "2h"

make_cert "$work/cert-expired.pub" "-2h:-1h"
check "expired certificate is not ready" "expired" \
  "$work/cert-expired.pub" "2h"

check "missing certificate file is not ready" "is missing" \
  "$work/no-such-cert.pub" "2h"

echo garbage > "$work/cert-garbage.pub"
check "unparseable certificate file is not ready" "not parseable" \
  "$work/cert-garbage.pub" "2h"

# host_certificate_check_applies: the gate that keeps every probe
# unaffected unless BOTH the readiness flag and the feature are on.
assert_applies() {
  local desc="$1" expect="$2"; shift 2
  if host_certificate_check_applies "$@"; then
    if [ "$expect" = yes ]; then echo "ok    $desc"; else echo "FAIL  $desc: expected the check to be skipped, it applied"; fail=1; fi
  else
    if [ "$expect" = no ]; then echo "ok    $desc"; else echo "FAIL  $desc: expected the check to apply, it was skipped"; fail=1; fi
  fi
}

NIX_WORKER_HOST_CERTIFICATE_ENABLED=true assert_applies \
  "--readiness with host certificates enabled: check applies" yes --readiness
NIX_WORKER_HOST_CERTIFICATE_ENABLED=false assert_applies \
  "--readiness with host certificates disabled (chart default): unaffected" no --readiness
NIX_WORKER_HOST_CERTIFICATE_ENABLED=true assert_applies \
  "liveness/startup probe (no --readiness argument): unaffected even with the feature on" no
unset NIX_WORKER_HOST_CERTIFICATE_ENABLED
assert_applies \
  "--readiness with NIX_WORKER_HOST_CERTIFICATE_ENABLED unset: unaffected (defaults to disabled)" no --readiness

if [ "$fail" = 0 ]; then
  echo "nix-worker healthcheck host-certificate cases OK"
else
  echo "::error::nix-worker healthcheck host-certificate cases FAILED" >&2
  exit 1
fi
