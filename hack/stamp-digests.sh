#!/usr/bin/env bash
# Stamp release image digests into chart defaults, BY KEY.
#
#   hack/stamp-digests.sh runnerImage=sha256:… nixWorkerImage=sha256:…
#
# Until the nix worker got its own image the release did this with one
# sed over every `digest:` line in every chart, which was correct while
# there was exactly one image to stamp. With two it is actively wrong: it
# would write the RUNNER's digest into nixWorkerImage, and the worker
# would run the runner image again -- silently, because the value is a
# valid digest and the pods start.
#
# THIS SCRIPT IS INTERIM. The estate's public repositories package charts
# with helmctl from goreleaser's dist/artifacts.json, which needs no
# stamping at all. ci-plane cannot use that yet: helmctl reads and
# validates a single `images:` map, and these charts carry top-level
# `runnerImage:` / `nixWorkerImage:`. Adopting that convention is a
# breaking values change with consumers to move, so it is its own piece
# of work -- and this script is what keeps the release correct until
# then. Delete it when `chart-images: goreleaser` lands.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[ $# -gt 0 ] || { echo "usage: $0 <key>=<digest> [<key>=<digest> ...]" >&2; exit 2; }

fail=0

for pair in "$@"; do
  key="${pair%%=*}"
  digest="${pair#*=}"

  case "$digest" in
    sha256:[0-9a-f]*) ;;
    *) echo "::error::$key: $digest is not a sha256 digest"; fail=1; continue ;;
  esac

  found=0

  for f in "$here"/charts/*/values.yaml; do
    [ -f "$f" ] || continue

    # Track the current top-level mapping and rewrite only the digest
    # inside the right one. A key-blind rewrite is the bug this exists
    # to prevent.
    python3 - "$f" "$key" "$digest" <<'PY'
import re, sys
path, key, digest = sys.argv[1], sys.argv[2], sys.argv[3]
lines = open(path).read().split("\n")
cur, hit = None, False
for i, line in enumerate(lines):
    m = re.match(r'^([A-Za-z_][A-Za-z0-9_]*):\s*$', line)
    if m:
        cur = m.group(1)
        continue
    if line and not line[0].isspace() and not line.lstrip().startswith("#"):
        cur = None
    if cur == key:
        m = re.match(r'^(\s+digest:\s*)(?:"[^"]*"|\S*)(\s*#.*)?$', line)
        if m:
            lines[i] = f'{m.group(1)}"{digest}"{m.group(2) or ""}'
            hit = True
open(path, "w").write("\n".join(lines))
sys.exit(0 if hit else 3)
PY
    case $? in
      0) found=$((found + 1)); echo "ok    $key -> ${digest:0:19}… in ${f#"$here"/}" ;;
      3) ;;
      *) echo "::error::$key: rewriting ${f#"$here"/} failed"; fail=1 ;;
    esac
  done

  if [ "$found" = 0 ]; then
    echo "::error::$key: no chart declares it — a release would publish charts with an unstamped image"
    fail=1
  fi
done

[ "$fail" = 0 ] && echo "digests stamped"
exit "$fail"
