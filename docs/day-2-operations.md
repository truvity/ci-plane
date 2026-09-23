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

1. a way for the job to get the CI automation App's token. This repo
   uses the keyless one: `auto-release.yaml` exchanges the run's own
   GitHub OIDC identity at the access-roster issuer for an installation
   token of the catalogue App (`token-source: access-roster`,
   `github-app: truvity-ci-automation`), so what has to be in place is
   `vars.ACCESS_ROSTER_ISSUER` and an issuer grant covering this
   repository's auto-release job. **No App private key lives on this
   repo**: the `CI_AUTOMATION_PRIVATE_KEY` secret was deleted on
   2026-09-18 when the exchange went live, and the
   `CI_AUTOMATION_APP_ID` variable that outlived it is read by nothing.
   Do not recreate either — an empty exchange is a configuration fix at
   the issuer, never a new key here. (The shared workflow still offers
   the classic `app-key` source, App id variable + private-key secret,
   for callers with no issuer to exchange against.)
2. the App as a bypass actor on the `v*` tag ruleset;
3. `gh variable set AUTO_RELEASE --body true`.

Why an App and not the built-in token: an App-pushed tag *triggers* the
Release workflow (a `GITHUB_TOKEN` push is inert), and the tagging
privilege stays one named, revocable actor instead of "every workflow
in the repo". Why an exchange and not the App's key: the token is
minted per run and narrowed to this repository, so there is nothing
stored to rotate or leak, and taking the privilege away is a grant edit
at the issuer rather than a secret deletion in every repo that tags.

## Consumer-side promotion (the Truvity wiring, reusable anywhere)

The producer releases and stops. In the consuming gitops repo:

1. Annotate the pin —
   ```yaml
   # renovate: datasource=docker depName=ghcr.io/truvity/charts/ci-builders
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

## Persistent Nix worker operations

The workers are CI-only. Diagnose them from an in-cluster Job or a
manually dispatched ARC workflow. Each private key is unique to one pod
and never persisted in a Kubernetes Secret, but trusted workflow code
can read and copy it from `/home/runner/.ssh`, and read the projected
token the job-started hook signs with; namespace ingress, the one-hour
certificate lifetime and the single-job pod bound that accepted CI-only
risk. A useful smoke builds one tiny derivation on each advertised
system, repeats it to prove store reuse, rejects an arbitrary SSH
command, and then makes OpenBao unavailable to prove local fallback.

Each architecture owns a disposable persistent store. Deleting its PVC
loses reuse but not source artifacts; the init container reseeds the Nix
runtime closure and later CI jobs warm it again. Worker readiness checks
the daemon socket, daemon PID, sshd PID, trusted client CA and local SSH
port. An SSH connection is not sufficient proof: verify a remote
derivation and copied-back output.

### Signer outage and authorization failures

Signing runs twice per runner: in the init container for a cold pod and
in the `ACTIONS_RUNNER_HOOK_JOB_STARTED` script when the job starts. Both
write an empty `/etc/nix-builders/machines` before contacting OpenBao.
DNS, TLS, login or signing failure logs one warning, removes any staged
key and exits successfully; the job then builds locally. In init, a
missing CA/config/known-hosts mount is a deployment error and correctly
blocks the pod rather than silently weakening trust. In the hook nothing
fails the job: any error, including a crash or the two-minute timeout,
empties the machines file and removes the key. The job log's
"job started" hook step says which way it went (`remote builders
enabled` or `Nix builds locally`).

Each organization's login role is independently revocable. Removing one
role or its sign-only policy prevents new certificates immediately, but
already-issued certificates remain valid for at most one hour. Removing
the CA public key from workers revokes every organization immediately
and is therefore a break-glass action.

### SSH user-CA rotation

Never replace the CA in place. Stand up the new CA (for example a second
SSH mount) and export both public keys. Workers first roll to trust
both; then runner configuration (`sshMount`, `sshCAPublicKeys`) moves
signing to the new one. Recycle all warm runners or wait the one-hour
maximum before removing the old public key from the workers.

### Host-key rotation

Host rotation is independent from the client CA and affects both
organizations. It requires a maintenance window because sshd loads its
host private key only at process start. First add the new host public key
alongside the old one in both namespace-local `known_hosts` values,
bump `nixBuilders.knownHosts.revision`, and verify ARC has recycled every
warm runner before changing the server key. Then update the server host
key, sync its Secret,
and restart both worker StatefulSets. Smoke both organizations and
architectures before removing the old `known_hosts` entries. Roll back by
restoring the previous server key and restarting both StatefulSets.

## npm read-through (verdaccio)

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
