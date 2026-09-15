# Architecture

The CI plane is three artifacts released as **one versioned unit**: the
runner image and the two Helm charts that surround it. This document
records the load-bearing decisions and why each one holds.

## One version, transitively digest-pinned

A `v*` tag releases everything. The release workflow:

1. builds the image **only if `image/` changed** since the previous
   release — otherwise the previous digest is re-tagged (a manifest
   copy, digest preserved);
2. stamps the resulting digest into both charts' default values;
3. publishes the charts at the tag's version.

A consumer therefore pins **one chart version** and is transitively
digest-pinned to the image. Chart-only patches never roll a runner
fleet (proven: v1.0.1 and v1.0.2 carry v1.0.0's image digest,
byte-identical). The stamp is the compatibility statement: chart X was
released against image X.

## Direct CRs, not the upstream scale-set chart

`charts/arc-runners` renders `AutoscalingRunnerSet` CRs directly rather
than depending on the upstream `gha-runner-scale-set` chart. The
controller and that chart share a version stream; bundling the chart
here would let the two drift apart independently. The CR API
(`actions.github.com/v1alpha1`) is the real contract, and the chart
mirrors what upstream renders: the CR, the per-scale-set manager
Role/RoleBinding for the controller SA, and the labels.

Two of those labels are load-bearing, learned the hard way:

- **`app.kubernetes.io/version` is a compatibility gate**: the
  controller DELETES any AutoscalingRunnerSet whose label differs from
  its build version — silently, seconds after apply. It is a REQUIRED
  value (`arcVersion`); feed it from the same pin that installs the
  controller so the two cannot drift.
- The `actions.github.com/values-hash` annotation changes whenever a
  set's rendered inputs change, which is what prompts the controller to
  recycle the listener — same mechanism as upstream.

## The release trigger is the last deliberate control point

With pull-based promotion automated downstream, tag creation is the one
remaining human-or-designated-actor decision. `v*` tags are
ruleset-restricted: a release team, plus (optionally) the CI automation
App as a bypass actor for the weekly auto-release. The App is a
dedicated identity rather than the built-in `github-actions` token for
three reasons: singular revocable control over release cadence, clean
attribution, and native event flow — an App-pushed tag *triggers* the
Release workflow, where a `GITHUB_TOKEN` push is inert (GitHub's
loop-protection), and that inertness is viral to every future
tag-reactive workflow.

## Promotion is the consumer's job

This repository releases and stops. There is no cross-repo reach: the
consumer's own renovate tracks the published chart version (one chart —
`charts/ci-cache` — serves as the sentinel, since both charts always
share a version), bumps its pin, regenerates any derived files inside
the update branch, and automerges on its own CI. See the Truvity wiring
in [day-2-operations.md](day-2-operations.md).

## The cache doctrine

Every `charts/ci-cache` component is a CACHE: disposable storage, zero
IAM where possible, no backups; losing one costs a re-warm. A dead
cache degrades to upstream — slower, never broken. buildkitd's PVC is
the HOT layer only; the WARM layer is `cache-to type=registry` and
survives builder replacement. Restrictive network defaults: only listed
namespaces reach the builders.

## Persistent Nix workers are CI-only and separate from BuildKit

The optional `nixWorkers` plane is one native StatefulSet per
architecture, each with its own RWO `/nix` store. It deliberately does
not reuse BuildKit pods or PVCs: rootless BuildKit and a privileged,
sandboxed multi-user Nix daemon have incompatible security models, and
sharing CPU, disk, GC and failure domains would let one cache evict or
stall the other.

The runner image also serves as the worker image when Kubernetes
overrides its command. An init container imports the image's Nix
closure into the persistent alternate-root store through Nix itself,
including the validity database; raw copying or mounting an empty PVC
over the baked store would produce an unusable image. The worker then
runs `nix-daemon` with build users and exposes only the legacy Nix SSH
store protocol. `sshd` forbids passwords, forwarding, TTYs and arbitrary
commands; its force-command accepts only `nix-store --serve` and
`nix-store --serve --write`.

The estate supplies only a stable server host key in `ci-cache`, public
`known_hosts` in each ARC namespace, the OpenBao HTTPS CA, and one or
more public SSH client-CA keys. Client private keys never enter Helm,
OpenBao KV, ESO or Kubernetes Secrets: each runner init generates an
Ed25519 key in its pod-local `emptyDir`, exchanges a narrowly projected
ServiceAccount JWT for a 72-hour OpenBao certificate, and publishes
the remote machines file only after validating that certificate. The
projected token is mounted only in init, never into workflow code.

OpenBao/network/signing failure publishes an empty machines file and
starts the runner normally, so local Nix remains the outage path. Stable
host verification is mandatory and independent from client-certificate
trust. NetworkPolicy admits only the listed runner namespaces. The
worker has no service-account token, but egress remains available because
fixed-output derivations must fetch sources; treat code admitted to this
CI plane as trusted and keep the privileged workers on a dedicated,
tainted build pool. Engineer keys, public ingress and workstation
integration are intentionally absent: these workers are shared CI
infrastructure, not developer machines.

ARC adds a machines file and `builders-use-substitutes`, but does not set
`max-jobs = 0`; local execution remains the outage fallback. The shared
stores are caches, not artifact authorities or backups. Access by two
organizations is an explicit shared-cache trust decision: either CI
identity can submit writes to the same architecture store.

## The image doctrine

The image contains only what devbox cannot deliver: the nix + devbox
bootstrap, bash-as-sh, the daemonless docker client + buildx,
go-cache-plugin, and the goreleaser-pro binary (its license key is a
secret and never ships). Everything else arrives per job from each
repository's own `devbox.json`. Repo- or cluster-specific content in
the image is a bug; the one documented debt is the baked in-cluster nix
substituter (dead elsewhere, upstream fallback).

Every version pin in the Dockerfile carries a `# renovate:` annotation
— a pin without one is invisible, and invisible is indistinguishable
from current. That includes the upstream runner base: `latest` was
replaced with an annotated pin precisely so CVE pickups become renovate
PRs instead of side effects.
