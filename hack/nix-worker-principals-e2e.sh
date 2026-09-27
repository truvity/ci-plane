#!/usr/bin/env bash
# Regression test for the nix-worker `nix` login and StrictModes.
#
# The bug (seen live): with nixWorkers.accounts.nix.principals set, every
# login as `nix` failed --
#
#   Authentication refused: bad ownership or modes for directory /etc/nix-worker/principals
#   Certificate does not contain an authorized principal
#   Failed publickey for nix from <ip> ... ED25519-CERT ...
#
# -- because AuthorizedPrincipalsFile pointed straight at the ConfigMap's
# own mount. Kubernetes' ConfigMap volume plugin (the "atomic writer")
# makes that mount directory 0777 and every file in it a symlink through
# a timestamped `..data` directory, and OpenSSH's `StrictModes yes`
# checks every component of an AuthorizedPrincipalsFile path for exactly
# that. A unit test of the entrypoint script alone cannot see this: it
# only shows up once something actually mounts a ConfigMap the way
# kubelet does, which the earlier local test missed by placing the file
# with `install -o root -g root` instead.
#
# This script reproduces that MOUNT SHAPE with plain directories and
# symlinks (no cluster needed) against the real, built nix-worker image,
# then drives an actual SSH login with a certificate signed by a local
# test CA. Two containers on one docker network -- a "server" running
# the image's own entrypoint, and a "client" run from the SAME image (it
# already carries `nix` and `ssh`) -- so nothing beyond docker is
# required on the host running this script, hosted GitHub runners
# included.
#
# Two scenarios, every run:
#   populated  -- a cert for principal `ci-nix` must be accepted, and the
#                 daemon must answer over ssh-ng (`nix store ping`
#                 reports Trusted: 1, the same proof used live).
#   empty      -- nixWorkers.accounts.nix.principals unset (the default)
#                 must still leave `nix` unreachable -- AllowUsers is
#                 decided once at container startup and this must not
#                 regress just because the principals path moved.
#
# A THIRD, opt-in scenario proves this test actually catches the bug:
# with PROVE_CATCHES_BUG=1, it also builds the image from BASELINE_REF
# (default origin/master, i.e. the code before this fix) and asserts the
# "populated" scenario FAILS against it, with the exact log lines from
# the report above. This is not run by default -- it builds a second
# full image (another Nix install + store closure copy, minutes) to
# prove a property of the test itself that does not change from run to
# run, not the code under test, so CI runs only the fast half on every
# push; the slow half is here for anyone who wants to see the regression
# test actually regress.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
net="nix-worker-principals-e2e-$$"
fail=0

command -v docker >/dev/null || { echo "docker is required" >&2; exit 2; }

