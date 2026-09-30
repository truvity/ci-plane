#!/usr/bin/env bash
# Regression test for the nix-worker `nix` login and StrictModes, the
# removed legacy `nixremote` login, and (v5.0.0) the ephemeral per-pod
# host key signed as a host certificate.
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
# Scenarios, every run:
#   populated  -- a cert for principal `ci-nix` must be accepted, and the
#                 daemon must answer over ssh-ng (`nix store ping`
#                 reports Trusted: 1, the same proof used live). Static
#                 host key (hostCertificate off).
#   no nixremote -- v5.0.0 removed the legacy login: the image has no
#                 `nixremote` account and no nix-store --serve shim, a
#                 `nixremote` certificate is refused, and a worker told
#                 NIX_WORKER_ACCOUNTS_NIXREMOTE_ENABLED=true (an old
#                 chart) refuses to start rather than run without it.
#   empty      -- nixWorkers.accounts.nix.principals empty and opkssh off
#                 leaves no login at all: the worker refuses to start.
#   opkssh     -- the people pilot (see check_opkssh below): with
#                 NIX_WORKER_OPKSSH_ENABLED=false (the default), `admin`
#                 must be refused for the AllowUsers reason; with it
#                 true, `admin` must be REACHABLE (AllowUsers admits the
#                 connection and sshd runs opkssh's own
#                 AuthorizedKeysCommand) while still refusing a plain key
#                 that carries no OpenPubkey ID token -- this script has
#                 no OIDC provider to mint a real one against, so it
#                 proves the sshd-level wiring (account admitted, opkssh
#                 actually invoked, no accidental bypass) and leaves
#                 verifying a genuine sign-in to the live pilot's own
#                 smoke test.
#   ephemeral host key -- NIX_WORKER_HOST_CERTIFICATE_ENABLED=true with NO
#                 static key mounted: two workers each generate their
#                 own host key (they must differ), get it signed (a stand-
#                 in signer with a local host CA replaces the OpenBao
#                 client binary; the Go binary has its own unit tests),
#                 and a client whose known_hosts holds ONLY the
#                 `@cert-authority` line connects with strict host-key
#                 checking. The readiness probe passes, a client trusting
#                 a different CA is refused, and a renewal re-signs the
#                 same key and keeps serving.
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
  docker rm -f "$net-server" "$net-server-baseline" "$net-server-opkssh" "$net-server-empty" \
    "$net-server-nixremote-on" "$net-server-cert-a" "$net-server-cert-b" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "== building the nix-worker image under test =="
docker buildx build --load --tag "$net:head" --file "$here/image/nix-worker/Dockerfile" "$here/image/" || {
  echo "::error::failed to build image/nix-worker/Dockerfile" >&2; exit 1; }

# One CA, one host key, one client cert per login -- shared by every
# scenario below. `client_key` carries the `ci-nix` principal (the `nix`
# ssh-ng login); `client_key_nixremote` carries `nixremote` itself, used
# only to prove the removed legacy login stays refused.
ssh-keygen -q -t ed25519 -N '' -f "$work/ca_key" -C test-ca
ssh-keygen -q -t ed25519 -N '' -f "$work/host_key" -C worker-host
ssh-keygen -q -t ed25519 -N '' -f "$work/client_key" -C test-client
ssh-keygen -s "$work/ca_key" -I ci-nix-test-cert -n ci-nix -V always:forever "$work/client_key.pub" >/dev/null
ssh-keygen -q -t ed25519 -N '' -f "$work/client_key_nixremote" -C test-client-nixremote
ssh-keygen -s "$work/ca_key" -I nixremote-test-cert -n nixremote -V always:forever "$work/client_key_nixremote.pub" >/dev/null

mkdir -p "$work/ssh" "$work/ca"
cp "$work/host_key" "$work/ssh/ssh_host_ed25519_key"
cp "$work/ca_key.pub" "$work/ca/trusted_user_ca_keys"

