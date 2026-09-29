#!/usr/bin/env bash
# Runner packing (values.packing), asserted on the RENDERED chart.
#
# Packing is a scheduling PREFERENCE, so nothing fails when it is wrong:
# a missing label, a term that matches only its own namespace, or a
# caller's affinity silently replaced all render, install and run -- the
# runners just spread again. These cases are the only place that shows.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for t in helm python3; do
  command -v "$t" >/dev/null || { echo "$t is required" >&2; exit 2; }
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

render() {
  helm template t "$here/charts/arc-runners" \
    --set githubConfigUrl=https://github.com/example \
    --set controllerServiceAccount.namespace=arc-system "$@"
}

# Checks every AutoscalingRunnerSet in the render file $2. $1 is the
# case: on | off | merged. (A file, not stdin: the heredoc below IS
# python's stdin.)
check() {
  python3 - "$1" "$2" <<'PY'
import sys, yaml

case, path = sys.argv[1], sys.argv[2]
LABEL = {"ci-plane.io/packing-group": "runners"}
TERM = {
    "weight": 100,
    "podAffinityTerm": {
        "topologyKey": "kubernetes.io/hostname",
        "namespaceSelector": {},
        "labelSelector": {"matchLabels": LABEL},
    },
}

sets = [d for d in yaml.safe_load_all(open(path))
        if d and d.get("kind") == "AutoscalingRunnerSet"]
if len(sets) < 3:
    sys.exit(f"{case}: expected every default scale set, found {len(sets)}")

bad = []
for s in sets:
    name = s["metadata"]["name"]
    tmpl = s["spec"]["template"]
    labels = (tmpl.get("metadata") or {}).get("labels") or {}
    aff = tmpl["spec"].get("affinity")
    if case == "off":
        if "ci-plane.io/packing-group" in labels:
            bad.append(f"{name}: packing label rendered with packing off")
        if aff is not None:
            bad.append(f"{name}: affinity rendered with packing off: {aff}")
        continue
    if labels.get("ci-plane.io/packing-group") != "runners":
        bad.append(f"{name}: runner pod is missing the packing label")
    pref = ((aff or {}).get("podAffinity") or {}).get(
        "preferredDuringSchedulingIgnoredDuringExecution") or []
    if pref.count(TERM) != 1:
        # More than one means the render appended to .Values and the
        # terms pile up set after set.
        bad.append(f"{name}: expected exactly one packing term, got {pref}")
    if (aff or {}).get("podAffinity", {}).get(
            "requiredDuringSchedulingIgnoredDuringExecution"):
        bad.append(f"{name}: packing must never be a REQUIRED term")
    if case == "merged":
        if len(pref) != 2 or pref[0].get("weight") != 7:
            bad.append(f"{name}: the caller's preferred pod term was not kept first: {pref}")
        if "nodeAffinity" not in aff:
            bad.append(f"{name}: the caller's nodeAffinity was dropped")
        if "podAntiAffinity" not in aff:
            bad.append(f"{name}: the caller's podAntiAffinity was dropped")

if bad:
    print("\n".join(bad), file=sys.stderr)
    sys.exit(1)
print(f"{case}: {len(sets)} scale sets OK")
PY
}

fail=0

render > "$work/on.yaml" || { echo "::error::default render failed" >&2; exit 1; }
check on "$work/on.yaml" || fail=1

render --set packing.enabled=false > "$work/off.yaml" \
  || { echo "::error::packing-off render failed" >&2; exit 1; }
check off "$work/off.yaml" || fail=1

cat > "$work/affinity.yaml" <<'EOF'
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - {key: kubernetes.io/arch, operator: In, values: [arm64]}
  podAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 7
        podAffinityTerm:
          topologyKey: topology.kubernetes.io/zone
          labelSelector:
            matchLabels: {app: example-cache}
  podAntiAffinity:
    preferredDuringSchedulingIgnoredDuringExecution:
      - weight: 1
        podAffinityTerm:
          topologyKey: kubernetes.io/hostname
          labelSelector:
            matchLabels: {app: example-noisy}
EOF
render -f "$work/affinity.yaml" > "$work/merged.yaml" \
  || { echo "::error::caller-affinity render failed" >&2; exit 1; }
check merged "$work/merged.yaml" || fail=1

# Off with a caller's affinity: rendered exactly as given.
render -f "$work/affinity.yaml" --set packing.enabled=false \
  | python3 -c '
import sys, yaml
want = yaml.safe_load(open(sys.argv[1]))["affinity"]
n = 0
for d in yaml.safe_load_all(sys.stdin):
    if d and d.get("kind") == "AutoscalingRunnerSet":
        n += 1
        got = d["spec"]["template"]["spec"].get("affinity")
        if got != want:
            sys.exit(d["metadata"]["name"] + ": caller affinity not rendered verbatim with packing off")
if n < 3:
    sys.exit(f"off+caller affinity: expected every default scale set, found {n}")
print(f"off+caller affinity: {n} scale sets verbatim OK")
' "$work/affinity.yaml" || fail=1

# The weight bound is refused at render, not left to the API server.
for w in 0 101; do
  if render --set packing.weight=$w > /dev/null 2>&1; then
    echo "packing.weight=$w rendered; it must be refused" >&2
    fail=1
  fi
done

if [ "$fail" -ne 0 ]; then
  echo "::error::packing cases failed" >&2
  exit 1
fi
echo "packing cases OK"
