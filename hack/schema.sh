#!/usr/bin/env bash
# Schema-validate every committed golden render against the Kubernetes API.
#
# The goldens compare text, so a manifest the API server refuses (a field
# one level too high, a misspelt key) is a clean diff. This runs
# `kubeconform -strict` over every golden: unknown fields fail, as do wrong
# types. Kinds with no schema in the Kubernetes set (cert-manager,
# External Secrets, ...) are skipped. The goldens are the same bytes the
# `golden` recipe proves the templates render, so run both.
#
#   hack/schema.sh                     the Kubernetes version below
#   K8S_VERSION=1.35.0 hack/schema.sh  another one
#
# Fetching the schemas needs network access (kubeconform's default
# location, github.com/yannh/kubernetes-json-schema).
set -uo pipefail

cd "$(dirname "$0")/.."

version=${K8S_VERSION:-1.36.0}
goldens=(tests/golden/*/*.yaml)
[ -e "${goldens[0]}" ] || { echo "no goldens -- that is a failure, not a pass" >&2; exit 1; }

fail=0
for golden in "${goldens[@]}"; do
  if ! out=$(kubeconform -strict -kubernetes-version "$version" -ignore-missing-schemas -summary "$golden" 2>&1); then
    echo "SCHEMA: $golden"
    sed 's/^/    /' <<< "$out" | head -20
    fail=1
  fi
done

# The ESO CRD defaults spec.data[].conversionStrategy to None. A render that
# leaves it out is stored with it filled in, and Argo CD then shows the
# application OutOfSync for good (the apply is a no-op, the diff stays). So
# every PushSecret data entry in the goldens must say it. The kubeconform
# pass above skips External Secrets kinds (no schema in the Kubernetes set),
# which is why this is its own check.
missing=$(yq eval-all '[select(.kind == "PushSecret") | .spec.data[] | select(has("conversionStrategy") | not)] | length' "${goldens[@]}" | awk '{ n += $1 } END { print n + 0 }')
pushsecrets=$(yq eval-all '[select(.kind == "PushSecret")] | length' "${goldens[@]}" | awk '{ n += $1 } END { print n + 0 }')
if [ "$missing" -ne 0 ]; then
  echo "ESO-DEFAULTS: $missing PushSecret spec.data entries lack conversionStrategy (the CRD defaults it to None; leaving it out is permanent Argo CD drift)"
  fail=1
fi
if [ "$pushsecrets" -eq 0 ]; then
  echo "ESO-DEFAULTS: no PushSecret in any golden -- that is a failure, not a pass"
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "schema: ${#goldens[@]} goldens valid against Kubernetes $version"
fi
exit "$fail"