work=$(mktemp -d)
cleanup() {
  docker rm -f "$net-server" "$net-server-baseline" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "== building the nix-worker image under test =="
docker buildx build --load --tag "$net:head" --file "$here/image/nix-worker/Dockerfile" "$here/image/" || {
  echo "::error::failed to build image/nix-worker/Dockerfile" >&2; exit 1; }

# One CA, one host key, one client cert -- shared by every scenario below.
ssh-keygen -q -t ed25519 -N '' -f "$work/ca_key" -C test-ca
ssh-keygen -q -t ed25519 -N '' -f "$work/host_key" -C worker-host
ssh-keygen -q -t ed25519 -N '' -f "$work/client_key" -C test-client
ssh-keygen -s "$work/ca_key" -I ci-nix-test-cert -n ci-nix -V always:forever "$work/client_key.pub" >/dev/null

mkdir -p "$work/ssh" "$work/ca"
cp "$work/host_key" "$work/ssh/ssh_host_ed25519_key"
cp "$work/ca_key.pub" "$work/ca/trusted_user_ca_keys"

# The ConfigMap volume shape kubelet actually produces (the "atomic
# writer"): a timestamped real directory, a `..data` symlink to it, the
# named file a symlink THROUGH `..data`, and the mount directory itself
# 0777. `$1` is the target dir, `$2` the (possibly empty) file content.
make_principals_mount() {
  local dir="$1" content="$2" ts="..2024_01_01_00_00_00.000000000"
  rm -rf "$dir"
  mkdir -p "$dir/$ts"
  printf '%s' "$content" > "$dir/$ts/nix"
  ln -s "$ts" "$dir/..data"
  ln -s "..data/nix" "$dir/nix"
  chmod 0777 "$dir"
}

docker network create "$net" >/dev/null

# Runs the image under test as the chart does: the ConfigMap mount at
# principals-src, entrypoint copying it out before sshd starts. `nix
# store ping` (via a second, short-lived container on the same network)
# is the exact proof used live: Trusted: 1 means auth succeeded AND the
# forced command (`nix-daemon --stdio`) answered the wire protocol.
run_scenario() {
  local image="$1" name="$2" principals_dir="$3" expect="$4" # expect: pass|fail
  docker rm -f "$name" >/dev/null 2>&1 || true
  docker run -d --name "$name" --network "$net" --privileged \
    -e NIX_WORKER_RUNTIME_DIR=/run/nix-worker \
    -e NIX_WORKER_ACCOUNTS_NIX_TRUSTED=true \
    -v "$work/ssh:/etc/nix-worker/ssh:ro" \
    -v "$work/ca:/etc/nix-worker/ca:ro" \
    -v "$principals_dir:/etc/nix-worker/principals-src:ro" \
    --entrypoint /bin/bash \
    "$image" \
    -c 'NIX_WORKER_SEED_ROOT=/seed-scratch nix-worker-seed && exec nix-worker-entrypoint' >/dev/null

  local up=0
  for _ in $(seq 1 90); do
    if docker logs "$name" 2>&1 | grep -q "Server listening"; then up=1; break; fi
    if ! docker ps --filter "name=$name" --filter status=running -q | grep -q .; then break; fi
    sleep 2
  done
  if [ "$up" != 1 ]; then
    echo "::error::$name: sshd never came up" >&2
    docker logs "$name" 2>&1 | tail -30 >&2
    return 2
  fi

  local out rc
  out=$(docker run --rm --network "$net" -v "$work:/keys:ro" --entrypoint /bin/bash "$image" -c \
    'export NIX_SSHOPTS="-p 2222 -i /keys/client_key -o CertificateFile=/keys/client_key-cert.pub -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o BatchMode=yes"
     nix --extra-experimental-features nix-command store ping --store "ssh-ng://nix@'"$name"'"' 2>&1)
  rc=$?
  local server_log
  server_log=$(docker logs "$name" 2>&1)

  if [ "$expect" = pass ]; then
    if [ $rc -ne 0 ] || ! grep -q 'Trusted: 1' <<<"$out"; then
      echo "::error::expected the ssh-ng login to succeed with Trusted: 1" >&2
      printf '%s\n' "$out" >&2
      return 1
    fi
    if ! grep -q 'Accepted publickey for nix .*ED25519-CERT' <<<"$server_log"; then
      echo "::error::sshd log is missing the expected Accepted publickey line" >&2
      return 1
    fi
    if grep -qE 'bad ownership or modes|does not contain an authorized principal' <<<"$server_log"; then
      echo "::error::sshd log shows the StrictModes bug even though the login reported success" >&2
      return 1
    fi
  else
    if [ $rc -eq 0 ]; then
      echo "::error::expected the ssh-ng login to be refused, it succeeded" >&2
      return 1
    fi
  fi
  return 0
}

echo "== populated principals: login as nix must succeed (Trusted: 1) =="
make_principals_mount "$work/principals-populated" "ci-nix"
if run_scenario "$net:head" "$net-server" "$work/principals-populated" pass; then
  echo "ok    populated principals: nix login succeeds, no StrictModes failure logged"
else
  echo "FAIL  populated principals"; fail=1
fi

echo "== empty principals (the default): nix must stay unreachable =="
make_principals_mount "$work/principals-empty" ""
if run_scenario "$net:head" "$net-server" "$work/principals-empty" fail; then
  echo "ok    empty principals: nix login is still refused (AllowUsers unchanged)"
else
  echo "FAIL  empty principals"; fail=1
fi

if [ "${PROVE_CATCHES_BUG:-0}" = 1 ]; then
  baseline_ref=${BASELINE_REF:-origin/master}
  echo "== proving the test catches the bug: building $baseline_ref's image =="
  baseline_src="$work/baseline-src"
  mkdir -p "$baseline_src"
  git -C "$here" archive "$baseline_ref" -- image | tar -x -C "$baseline_src" || {
    echo "::error::could not archive $baseline_ref -- fetch it first (git fetch origin master)" >&2; exit 1; }
  docker buildx build --load --tag "$net:baseline" --file "$baseline_src/image/nix-worker/Dockerfile" "$baseline_src/image/" || {
    echo "::error::failed to build the baseline image" >&2; exit 1; }

  # The baseline chart mounted the ConfigMap straight at
  # /etc/nix-worker/principals -- reproduce THAT path, not principals-src.
  echo "== populated principals against $baseline_ref: must FAIL with the reported symptom =="
  docker rm -f "$net-server-baseline" >/dev/null 2>&1 || true
  docker run -d --name "$net-server-baseline" --network "$net" --privileged \
    -e NIX_WORKER_RUNTIME_DIR=/run/nix-worker \
    -e NIX_WORKER_ACCOUNTS_NIX_TRUSTED=true \
    -v "$work/ssh:/etc/nix-worker/ssh:ro" \
    -v "$work/ca:/etc/nix-worker/ca:ro" \
    -v "$work/principals-populated:/etc/nix-worker/principals:ro" \
    --entrypoint /bin/bash \
    "$net:baseline" \
    -c 'NIX_WORKER_SEED_ROOT=/seed-scratch nix-worker-seed && exec nix-worker-entrypoint' >/dev/null
  for _ in $(seq 1 90); do
    docker logs "$net-server-baseline" 2>&1 | grep -q "Server listening" && break
    sleep 2
  done
  out=$(docker run --rm --network "$net" -v "$work:/keys:ro" --entrypoint /bin/bash "$net:head" -c \
    'export NIX_SSHOPTS="-p 2222 -i /keys/client_key -o CertificateFile=/keys/client_key-cert.pub -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o BatchMode=yes"
     nix --extra-experimental-features nix-command store ping --store "ssh-ng://nix@'"$net-server-baseline"'"' 2>&1)
  rc=$?
  server_log=$(docker logs "$net-server-baseline" 2>&1)
  if [ $rc -eq 0 ]; then
    echo "::error::the pre-fix image was expected to refuse the login, it succeeded" >&2; fail=1
  elif ! grep -q 'bad ownership or modes' <<<"$server_log" || ! grep -q 'does not contain an authorized principal' <<<"$server_log"; then
    echo "::error::the pre-fix image failed, but not with the reported symptom -- log:" >&2
    printf '%s\n' "$server_log" >&2
    fail=1
  else
    echo "ok    $baseline_ref reproduces the exact reported failure (this test would have caught it)"
  fi
fi

if [ "$fail" = 0 ]; then
  echo "nix-worker principals e2e OK"
else
  echo "::error::nix-worker principals e2e FAILED" >&2
  exit 1
fi
