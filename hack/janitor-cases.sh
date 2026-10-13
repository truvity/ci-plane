#!/usr/bin/env bash
# The janitor's sweep, EXECUTED against a stub kubectl.
#
# What is tested is the script a CronJob would actually run: it is
# extracted from the RENDERED chart, so a template that stops producing it
# -- or produces it with the indentation of its heredocs wrong, which is a
# YAML error and not a shell one -- fails here rather than in a cluster at
# 3am.
#
# It deletes and patches live objects, so both ways of being wrong are
# expensive. Too eager and it releases a finalizer on a runner that was
# midway through an orderly deletion; too shy and the estate's CI stops
# until somebody notices and clears it by hand, which is what happened on
# 2026-09-24.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

for t in helm jq python3; do
  command -v "$t" >/dev/null || { echo "$t is required" >&2; exit 2; }
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

helm template t "$here/charts/arc-runners" \
  --set githubConfigUrl=https://github.com/example \
  --set controllerServiceAccount.namespace=arc-system \
  --set janitor.enabled=true > "$work/rendered.yaml" || {
    echo "::error::the chart did not render" >&2; exit 1; }

python3 - "$work/rendered.yaml" "$work/sweep.sh" <<'PY'
import sys, yaml
src, dst = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(src)):
    if d and d.get("kind") == "ConfigMap" and "sweep.sh" in d.get("data", {}):
        open(dst, "w").write(d["data"]["sweep.sh"])
        sys.exit(0)
print("no ConfigMap carrying sweep.sh in the rendered chart", file=sys.stderr)
sys.exit(1)
PY
[ -s "$work/sweep.sh" ] || exit 1

# The script is POSIX sh, not bash: the janitor image is Alpine.
sh -n "$work/sweep.sh" || { echo "::error::rendered sweep is not valid sh" >&2; exit 1; }

now=$(date -u +%s)
old=$(date -u -d "@$((now - 3600))" +%Y-%m-%dT%H:%M:%SZ)
recent=$(date -u -d "@$((now - 10))" +%Y-%m-%dT%H:%M:%SZ)

# The five shapes that matter, in one listing so the sweeps are exercised
# against each other rather than in isolation.
cat > "$work/items.json" <<JSON
{"items":[
 {"metadata":{"name":"wedged-old","creationTimestamp":"$old","deletionTimestamp":"$old","finalizers":["ephemeralrunner.actions.github.com/finalizer","ephemeralrunner.actions.github.com/runner-registration-finalizer"]},"status":{"jobRepositoryName":"example/repo"}},
 {"metadata":{"name":"deleting-recent","creationTimestamp":"$old","deletionTimestamp":"$recent","finalizers":["ephemeralrunner.actions.github.com/finalizer"]},"status":{"jobRepositoryName":"example/repo"}},
 {"metadata":{"name":"deleting-nofinalizer","creationTimestamp":"$old","deletionTimestamp":"$old"},"status":{"jobRepositoryName":"example/repo"}},
 {"metadata":{"name":"healthy-busy","creationTimestamp":"$old"},"status":{"jobRepositoryName":"example/repo"}},
 {"metadata":{"name":"idle-old","creationTimestamp":"$old"},"status":{}}
]}
JSON

mkdir -p "$work/bin"
cat > "$work/bin/kubectl" <<EOF
#!/bin/sh
case "\$1" in
  get)  cat "$work/items.json" ;;
  logs) echo "Job message already acquired" ;;
  *)    echo "\$*" >> "$work/calls" ;;
esac
EOF
chmod +x "$work/bin/kubectl"
: > "$work/calls"

PATH="$work/bin:$PATH" sh "$work/sweep.sh" > "$work/out" 2>&1

fail=0
ok()  { printf 'ok    %s\n' "$1"; }
bad() { printf 'FAIL  %s\n' "$1"; fail=$((fail + 1)); }

acted_on() { grep -q "$1" "$work/calls"; }

# ── the wedged runner, which is the whole reason for the second sweep ────
if acted_on 'patch ephemeralrunner .* wedged-old'; then
  ok "a deletion wedged past the grace period has its finalizers released"
else
  bad "a deletion wedged past the grace period has its finalizers released"
fi

# ── and the three that must NOT be touched ───────────────────────────────
# An ordinary delete completes in well under a second. Releasing its
# finalizer would race the controller's own cleanup.
if acted_on 'deleting-recent'; then
  bad "a deletion younger than the grace period is left alone"
else
  ok "a deletion younger than the grace period is left alone"
fi

# Nothing to release; patching it would be a write for no reason.
if acted_on 'deleting-nofinalizer'; then
  bad "a deletion with no finalizers is left alone"
else
  ok "a deletion with no finalizers is left alone"
fi

# The one that would be catastrophic: a runner doing real work.
if acted_on 'healthy-busy'; then
  bad "a healthy runner holding a job is never touched"
else
  ok "a healthy runner holding a job is never touched"
fi

# ── the original sweep still works ───────────────────────────────────────
if acted_on 'delete ephemeralrunner .* idle-old'; then
  ok "an idle runner matching the stuck signature is still deleted"
else
  bad "an idle runner matching the stuck signature is still deleted"
fi

