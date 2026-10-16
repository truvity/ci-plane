#!/usr/bin/env bash
# Golden renders and refused fixtures, for every chart.
#
#   hack/golden.sh           check: render, compare, prove each refusal
#   hack/golden.sh update    rewrite tests/golden from the cases
#
# Every tests/cases/<chart>/<case>/values.yaml renders, in namespace
# `example`, to tests/golden/<chart>/<case>.yaml. A case that proves a
# preset has a `presets` file beside its values.yaml (one preset name per
# line): each is passed as `-f charts/<chart>/presets/<name>.yaml` BEFORE
# the case's values, the order a consumer lists them in. A case WITHOUT a
# `presets` file is rendered over tests/cases/<chart>/base.yaml, when that
# exists: the scale sets the chart no longer ships by default, so the
# cases that exercise something else (a nix sandbox, a watchdog) still
# have sets to exercise it on. The goldens are
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
    preset_args=()
    if [ ! -f "$(dirname "$values")/presets" ] && [ -f "tests/cases/$chart/base.yaml" ]; then
      preset_args+=(-f "tests/cases/$chart/base.yaml")
    fi
    if [ -f "$(dirname "$values")/presets" ]; then
      while read -r preset; do
        [ -n "$preset" ] || continue
        if [ ! -f "charts/$chart/presets/$preset.yaml" ]; then
          echo "NO PRESET: $(dirname "$values")/presets names $preset"
          fail=1
        fi
        preset_args+=(-f "charts/$chart/presets/$preset.yaml")
      done < "$(dirname "$values")/presets"
    fi
    if ! out=$(helm template "$chart" "$chart_dir" --namespace example "${preset_args[@]}" -f "$values" 2>&1); then
      echo "DID NOT RENDER: $values"
      sed 's/^/    /' <<< "$out" | tail -5
      fail=1
      continue
    fi
    rendered=$((rendered + 1))
    # Every runner pod template must opt out of the service-account
    # automount, nixBuilders on or off: a job must not read a Kubernetes
    # API token. Counted per AutoscalingRunnerSet document.
    if [ "$chart" = arc-runners ]; then
      bad=$(awk -v RS='\n---\n' '/kind: AutoscalingRunnerSet/ && !/\n      automountServiceAccountToken: false\n/ {n++} END {print n+0}' <<< "$out")
      sets=$(grep -c '^kind: AutoscalingRunnerSet' <<< "$out")
      if [ "$bad" -ne 0 ] || [ "$sets" -eq 0 ]; then
        echo "RUNNER POD AUTOMOUNTS A TOKEN (or no runner set rendered): $values ($bad of $sets sets lack automountServiceAccountToken: false)"
        fail=1
      fi
    fi
    # A case named talos-* is the proof the chart works with no Karpenter
    # (and no EKS): its render must not mention either.
    case "$name" in talos*)
      if grep -qiE 'karpenter|eks\.amazonaws|nodepool' <<< "$out"; then
        echo "TALOS CASE MENTIONS KARPENTER/EKS: $values"
        grep -niE 'karpenter|eks\.amazonaws|nodepool' <<< "$out" | head -5 | sed 's/^/    /'
        fail=1
      fi ;;
    esac
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

# The runner nix ConfigMap is immutable and named by its content: with
# nixBuilders off, setting nixCache.url (which adds a substituters line to
# nix.conf) must move the name, or Argo CD tries to mutate the object in
# place. The `minimal` and `nix-cache` cases differ only in nixCache.url.
cm_name() {
  grep -m1 -E '^  name: runner-nix-config-' "$1" | awk '{print $2}'
}
a=$(cm_name tests/golden/arc-runners/minimal.yaml)
b=$(cm_name tests/golden/arc-runners/nix-cache.yaml)
if [ -z "$a" ] || [ -z "$b" ] || [ "$a" = "$b" ]; then
  echo "NIX CONFIGMAP NAME: minimal ($a) and nix-cache ($b) must differ -- nixCache.url is part of the immutable nix.conf"
  fail=1
fi

if [ "$rendered" -eq 0 ]; then
  echo "rendered nothing -- that is a failure, not a pass"
  exit 1
fi
if [ "$fail" -eq 0 ]; then
  echo "golden ($mode): $rendered renders, $refused fixtures refused"
fi
exit "$fail"