# Host certificates (the ephemeral host key scenario). A local host CA,
# a second unrelated one a wrong client trusts, and a stand-in for
# /usr/local/bin/nix-worker-host-cert with the same contract the
# entrypoint relies on (sign NIX_WORKER_HOST_PUBLIC_KEY_FILE for
# NIX_WORKER_HOST_PRINCIPALS, write NIX_WORKER_HOST_CERT_FILE, exit 0)
# but signing locally instead of at OpenBao. Its identity carries a
# counter so a renewal is visibly a NEW certificate.
mkdir -p "$work/host-ca" "$work/signer"
ssh-keygen -q -t ed25519 -N '' -f "$work/host-ca/host_ca" -C test-host-ca
ssh-keygen -q -t ed25519 -N '' -f "$work/other_host_ca" -C other-host-ca
cat > "$work/signer/nix-worker-host-cert" <<'SIGNER_EOF'
#!/usr/bin/env bash
set -euo pipefail
tmp=$(mktemp -d)
cp "$NIX_WORKER_HOST_PUBLIC_KEY_FILE" "$tmp/host.pub"
ssh-keygen -q -s /host-ca/host_ca -I "e2e-$HOSTNAME-$(date +%s%N)" -h \
  -n "$NIX_WORKER_HOST_PRINCIPALS" -V -5m:+24h "$tmp/host.pub"
install -m 0644 "$tmp/host-cert.pub" "$NIX_WORKER_HOST_CERT_FILE"
rm -rf "$tmp"
echo "e2e signer: signed $(ssh-keygen -l -f "$NIX_WORKER_HOST_PUBLIC_KEY_FILE" | awk '{print $2}')"
SIGNER_EOF
chmod 0755 "$work/signer/nix-worker-host-cert"
printf '@cert-authority * %s\n' "$(cat "$work/host-ca/host_ca.pub")" > "$work/known_hosts_ca"
printf '@cert-authority * %s\n' "$(cat "$work/other_host_ca.pub")" > "$work/known_hosts_other_ca"

# opkssh (the people pilot) fixtures. A PLAIN keypair, no
# certificate at all: this script has no OIDC provider to mint a real
# OpenPubkey ID token against, so it cannot prove a genuine sign-in
# succeeds -- only that the `admin` account is reachable at the sshd
# level (AllowUsers admits the connection, sshd actually invokes
# `opkssh verify`) and that a bogus credential is still refused by
# opkssh itself, not silently accepted.
ssh-keygen -q -t ed25519 -N '' -f "$work/plain_client_key" -C test-plain-key
mkdir -p "$work/opkssh-src"
printf 'https://issuer.example opkssh 24h\n' > "$work/opkssh-src/providers"
printf 'admin oidc:groups:test.group https://issuer.example\n' > "$work/opkssh-src/auth_id"

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

# The ConfigMap mount at principals-src, entrypoint copying it out
# before sshd starts -- as the chart runs it.
# Starts the image under test as the chart does. `extra_args` are docker
# run arguments this script builds itself (never external input), and
# word-splitting them is the point. The static host key is mounted unless
# they ask for host certificates, exactly like the chart.
start_server() {
  local image="$1" name="$2" principals_dir="$3" extra_args="${4:-}"
  local ssh_mount=(-v "$work/ssh:/etc/nix-worker/ssh:ro")
  [[ "$extra_args" == *NIX_WORKER_HOST_CERTIFICATE_ENABLED=true* ]] && ssh_mount=()
  docker rm -f "$name" >/dev/null 2>&1 || true
  # shellcheck disable=SC2086
  docker run -d --name "$name" --hostname "$name" --network "$net" --privileged \
    -e NIX_WORKER_RUNTIME_DIR=/run/nix-worker \
    -e NIX_WORKER_ACCOUNTS_NIX_TRUSTED=true \
    $extra_args \
    "${ssh_mount[@]}" \
    -v "$work/ca:/etc/nix-worker/ca:ro" \
    -v "$principals_dir:/etc/nix-worker/principals-src:ro" \
    -v "$work/opkssh-src:/etc/nix-worker/opkssh-src:ro" \
    --entrypoint /bin/bash \
    "$image" \
    -c 'NIX_WORKER_SEED_ROOT=/seed-scratch nix-worker-seed && exec nix-worker-entrypoint' >/dev/null
}

