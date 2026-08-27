# Day 2 — operating the CI plane

## Releasing

**A `v*` tag is the release act.** Tag creation is ruleset-restricted;
who can tag is who can publish. A patch tag on an unchanged tree is the
legitimate "pure refresh" release (base-image/CVE pickup — renovate has
already moved the pins on master).

```bash
git tag -a vX.Y.Z -m "..." && git push origin vX.Y.Z
```

The Release workflow does the rest: image-if-changed (else digest
reuse), digest stamped into charts, charts published. Semver intent:
patch = refresh/mechanics, minor = chart features or new tools in the
image, major = breaking values.

## The automatic chain (optional, two switches)

With renovate automerging pin bumps on master, the weekly
`auto-release` workflow can cut the patch tag itself. It is doubly
guarded and dark by default; enabling it takes:

1. the CI automation App's credentials on this repo
   (`CI_AUTOMATION_APP_ID` variable + `CI_AUTOMATION_PRIVATE_KEY`
   secret — at Truvity, set from the SSM mirror of the 1Password item);
2. the App as a bypass actor on the `v*` tag ruleset;
3. `gh variable set AUTO_RELEASE --body true`.

Why an App and not the built-in token: an App-pushed tag *triggers* the
Release workflow (a `GITHUB_TOKEN` push is inert), and the tagging
privilege stays one named, revocable actor instead of "every workflow
in the repo".

## Consumer-side promotion (the Truvity wiring, reusable anywhere)

The producer releases and stops. In the consuming gitops repo:

1. Annotate the pin —
   ```yaml
   # renovate: datasource=docker depName=ghcr.io/truvity/charts/ci-cache
   ciPlane: "1.0.3"
   ```
   One chart is the sentinel; both always share a version.
2. A renovate custom regex manager over that file, plus a packageRule
   with `automerge: true` and `postUpgradeTasks` running whatever
   regenerates derived files (renders, goldens) **inside the update
   branch**, so the PR is born green.
3. Hard-won renovate settings that make this work:
   - `platformCommit: "disabled"` — with an App token, renovate's
     GraphQL commit can fail with "unknown error" AFTER postUpgradeTasks
     succeed, silently dropping the branch; plain git commits are the
     provably-working path.
   - the shared renovate workflow must run renovate via **npx, not the
     docker-mode action** (daemonless runners), on **Node ≥ 24**
     (renovate 44 uses `RegExp.escape`), with `allowed-commands`
     matching the postUpgradeTasks commands EXACTLY.
   - the renovate App's variable/secret must actually be scoped to the
     repo — a missing entitlement skips silently.

## Rollback

Revert the consumer's pin PR. The previous charts and image digests are
immutable and still published; ArgoCD walks the fleet back on sync.
Never delete published versions.

## Upgrading the ARC controller

Bump the controller and the consumers' `arcVersion` value **in the same
change** — they must come from one pin. A mismatched `arcVersion` does
not degrade politely: the controller deletes the scale sets.

## Changing profiles / sizes

`scaleSets` map in `charts/arc-runners` values. Sizes are MEASURED, not
guessed: memory request == limit (no overcommit — ceilings cannot sum
past a node's allocatable), GOMAXPROCS pinned to the cgroup, no
ephemeral-storage request (breaks scale-from-zero) but a hard limit
(exceeding it must evict the pod, never the node). A runner killed at
its cgroup limit uploads no log and reads as a hang — err high.

## Cache operations

Every cache is disposable: delete the PVC, it re-warms. buildkitd's
warm layer lives in the registry (`cache-to type=registry`) and
survives builder replacement — expect one slower build cycle after a
PVC reset, not breakage. The nix cache and module proxy degrade to
upstream when down.

### npm read-through (verdaccio)

Adoption is one line: ARC-pooled repos pass `node-cache: true` to the
shared `check.yaml` (ci-workflows ≥ v2.13.0). The job probes
`npm-cache.ci-cache.svc/-/ping` (2s) and, when it answers, sets
`npm_config_registry` (npm, yarn classic) and `YARN_NPM_REGISTRY_SERVER`
(berry) for the job — a down cache degrades the job to *slow* (direct
npmjs, one warning line), never to *broken*. Both paths proven live
2026-08-27: override active on bar's ARC job; graceful fallback on a
hosted job.

Know your consumer before expecting traffic: **hosted runners cannot
reach the service at all** (public repos ride hosted — do not opt them
in, the warning is pure noise), and a **zero-install yarn repo (bar:
committed `.yarn/cache`) never fetches from any registry during
install** — the override there only catches ad-hoc fetches (`npx`,
`npm exec`, toolchain downloads honoring npm config). The cache earns
its keep when a non-zero-install Node repo lands on the ARC pool.
