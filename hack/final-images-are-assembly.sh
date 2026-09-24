#!/usr/bin/env bash
# The release images must ASSEMBLE, never EXECUTE.
#
# Every instruction that has to run lives in the per-architecture base
# images, built natively on their own runner. The release Dockerfiles
# under image/release/ only `FROM` a manifest list and add labels, so
# buildx resolves each platform without emulating anything.
#
# Measured on the same image: a RUN-less arm64 stage is "DONE 0.0s"; one
# `apk add` under QEMU is two seconds -- and the real work here is a nix
# install, a devbox install, apt and the docker toolchain. So a single
# `RUN` added to a release Dockerfile silently puts the whole arm64 half
# of every release back under emulation. It would still be correct, and
# still be green, which is exactly why it needs a guard rather than a
# convention.
#
# Instructions are counted, not the WORD: these files explain themselves
# at length and say "RUN" in prose several times. A guard that reads its
# own rationale reports the documentation instead of the build -- which
# the first version of hack/nix-parity.sh did, on this same repository,
# an hour before this was written.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dir="$here/image/release"

[ -d "$dir" ] || { echo "::error::no $dir"; exit 2; }

# Anything that executes during a build, or that would need the target
# platform's userland to make sense.
forbidden='RUN|SHELL|HEALTHCHECK'

fail=0
checked=0

for f in "$dir"/*.Dockerfile; do
  [ -f "$f" ] || continue
  checked=$((checked + 1))
  rel="${f#"$here"/}"

  # Strip comments and blank lines, then look at the INSTRUCTION word.
  hits=$(sed -E 's/^[[:space:]]*#.*$//' "$f" \
         | grep -nEi "^[[:space:]]*(${forbidden})[[:space:]]" || true)

  if [ -n "$hits" ]; then
    echo "::error::$rel executes during the build; the base image is where that belongs:"
    echo "$hits" | sed 's/^/      /'
    fail=1
  else
    echo "ok    $rel assembles only"
  fi

  # A release image that does not start FROM a base is not this pattern.
  if ! sed -E 's/^[[:space:]]*#.*$//' "$f" | grep -qE '^[[:space:]]*FROM[[:space:]]'; then
    echo "::error::$rel has no FROM"
    fail=1
  fi
done

if [ "$checked" = 0 ]; then
  echo "::error::no release Dockerfiles found — the guard checked nothing"
  exit 2
fi

[ "$fail" = 0 ] && echo "release images assemble only ($checked checked)"
exit "$fail"
