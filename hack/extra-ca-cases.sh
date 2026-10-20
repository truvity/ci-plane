#!/usr/bin/env bash
# image/runner-extra-ca.sh, run for real as a non-root user: what the
# runner container's entrypoint prefix does with CI_EXTRA_CA_DIR. No
# cluster, no image: the script only needs bash, openssl and coreutils.
#
# What has to hold: unset directory -> the command runs and nothing is
# exported; a directory of certificates -> a bundle holding the system roots
# AND the extras (an openssl verify against it succeeds for a leaf the extra
# CA signed, and still for one the system root signed), NODE_EXTRA_CA_CERTS
# naming the extras alone; a ConfigMap-shaped directory (symlinks through
# ..data) works; a missing directory, an empty one and a non-certificate file
# are reported and never stop the command; arguments pass through intact and
# so does the exit code.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$here/image/runner-extra-ca.sh"
fail=0

command -v openssl >/dev/null || { echo "openssl is required" >&2; exit 2; }

if [ "$(id -u)" = 0 ]; then
  echo "run as a non-root user: the root path installs into /usr/local/share" >&2
  exit 2
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

check() { # name, condition result (0 = ok), detail
  if [ "$2" -eq 0 ]; then
    echo "ok: $1"
  else
    echo "FAIL: $1${3:+ -- $3}"
    fail=1
  fi
}

# mkca <name>: a self-signed CA and a leaf it signs, under $work/<name>.
mkca() {
  local d="$work/$1"
  mkdir -p "$d"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$d/ca.key" -out "$d/ca.pem" -subj "/CN=$1 root" -days 2 \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
    -keyout "$d/leaf.key" -out "$d/leaf.csr" -subj "/CN=leaf.example.test" 2>/dev/null
  openssl x509 -req -in "$d/leaf.csr" -CA "$d/ca.pem" -CAkey "$d/ca.key" -CAcreateserial \
    -out "$d/leaf.pem" -days 1 2>/dev/null
}
mkca system
mkca private

# The "system" store is a stand-in so the test does not depend on the host's.
sysbundle="$work/system-bundle.crt"
cp "$work/system/ca.pem" "$sysbundle"

run() { # extra env assignments..., then the command, via env -i
  env -i PATH="$PATH" HOME="$work/home" SSL_CERT_FILE="$sysbundle" "$@"
}
mkdir -p "$work/home"

# 1. Unset: a no-op.
out=$(run bash "$script" -- env 2>&1)
check "unset: runs the command" "$?"
grep -q '^CI_EXTRA_CA_DIR=' <<< "$out"; [ $? -ne 0 ]; check "unset: nothing exported for it" "$?"
grep -qx "SSL_CERT_FILE=$sysbundle" <<< "$out"; check "unset: SSL_CERT_FILE untouched" "$?"
grep -q '^NODE_EXTRA_CA_CERTS=' <<< "$out"; [ $? -ne 0 ]; check "unset: no NODE_EXTRA_CA_CERTS" "$?"

# 2. A directory of certificates (+ a non-certificate and an unrelated file).
d="$work/ca-dir"
mkdir -p "$d"
cp "$work/private/ca.pem" "$d/private-roots.pem"
printf 'not a certificate\n' > "$d/junk.crt"
printf 'ignored\n' > "$d/README.txt"
out=$(run CI_EXTRA_CA_DIR="$d" bash "$script" env 2>&1)
check "dir: runs the command" "$?"
bundle=$(sed -n 's/^SSL_CERT_FILE=//p' <<< "$out")
extra=$(sed -n 's/^NODE_EXTRA_CA_CERTS=//p' <<< "$out")
[ -s "$bundle" ] && [ "$bundle" != "$sysbundle" ]; check "dir: SSL_CERT_FILE names a new bundle" "$?" "$bundle"
for v in NIX_SSL_CERT_FILE CURL_CA_BUNDLE GIT_SSL_CAINFO REQUESTS_CA_BUNDLE; do
  grep -qx "$v=$bundle" <<< "$out"; check "dir: $v is the bundle" "$?"
