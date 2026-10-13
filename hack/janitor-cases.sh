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

# ── the readiness verdict (runner.watchdog) ─────────────────────────────
#
# The same sweep, rendered with the defaults (watchdog OFF): the criterion
# is gated on the PODS, not on a chart value, so it must match nothing
# where no runner container has a readiness probe -- and the cases below
# prove both halves. A runner's own watchdog reports STUCK as a failing
# readiness probe; the janitor deletes the runner once the pod has been
# NotReady for notReadySeconds (60), without needing the signature in the
# last 200 log lines. What it must still leave alone: a runner holding a
# job (whatever its readiness), a ready one, a container that is not
# running (waiting / crash-looping is not the watchdog's verdict), one
# NotReady for less than notReadySeconds, and one with no readiness probe
# at all.
ago() { date -u -d "@$((now - $1))" +%Y-%m-%dT%H:%M:%SZ; }
t120=$(ago 120)
t30=$(ago 30)

cat > "$work/runners-wd.json" <<JSON
{"items":[
 {"metadata":{"name":"wd-stuck","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-stuck-young","creationTimestamp":"$(ago 150)"},"status":{}},
 {"metadata":{"name":"wd-ready","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-busy-notready","creationTimestamp":"$old"},"status":{"jobRepositoryName":"example/repo"}},
 {"metadata":{"name":"wd-waiting","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-flipping","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-noprobe","creationTimestamp":"$old"},"status":{}},
 {"metadata":{"name":"wd-old-image","creationTimestamp":"$old"},"status":{}}
]}
JSON
running='"state":{"running":{"startedAt":"'"$old"'"}}'
probe='"spec":{"containers":[{"name":"runner","readinessProbe":{"httpGet":{"path":"/healthz","port":"watchdog"}}}]}'
noprobe='"spec":{"containers":[{"name":"runner"}]}'
cond() { printf '"conditions":[{"type":"Ready","status":"%s","lastTransitionTime":"%s"}]' "$1" "$2"; }
cat > "$work/pods-wd.json" <<JSON
{"items":[
 {"metadata":{"name":"wd-stuck"},$probe,"status":{"containerStatuses":[{"name":"runner","ready":false,$running}],$(cond False "$t120")}},
 {"metadata":{"name":"wd-stuck-young"},$probe,"status":{"containerStatuses":[{"name":"runner","ready":false,$running}],$(cond False "$t120")}},
 {"metadata":{"name":"wd-ready"},$probe,"status":{"containerStatuses":[{"name":"runner","ready":true,$running}],$(cond True "$t120")}},
 {"metadata":{"name":"wd-busy-notready"},$probe,"status":{"containerStatuses":[{"name":"runner","ready":false,$running}],$(cond False "$t120")}},
 {"metadata":{"name":"wd-waiting"},$probe,"status":{"containerStatuses":[{"name":"runner","ready":false,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}],$(cond False "$t120")}},
 {"metadata":{"name":"wd-flipping"},$probe,"status":{"containerStatuses":[{"name":"runner","ready":false,$running}],$(cond False "$t30")}},
 {"metadata":{"name":"wd-noprobe"},$noprobe,"status":{"containerStatuses":[{"name":"runner","ready":false,$running}],$(cond False "$t120")}},
 {"metadata":{"name":"wd-old-image"},$noprobe,"status":{"containerStatuses":[{"name":"runner","ready":true,$running}],$(cond True "$t120")}}
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
PATH="$work/bin:$PATH" sh "$work/sweep.sh" > "$work/out-wd" 2>&1

wd_acted_on() { grep -q "$1" "$work/calls-wd"; }

if wd_acted_on 'delete ephemeralrunner .* wd-stuck '; then
  ok "watchdog: an idle runner NotReady for >= 60s is deleted"
else
  bad "watchdog: an idle runner NotReady for >= 60s is deleted"
fi
# Younger than the 180s age grace but NotReady for 120s: the shorter clock.
if wd_acted_on 'delete ephemeralrunner .* wd-stuck-young '; then
  ok "watchdog: the 60s NotReady clock is not held back by the 180s age grace"
else
  bad "watchdog: the 60s NotReady clock is not held back by the 180s age grace"
fi
for keep in wd-ready wd-busy-notready wd-waiting wd-flipping wd-noprobe; do
  if wd_acted_on "$keep"; then
    bad "watchdog: $keep is left alone"
  else
    ok "watchdog: $keep is left alone"
  fi
done
if wd_acted_on 'delete ephemeralrunner .* wd-old-image '; then
  ok "watchdog: the log-signature sweep still runs as the backstop"
else
  bad "watchdog: the log-signature sweep still runs as the backstop"
fi
if grep -q 'sweep complete (3 released)' "$work/out-wd"; then
  ok "watchdog: the summary counts what it actually did"
else
  bad "watchdog: the summary counts what it actually did: $(grep 'sweep complete' "$work/out-wd")"
fi

echo
if [ "$fail" = 0 ]; then
  echo "janitor holds (15 cases checked)"
else
  echo "::error::janitor cases failed"
fi

exit "$fail"
