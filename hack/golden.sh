#!/usr/bin/env bash
# Golden renders and refused fixtures, for every chart.
#
#   hack/golden.sh           check: render, compare, prove each refusal
#   hack/golden.sh update    rewrite tests/golden from the cases
#
# Every tests/cases/<chart>/<case>/values.yaml renders, in namespace
# `example`, to tests/golden/<chart>/<case>.yaml. The goldens are
# committed, so a template change that alters a manifest is a diff a
# reviewer reads rather than a surprise in a cluster.
#
# Every tests/invalid/<chart>/*.yaml must FAIL to render, and its first
# line is `# refuses: <words>`: the words must appear in what helm says.
# A fixture that fails for some other reason -- a typo in the fixture, a
# helper that broke -- has proved nothing about the rule it names.
set -uo pipefail

cd "$(dirname "$0")/.."

mode=${1:-check}
case "$mode" in check|update) ;; *) echo "usage: $0 [check|update]" >&2; exit 2 ;; esac

fail=0
rendered=0
refused=0

for chart_dir in charts/*/; do
  chart=$(basename "$chart_dir")

  cases=0
  for values in tests/cases/"$chart"/*/values.yaml; do
    [ -e "$values" ] || continue
    cases=$((cases + 1))
    name=$(basename "$(dirname "$values")")
    golden="tests/golden/$chart/$name.yaml"
    if ! out=$(helm template "$chart" "$chart_dir" --namespace example -f "$values" 2>&1); then
      echo "DID NOT RENDER: $values"
      sed 's/^/    /' <<< "$out" | tail -5
      fail=1
      continue
    fi
    rendered=$((rendered + 1))
    if [ "$mode" = update ]; then
      mkdir -p "tests/golden/$chart"
      printf '%s\n' "$out" > "$golden"
    elif [ ! -f "$golden" ]; then
      echo "NO GOLDEN: $golden (run: just golden-update)"
      fail=1
    elif ! diff -u "$golden" <(printf '%s\n' "$out") > /dev/null; then
      echo "GOLDEN DIFFERS: $golden (run: just golden-update, then review the diff)"
      diff -u "$golden" <(printf '%s\n' "$out") | head -40 | sed 's/^/    /'
      fail=1
    fi
  done
  # A chart with no case is a chart nothing renders, which diffs clean.
  if [ "$cases" -eq 0 ]; then
    echo "NO CASES: tests/cases/$chart/ is empty"
    fail=1
  fi

  fixtures=0
  for values in tests/invalid/"$chart"/*.yaml; do
    [ -e "$values" ] || continue
    fixtures=$((fixtures + 1))
    want=$(head -1 "$values" | sed -n 's/^# refuses: //p')
    if [ -z "$want" ]; then
      echo "FIXTURE SAYS NOTHING: $values has no '# refuses: <words>' first line"
      fail=1
      continue
    fi
    if out=$(helm template "$chart" "$chart_dir" --namespace example -f "$values" 2>&1); then
      echo "RENDERED BUT SHOULD HAVE FAILED: $values"
      fail=1
    elif ! grep -qF -- "$want" <<< "$out"; then
      echo "REFUSED WITHOUT SAYING WHY: $values"
      echo "    wanted the words: $want"
      echo "    said: $(tail -3 <<< "$out" | tr '\n' ' ')"
      fail=1
    else
      refused=$((refused + 1))
    fi
  done
  if [ "$fixtures" -eq 0 ]; then
    echo "NO FIXTURES: tests/invalid/$chart/ is empty -- nothing proves a refusal"
    fail=1
  fi
done

# A golden whose case was deleted is left behind, still looking tested.
for golden in tests/golden/*/*.yaml; do
  [ -e "$golden" ] || continue
  chart=$(basename "$(dirname "$golden")")
  name=$(basename "$golden" .yaml)
  if [ ! -f "tests/cases/$chart/$name/values.yaml" ]; then
    echo "ORPHAN GOLDEN: $golden has no tests/cases/$chart/$name/values.yaml"
    fail=1
  fi
done

if [ "$rendered" -eq 0 ]; then
  echo "rendered nothing -- that is a failure, not a pass"
  exit 1
fi
if [ "$fail" -eq 0 ]; then
  echo "golden ($mode): $rendered renders, $refused fixtures refused"
fi
exit "$fail"