# Waits for sshd; 0 when it listens, 2 when the container died first.
wait_for_sshd() {
  local name="$1" up=0
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
  return 0
}

# `nix store ping` over ssh-ng as `nix` (via a second, short-lived
# container on the same network) -- the exact proof used live: Trusted: 1
# means auth succeeded AND the forced command (`nix-daemon --stdio`)
# answered the wire protocol. `host_opts` decides how the client trusts
# the server (default: not at all, the static-key scenarios' shape).
store_ping() {
  local image="$1" name="$2" host_opts="${3:--o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no}"
  docker run --rm --network "$net" -v "$work:/keys:ro" --entrypoint /bin/bash "$image" -c \
    'export NIX_SSHOPTS="-p 2222 -i /keys/client_key -o CertificateFile=/keys/client_key-cert.pub '"$host_opts"' -o BatchMode=yes"
     nix --extra-experimental-features nix-command store ping --store "ssh-ng://nix@'"$name"'"' 2>&1
}

run_scenario() {
  local image="$1" name="$2" principals_dir="$3" expect="$4" extra_args="${5:-}" host_opts="${6:-}" # expect: pass|fail
  start_server "$image" "$name" "$principals_dir" "$extra_args"
  wait_for_sshd "$name" || return 2

  local out rc
  out=$(store_ping "$image" "$name" ${host_opts:+"$host_opts"})
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

# The removed legacy `nixremote` login (`nix-store --serve`), attempted
# with a certificate carrying the `nixremote` principal: v5.0.0 removed
# the account, so sshd refuses it before any key is even considered.
check_nixremote_refused() {
  local image="$1" name="$2"
  local out rc server_log
  out=$(docker run --rm --network "$net" -v "$work:/keys:ro" --entrypoint /bin/bash "$image" -c \
    'ssh -p 2222 -i /keys/client_key_nixremote -o CertificateFile=/keys/client_key_nixremote-cert.pub -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=10 nixremote@'"$name"' true' 2>&1)
  rc=$?
  server_log=$(docker logs "$name" 2>&1)
  if [ $rc -eq 0 ]; then
    echo "::error::expected the legacy nixremote login to be refused, it succeeded" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
  if ! grep -qE 'Invalid user nixremote|User nixremote .*not allowed' <<<"$server_log"; then
    echo "::error::nixremote login was refused, but not because the account is gone -- server log:" >&2
    printf '%s\n' "$server_log" >&2
    return 1
  fi
  if docker run --rm --entrypoint /bin/bash "$image" -c 'id nixremote || test -e /usr/local/bin/nix-worker-ssh-command' >/dev/null 2>&1; then
    echo "::error::the image still carries the nixremote account or its shim" >&2
    return 1
  fi
  return 0
}

# A worker that must REFUSE to start: the container exits (never
# listening) with `want` in its log.
check_refuses_to_start() {
  local name="$1" want="$2" state
  for _ in $(seq 1 90); do
    state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null || echo gone)
    [ "$state" = exited ] && break
    if docker logs "$name" 2>&1 | grep -q "Server listening"; then break; fi
    sleep 2
  done
  if [ "$state" != exited ]; then
    echo "::error::$name: expected the worker to refuse to start, it is $state" >&2
    docker logs "$name" 2>&1 | tail -20 >&2
    return 1
  fi
  if ! docker logs "$name" 2>&1 | grep -qF -- "$want"; then
    echo "::error::$name refused to start, but not saying: $want" >&2
    docker logs "$name" 2>&1 | tail -20 >&2
    return 1
  fi
  return 0
}

