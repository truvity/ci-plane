#!/usr/bin/env bash
# Every `# renovate:` annotation in an image Dockerfile must sit in a file
# the custom managers actually match.
#
# WHY this exists. The annotations carry the version pins Renovate cannot
# find on its own -- `ARG RUNNER_VERSION=`, `go install ...@v0.1.1` -- and
# the managers find them by PATH. On 2026-09-24 the runner and nix-worker
# images split into image/runner/ and image/nix-worker/, and
# managerFilePatterns still said `image/Dockerfile`. Eight pins went
# unwatched, including the actions-runner, docker, devbox and nix
# versions, and nothing anywhere failed: Renovate simply opened no pull
# requests, which is indistinguishable from "everything is current".
#
# That is the shape of the bug this guards. A literal path in a config
# stops matching the day a file moves, and the symptom is silence.
#
# NOT the charts. charts/*/values.yaml also carries `# renovate:` lines,
# but those are the standard helm-values manager's, inherited from the
# ci-workflows preset -- a different mechanism with a different path
# surface. Checking them here would mean reimplementing that preset's
# matching and would fail on a config this repository does not own.
set -euo pipefail

cd "$(dirname "$0")/.."

python3 - <<'PY'
import fnmatch, json, pathlib, re, sys

cfg = json.loads(pathlib.Path("renovate.json").read_text())

patterns = []
for m in cfg.get("customManagers", []):
    patterns.extend(m.get("managerFilePatterns", []))

if not patterns:
    sys.exit("renovate.json declares no customManagers managerFilePatterns")


def matches(path: str) -> bool:
    for p in patterns:
        # Renovate reads a pattern wrapped in slashes as a regex and
        # anything else as a glob. Both forms are honoured so this does
        # not quietly pass a config written in the other style.
        if len(p) > 1 and p.startswith("/") and p.endswith("/"):
            if re.search(p[1:-1], path):
                return True
        elif fnmatch.fnmatch(path, p):
            return True
    return False


annotated = []
for f in sorted(pathlib.Path("image").rglob("*")):
    if not f.is_file():
        continue
    if f.name != "Dockerfile" and not f.name.endswith(".Dockerfile"):
        continue
    # An annotation that pins something, not the prose ABOUT annotations.
    pins = re.findall(r"^\s*# renovate: datasource=", f.read_text(), re.M)
    if pins:
        annotated.append((f.as_posix(), len(pins)))

# A guard that scanned nothing must not report success. If the image
# tree is ever restructured out from under this, that is the same class
# of failure it exists to catch.
if not annotated:
    sys.exit("no annotated Dockerfile found under image/ — this guard swept nothing")

bad = [(f, n) for f, n in annotated if not matches(f)]

for f, n in annotated:
    print(f"{'MISS' if (f, n) in bad else ' ok '}  {f}  ({n} pins)")

if bad:
    print()
    print("renovate.json managerFilePatterns: " + ", ".join(patterns), file=sys.stderr)
    for f, n in bad:
        print(f"::error::{f} carries {n} renovate pins that no custom manager matches", file=sys.stderr)
    sys.exit(1)

total = sum(n for _, n in annotated)
print(f"\n{len(annotated)} Dockerfile(s), {total} pins, all matched")
PY