done
openssl verify -CAfile "$bundle" "$work/private/leaf.pem" >/dev/null 2>&1; check "dir: a leaf of the extra CA verifies" "$?"
openssl verify -CAfile "$bundle" "$work/system/leaf.pem" >/dev/null 2>&1; check "dir: a leaf of the system CA still verifies" "$?"
openssl verify -CAfile "$sysbundle" "$work/private/leaf.pem" >/dev/null 2>&1; [ $? -ne 0 ]; check "dir: ...and did not before (control)" "$?"
openssl verify -CAfile "$extra" "$work/private/leaf.pem" >/dev/null 2>&1; check "dir: NODE_EXTRA_CA_CERTS holds the extra CA" "$?"
openssl verify -CAfile "$extra" "$work/system/leaf.pem" >/dev/null 2>&1; [ $? -ne 0 ]; check "dir: NODE_EXTRA_CA_CERTS holds the extras alone" "$?"
grep -q 'skipping .*junk.crt' <<< "$out"; check "dir: the non-certificate is reported" "$?"

# 3. A ConfigMap volume's layout: files are symlinks through ..data.
d="$work/cm"
mkdir -p "$d/..2026_10_20" 
cp "$work/private/ca.pem" "$d/..2026_10_20/roots.pem"
ln -s ..2026_10_20 "$d/..data"
ln -s ..data/roots.pem "$d/roots.pem"
out=$(run CI_EXTRA_CA_DIR="$d" bash "$script" -- env 2>&1)
bundle=$(sed -n 's/^SSL_CERT_FILE=//p' <<< "$out")
openssl verify -CAfile "$bundle" "$work/private/leaf.pem" >/dev/null 2>&1; check "configmap layout: the extra CA is trusted" "$?"

# 4. A file without a trailing newline cannot fuse two blocks.
d="$work/nonl"
mkdir -p "$d"
printf '%s' "$(cat "$work/private/ca.pem")" > "$d/a.pem"
printf '%s' "$(cat "$work/system/ca.pem")" > "$d/b.crt"
out=$(run CI_EXTRA_CA_DIR="$d" bash "$script" env 2>&1)
extra=$(sed -n 's/^NODE_EXTRA_CA_CERTS=//p' <<< "$out")
[ "$(grep -c '^-----BEGIN CERTIFICATE-----$' "$extra")" = 2 ] && [ "$(grep -c '^-----END CERTIFICATE-----$' "$extra")" = 2 ]
check "no trailing newline: two separate blocks" "$?"

# 5. Misconfiguration is reported and never stops the command.
out=$(run CI_EXTRA_CA_DIR="$work/missing" bash "$script" echo started 2>&1)
check "missing dir: the command still runs" "$?"
grep -q 'not a directory' <<< "$out" && grep -qx started <<< "$out"; check "missing dir: reported" "$?"
mkdir -p "$work/empty"
out=$(run CI_EXTRA_CA_DIR="$work/empty" bash "$script" env 2>&1)
check "empty dir: the command still runs" "$?"
grep -q 'no \*.pem or \*.crt' <<< "$out"; check "empty dir: reported" "$?"
grep -qx "SSL_CERT_FILE=$sysbundle" <<< "$out"; check "empty dir: SSL_CERT_FILE untouched" "$?"

# 6. Arguments and exit code pass through; no command is a usage error.
out=$(run bash "$script" -- printf '%s|' 'a b' c 2>&1)
[ "$out" = 'a b|c|' ]; check "arguments pass through intact" "$?" "$out"
run bash "$script" -- bash -c 'exit 7' 2>/dev/null; [ $? -eq 7 ]; check "exit code passes through" "$?"
run bash "$script" 2>/dev/null; [ $? -eq 2 ]; check "no command: usage error" "$?"

exit "$fail"