# The ephemeral host key (v5.0.0). Both workers must be up already.
host_fingerprint() {
  docker exec "$1" ssh-keygen -l -f /run/nix-worker/ssh_host_ed25519_key.pub | awk '{print $2}'
}
check_ephemeral_host_keys() {
  local image="$1" a="$2" b="$3" fa fb out rc
  fa=$(host_fingerprint "$a") && fb=$(host_fingerprint "$b") || {
    echo "::error::no ephemeral host public key in the runtime dir" >&2; return 1; }
  if [ -z "$fa" ] || [ "$fa" = "$fb" ]; then
    echo "::error::the two workers share a host key ($fa / $fb): it must be generated per pod" >&2
    return 1
  fi
  for w in "$a" "$b"; do
    if docker exec "$w" test -e /etc/nix-worker/ssh/ssh_host_ed25519_key; then
      echo "::error::$w: a static host key is present; the certificate path must not need one" >&2
      return 1
    fi
    if ! docker exec "$w" grep -qx 'HostKey /run/nix-worker/ssh_host_ed25519_key' /run/nix-worker/sshd_config \
       || ! docker exec "$w" grep -qx 'HostCertificate /run/nix-worker/ssh_host_ed25519_key-cert.pub' /run/nix-worker/sshd_config; then
      echo "::error::$w: the runtime sshd_config does not serve the ephemeral key with its certificate" >&2
      return 1
    fi
    if ! out=$(docker exec "$w" /usr/local/bin/nix-worker-healthcheck --readiness 2>&1); then
      echo "::error::$w: readiness failed with a valid certificate: $out" >&2
      return 1
    fi
  done
  # The certificate is what the client checks: a known_hosts holding
  # ONLY the @cert-authority line, strict checking on.
  for w in "$a" "$b"; do
    out=$(store_ping "$image" "$w" "-o UserKnownHostsFile=/keys/known_hosts_ca -o StrictHostKeyChecking=yes")
    rc=$?
    if [ $rc -ne 0 ] || ! grep -q 'Trusted: 1' <<<"$out"; then
      echo "::error::$w: a client trusting only the host CA could not connect:" >&2
      printf '%s\n' "$out" >&2
      return 1
    fi
    if ! docker logs "$w" 2>&1 | grep -q 'Accepted publickey for nix .*ED25519-CERT'; then
      echo "::error::$w: sshd log is missing Accepted publickey for nix" >&2
      return 1
    fi
  done
  # ...and a client trusting a DIFFERENT CA is refused.
  out=$(store_ping "$image" "$a" "-o UserKnownHostsFile=/keys/known_hosts_other_ca -o StrictHostKeyChecking=yes")
  if [ $? -eq 0 ]; then
    echo "::error::a client trusting an unrelated host CA was let through" >&2
    return 1
  fi
  # Readiness is NOT fooled by a fresh certificate for another key.
  # On worker B, whose renewal (12h) cannot race the swap.
  docker exec "$b" bash -c 'cp /run/nix-worker/ssh_host_ed25519_key-cert.pub /tmp/good-cert.pub'
  docker exec "$b" bash -c 'ssh-keygen -q -t ed25519 -N "" -f /tmp/other && cp /tmp/other.pub /tmp/o.pub \
    && ssh-keygen -q -s /host-ca/host_ca -I wrong -h -n x -V -5m:+24h /tmp/o.pub \
    && cp /tmp/o-cert.pub /run/nix-worker/ssh_host_ed25519_key-cert.pub'
  if docker exec "$b" /usr/local/bin/nix-worker-healthcheck --readiness >/dev/null 2>&1; then
    echo "::error::readiness passed with a certificate for a different key" >&2
    return 1
  fi
  docker exec "$b" bash -c 'cp /tmp/good-cert.pub /run/nix-worker/ssh_host_ed25519_key-cert.pub'
  # Renewal (renewEvery=10s on worker A): the SAME key is re-signed, sshd
  # reloads on SIGHUP, and the worker keeps serving CA-trusting clients.
  # Counted, not grepped: earlier renewals may already be in the log.
  local before after renewed0
  renewed0=$(docker logs "$a" 2>&1 | grep -c 'host certificate renewed')
  before=$(docker exec "$a" ssh-keygen -L -f /run/nix-worker/ssh_host_ed25519_key-cert.pub | sed -n 's/^ *Key ID: //p')
  for _ in $(seq 1 30); do
    [ "$(docker logs "$a" 2>&1 | grep -c 'host certificate renewed')" -gt "$renewed0" ] && break
    sleep 1
  done
  after=$(docker exec "$a" ssh-keygen -L -f /run/nix-worker/ssh_host_ed25519_key-cert.pub | sed -n 's/^ *Key ID: //p')
  if [ "$before" = "$after" ] || [ "$(host_fingerprint "$a")" != "$fa" ]; then
    echo "::error::renewal did not re-sign the same ephemeral key ($before -> $after)" >&2
    return 1
  fi
  out=$(store_ping "$image" "$a" "-o UserKnownHostsFile=/keys/known_hosts_ca -o StrictHostKeyChecking=yes")
  if [ $? -ne 0 ] || ! grep -q 'Trusted: 1' <<<"$out"; then
    echo "::error::$a stopped serving after a renewal:" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
  return 0
}

