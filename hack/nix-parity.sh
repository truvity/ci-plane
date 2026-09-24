#!/usr/bin/env bash
# The runner and the nix worker must pin the SAME nix.
#
# Remote building is a protocol between two nix installations. While one
# manifest served both roles they matched by construction; splitting the
# worker into its own image (2026-09-24) removed that guarantee and put a
# pin in its place. This is what keeps the pin honest.
#
# It is worth a check of its own because of HOW a mismatch fails: not with
# a version error, but as a remote build that behaves oddly or refuses
# work for reasons that name neither image. Nobody reads two Dockerfiles
# side by side to find that.
#
# Also refuses an UNPINNED installer in either file. The shared image
# fetched nixos.org/nix/install -- whatever that served that day -- which
# was survivable only because one RUN line produced both halves.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

runner="$here/image/runner/Dockerfile"
worker="$here/image/nix-worker/Dockerfile"

fail=0

for f in "$runner" "$worker"; do
  [ -f "$f" ] || { echo "::error::missing $f"; exit 2; }
done

version_of() {
  # The DEFAULT on the global ARG is the pin; the bare `ARG NIX_VERSION`
  # inside the stage only brings it into scope.
  grep -oE '^ARG NIX_VERSION=[0-9][0-9.]*' "$1" | head -1 | cut -d= -f2
}

rv="$(version_of "$runner")"
wv="$(version_of "$worker")"

if [ -z "$rv" ] || [ -z "$wv" ]; then
  echo "::error::no pinned ARG NIX_VERSION in $( [ -z "$rv" ] && echo image/runner/Dockerfile) $( [ -z "$wv" ] && echo image/nix-worker/Dockerfile)"
  fail=1
elif [ "$rv" != "$wv" ]; then
  echo "::error::nix pins differ — runner $rv, worker $wv."
  echo "  Remote building is a protocol between these two installations;"
  echo "  a mismatch fails as a confusing build, not as a version error."
  fail=1
else
  echo "ok    both images pin nix $rv"
fi

# An unpinned installer would make the pin above decorative.
#
# Comments are stripped first: both files EXPLAIN the old unversioned URL,
# and the first version of this check flagged its own rationale. A guard
# that reads prose reports the documentation, not the build.
strip_comments() { sed -E 's/^[[:space:]]*#.*$//' "$1"; }

for f in "$runner" "$worker"; do
  if strip_comments "$f" | grep -q 'nixos\.org/nix/install'; then
    echo "::error::${f#"$here"/} fetches the UNVERSIONED installer; use releases.nixos.org/nix/nix-\${NIX_VERSION}/install"
    fail=1
  fi
done

# Both must actually USE the pin, not merely declare it.
for f in "$runner" "$worker"; do
  if ! strip_comments "$f" | grep -q 'nix-\${NIX_VERSION}/install'; then
    echo "::error::${f#"$here"/} declares NIX_VERSION but does not install that version"
    fail=1
  fi
done

[ "$fail" = 0 ] && echo "nix parity holds"
exit "$fail"