# ── the count is real ────────────────────────────────────────────────────
# Both loops are fed by heredoc rather than by a pipe for exactly this: a
# piped `while` runs in a subshell, so the counter would be incremented in
# a process that exits before anything reads it and the total would always
# be 0 -- a number that is worse than no number, because it reads as an
# answer.
if grep -q 'sweep complete (2 released)' "$work/out"; then
  ok "the summary counts what it actually did"
else
  bad "the summary counts what it actually did: $(grep 'sweep complete' "$work/out")"
fi

# ── runner.watchdog on: the readiness verdict ───────────────────────────
#
# The same sweep, rendered with runner.watchdog.enabled. A runner's own
# watchdog reports STUCK as a failing readiness probe; the janitor reads
# that from the pods and deletes the runner without needing the signature
# in the last 200 log lines. What it must still leave alone: a runner
# holding a job (whatever its readiness), a ready one, a container that is
# not running (waiting / crash-looping is not the watchdog's verdict), and
# one younger than the grace period.
helm template t "$here/charts/arc-runners" \
  --set githubConfigUrl=https://github.com/example \
  --set controllerServiceAccount.namespace=arc-system \
  --set janitor.enabled=true \
  --set runner.watchdog.enabled=true > "$work/rendered-wd.yaml" || {
    echo "::error::the chart did not render with runner.watchdog.enabled" >&2; exit 1; }
python3 - "$work/rendered-wd.yaml" "$work/sweep-wd.sh" <<'PY'
import sys, yaml
src, dst = sys.argv[1], sys.argv[2]
for d in yaml.safe_load_all(open(src)):
    if d and d.get("kind") == "ConfigMap" and "sweep.sh" in d.get("data", {}):
        open(dst, "w").write(d["data"]["sweep.sh"])
        sys.exit(0)
sys.exit(1)
PY
sh -n "$work/sweep-wd.sh" || { echo "::error::rendered watchdog sweep is not valid sh" >&2; exit 1; }

cat > "$work/runners-wd.json" <<JSON
{"items":[
 {"metadata":{"name":"wd-stuck","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-ready","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-busy-notready","creationTimestamp":"$old"},"status":{"jobRepositoryName":"example/repo"}},
 {"metadata":{"name":"wd-waiting","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-young-notready","creationTimestamp":"$recent"},"status":{}},
 {"metadata":{"name":"wd-old-image","creationTimestamp":"$old"},"status":{}}
]}
JSON
running='"state":{"running":{"startedAt":"'"$old"'"}}'
cat > "$work/pods-wd.json" <<JSON
{"items":[
 {"metadata":{"name":"wd-stuck"},"status":{"containerStatuses":[{"name":"runner","ready":false,$running}]}},
 {"metadata":{"name":"wd-ready"},"status":{"containerStatuses":[{"name":"runner","ready":true,$running}]}},
 {"metadata":{"name":"wd-busy-notready"},"status":{"containerStatuses":[{"name":"runner","ready":false,$running}]}},
 {"metadata":{"name":"wd-waiting"},"status":{"containerStatuses":[{"name":"runner","ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"name":"wd-young-notready"},"status":{"containerStatuses":[{"name":"runner","ready":false,$running}]}},
 {"metadata":{"name":"wd-old-image"},"status":{"containerStatuses":[{"name":"runner","ready":true,$running}]}}
]}
JSON

# get pods / get ephemeralrunners answer from their own listings; only
# wd-old-image carries the signature in its log (the fallback sweep).
cat > "$work/bin/kubectl" <<STUB
#!/bin/sh
case "\$1" in
  get)
    case "\$2" in
      pods) cat "$work/pods-wd.json" ;;
      *)    cat "$work/runners-wd.json" ;;
    esac ;;
  logs)
    [ "\$4" = wd-old-image ] && echo "Skipping message Job. Job message already acquired 'x'. job assignment is invalid: MissingKey"
    exit 0 ;;
  *) echo "\$*" >> "$work/calls-wd" ;;
esac
STUB
chmod +x "$work/bin/kubectl"
: > "$work/calls-wd"
PATH="$work/bin:$PATH" sh "$work/sweep-wd.sh" > "$work/out-wd" 2>&1

wd_acted_on() { grep -q "$1" "$work/calls-wd"; }

if wd_acted_on 'delete ephemeralrunner .* wd-stuck '; then
  ok "watchdog: an idle runner whose runner container is NotReady is deleted"
else
  bad "watchdog: an idle runner whose runner container is NotReady is deleted"
fi
for keep in wd-ready wd-busy-notready wd-waiting wd-young-notready; do
  if wd_acted_on "$keep"; then
    bad "watchdog: $keep is left alone"
  else
    ok "watchdog: $keep is left alone"
  fi
done
if wd_acted_on 'delete ephemeralrunner .* wd-old-image '; then
  ok "watchdog: the log-signature sweep still runs as the fallback"
else
  bad "watchdog: the log-signature sweep still runs as the fallback"
fi
if grep -q 'sweep complete (2 released)' "$work/out-wd"; then
  ok "watchdog: the summary counts what it actually did"
else
  bad "watchdog: the summary counts what it actually did: $(grep 'sweep complete' "$work/out-wd")"
fi

echo
if [ "$fail" = 0 ]; then
  echo "janitor holds (13 cases checked)"
else
  echo "::error::janitor cases failed"
fi

exit "$fail"