# opkssh (the people pilot). No OIDC provider exists in this
# harness to mint a real OpenPubkey ID token, so `admin`'s login always
# fails here -- what these two checks tell apart is WHY it fails.
# Admitted: sshd's AllowUsers let the connection through and actually
# invoked `opkssh verify` (proven by that exact log line and by
# "Failed publickey", opkssh's own refusal of a non-certificate key --
# never "not allowed because not listed in AllowUsers", which would mean
# opkssh was never reached at all).
check_opkssh_admitted() {
  local image="$1" name="$2"
  local out rc server_log
  out=$(docker run --rm --network "$net" -v "$work:/keys:ro" --entrypoint /bin/bash "$image" -c \
    'ssh -p 2222 -i /keys/plain_client_key -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=10 admin@'"$name"' true' 2>&1)
  rc=$?
  server_log=$(docker logs "$name" 2>&1)
  if [ $rc -eq 0 ]; then
    echo "::error::expected the bogus-key admin login to be refused, it succeeded" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
  if grep -q 'not allowed because not listed in AllowUsers' <<<"$server_log"; then
    echo "::error::admin login was refused at AllowUsers -- opkssh was never invoked" >&2
    return 1
  fi
  if ! grep -q '/usr/local/bin/opkssh verify admin ' <<<"$server_log"; then
    echo "::error::sshd log is missing the expected AuthorizedKeysCommand invocation" >&2
    printf '%s\n' "$server_log" >&2
    return 1
  fi
  if ! grep -q 'Failed publickey for admin' <<<"$server_log"; then
    echo "::error::admin login was refused, but not by opkssh's own publickey check -- server log:" >&2
    printf '%s\n' "$server_log" >&2
    return 1
  fi
  return 0
}

# opkssh disabled (NIX_WORKER_OPKSSH_ENABLED unset, the default): `admin`
# must be refused for the AllowUsers reason -- opkssh verify must never
# even run.
check_opkssh_refused_disabled() {
  local image="$1" name="$2"
  local out rc server_log
  out=$(docker run --rm --network "$net" -v "$work:/keys:ro" --entrypoint /bin/bash "$image" -c \
    'ssh -p 2222 -i /keys/plain_client_key -o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o BatchMode=yes -o ConnectTimeout=10 admin@'"$name"' true' 2>&1)
  rc=$?
  server_log=$(docker logs "$name" 2>&1)
  if [ $rc -eq 0 ]; then
    echo "::error::expected the admin login to be refused with opkssh disabled, it succeeded" >&2
    printf '%s\n' "$out" >&2
    return 1
  fi
  if ! grep -q 'not allowed because not listed in AllowUsers' <<<"$server_log"; then
    echo "::error::admin login was refused, but not for the expected AllowUsers reason -- server log:" >&2
    printf '%s\n' "$server_log" >&2
    return 1
  fi
  return 0
}

echo "== populated principals: login as nix must succeed (Trusted: 1) =="
make_principals_mount "$work/principals-populated" "ci-nix"
make_principals_mount "$work/principals-empty" ""
if run_scenario "$net:head" "$net-server" "$work/principals-populated" pass; then
  echo "ok    populated principals: nix login succeeds, no StrictModes failure logged"
else
  echo "FAIL  populated principals"; fail=1
fi

echo "== no nixremote: the removed legacy login is refused, the account and shim are gone =="
if check_nixremote_refused "$net:head" "$net-server"; then
  echo "ok    no nixremote: login refused (no such user), no account or shim in the image"
else
  echo "FAIL  no nixremote"; fail=1
fi

echo "== opkssh disabled (the default): admin stays unreachable =="
if check_opkssh_refused_disabled "$net:head" "$net-server"; then
  echo "ok    opkssh disabled: admin login refused (AllowUsers)"
else
  echo "FAIL  opkssh disabled: admin login was not refused for the expected AllowUsers reason"; fail=1
fi

echo "== an old chart asking for nixremote: the worker refuses to start =="
start_server "$net:head" "$net-server-nixremote-on" "$work/principals-populated" "-e NIX_WORKER_ACCOUNTS_NIXREMOTE_ENABLED=true"
if check_refuses_to_start "$net-server-nixremote-on" "has no nixremote login"; then
  echo "ok    NIX_WORKER_ACCOUNTS_NIXREMOTE_ENABLED=true: refused to start, naming why"
else
  echo "FAIL  NIX_WORKER_ACCOUNTS_NIXREMOTE_ENABLED=true"; fail=1
fi

echo "== empty principals, opkssh off: no login at all, the worker refuses to start =="
start_server "$net:head" "$net-server-empty" "$work/principals-empty"
if check_refuses_to_start "$net-server-empty" "no login is configured"; then
  echo "ok    empty principals: refused to start, naming why"
else
  echo "FAIL  empty principals"; fail=1
fi

echo "== opkssh enabled: admin is reachable, opkssh itself refuses a non-certificate key =="
if run_scenario "$net:head" "$net-server-opkssh" "$work/principals-empty" fail \
     "-e NIX_WORKER_OPKSSH_ENABLED=true"; then
  if check_opkssh_admitted "$net:head" "$net-server-opkssh"; then
    echo "ok    opkssh enabled: admin login reaches opkssh verify (AllowUsers admits it), refused only for lacking a real ID token"
  else
    echo "FAIL  opkssh enabled: admin was not admitted/verified as expected"; fail=1
  fi
else
  echo "FAIL  opkssh enabled: the empty-principals nix scenario regressed"; fail=1
fi

echo "== ephemeral host key: per-pod keys, certified, trusted through the CA alone =="
cert_args="-e NIX_WORKER_HOST_CERTIFICATE_ENABLED=true -v $work/signer/nix-worker-host-cert:/usr/local/bin/nix-worker-host-cert:ro -v $work/host-ca:/host-ca:ro"
start_server "$net:head" "$net-server-cert-a" "$work/principals-populated" \
  "$cert_args -e NIX_WORKER_HOST_PRINCIPALS=$net-server-cert-a -e NIX_WORKER_HOST_CERTIFICATE_RENEW_EVERY=10s"
start_server "$net:head" "$net-server-cert-b" "$work/principals-populated" \
  "$cert_args -e NIX_WORKER_HOST_PRINCIPALS=$net-server-cert-b"
if wait_for_sshd "$net-server-cert-a" && wait_for_sshd "$net-server-cert-b" \
   && check_ephemeral_host_keys "$net:head" "$net-server-cert-a" "$net-server-cert-b"; then
  echo "ok    ephemeral host key: distinct per worker, certified, CA-only known_hosts connects, wrong CA refused, readiness checks the key, renewal keeps serving"
else
  echo "FAIL  ephemeral host key"; fail=1
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
