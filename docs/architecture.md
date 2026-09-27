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

## Runners pack; they do not spread

Left to the default scheduler, runners SPREAD. `NodeResourcesFit`
scores with `LeastAllocated`, and balanced allocation agrees with it, so
of all the nodes a runner fits, it lands on the emptiest. Bigger nodes
do not change this: a pool of nodes that each hold several runners still
ends up with one idle warm runner per node. And the spread cannot be
undone afterwards, because runners carry `karpenter.sh/do-not-disrupt`
and the autoscaler may not move them to empty a node. On a managed
control plane the scheduler profile is out of reach, so the fix is in
the pod spec.

`packing.enabled` (the default) gives every runner pod the chart-owned
label `ci-plane.io/packing-group: runners` and a **preferred** podAffinity
toward it, keyed on `kubernetes.io/hostname`, with `namespaceSelector: {}`.
The label is the chart's own rather than one the controller stamps,
because a controller label that changes between ARC versions would turn
packing back into spreading with no error anywhere. The empty namespace
selector matters just as much: a pod affinity term without one only
looks at the pod's own namespace, and each organization's release is its
own namespace. With it, every size, scale set and organization is one
packing group on the shared pool.

It is a preference, never a requirement: a runner that fits no runner
node goes to an empty node or a new one, exactly as before. A caller's
own `affinity` is kept, and the packing term is appended to it.
`hack/packing-cases.sh` asserts all of this on the rendered chart, since
a broken preference fails nothing: the runners would simply spread
again.

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
`charts/ci-builders` — serves as the sentinel, since both charts always
share a version), bumps its pin, regenerates any derived files inside
the update branch, and automerges on its own CI. See the Truvity wiring
in [day-2-operations.md](day-2-operations.md).

## The cache doctrine

Every `charts/ci-builders` component is disposable: no backups, zero
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
more public keys of the environment's SSH user CA (exported, for
example, as `sshUserCaPublicKeys`; the workers trust the same keys).
Client private keys never enter Helm, OpenBao KV, ESO or Kubernetes
Secrets: the runner generates an Ed25519 key in its pod-local
`emptyDir`, exchanges a narrowly projected ServiceAccount JWT for an
OpenBao SSH user certificate of at most one hour, and publishes the
remote machines file only after validating that certificate: the one
principal, the expected CA, the generated key, the requested lifetime,
no critical options and no extension except `permit-pty` (signing roles
commonly add it; the workers' `PermitTTY no` makes it inert).

**Signing is per job, not per pod.** One hour is the signing role's
ceiling and a warm runner idles far longer, so a certificate signed when
the pod started would be expired by the time its job runs. The init
container signs for a cold pod; an `ACTIONS_RUNNER_HOOK_JOB_STARTED`
script re-runs the same setup as `runner` before the job's first step.
That is why the runner container mounts the projected token and why its
`~/.ssh` and `/etc/nix-builders` are writable `emptyDir`s: workflow code
can read the token and the key, and could sign certificates of its own
for the life of its pod. Namespace-only worker ingress, the one-hour
certificate and the ephemeral single-job pod are the boundary; the token
carries only the SSH signing policy. A job that runs longer than the
certificate keeps building locally once its remote connections need a
fresh certificate.

OpenBao/network/signing failure publishes an empty machines file and
starts the runner (or the job) normally, so local Nix remains the outage
path. Stable host verification is mandatory and independent from
client-certificate trust. NetworkPolicy admits only the listed runner
namespaces. The worker has no service-account token, but egress remains
available because fixed-output derivations must fetch sources; treat
code admitted to this CI plane as trusted and keep the privileged
workers on a dedicated, tainted build pool. Engineer keys, public
ingress and workstation integration are intentionally absent: these
workers are shared CI infrastructure, not developer machines.

ARC adds a machines file and `builders-use-substitutes`, but does not set
`max-jobs = 0`; local execution remains the outage fallback. The shared
stores are caches, not artifact authorities or backups. Access by two
organizations is an explicit shared-cache trust decision: either CI
identity can submit writes to the same architecture store.

### Worker host certificates (phase 1)

The pinned-host-key path above (`ssh.existingSecret`) does not go away;
`nixWorkers.hostCertificate` is an ADDITIVE alternative, off by default.
Enabling it signs the worker's own `ssh_host_ed25519_key` with an OpenBao
SSH secrets engine — `cert_type=host`, `valid_principals` set to the
worker's own DNS name, COMPUTED by the chart from the release namespace
(`nix-worker-<arch>.<namespace>.svc.cluster.local`) rather than taken as
a raw value, so a public repository never invites an estate-specific
hostname into a values file. A client that trusts the signing CA's public
key as an `@cert-authority` line (`arc-runners`'
`nixBuilders.knownHosts.certAuthorities`) then trusts every worker that
CA signs for, instead of pinning one host key Secret per architecture —
both mechanisms stay valid at once during a migration.

Signing and renewal happen in the SAME container as sshd, not a separate
init container: `nix-worker-entrypoint.sh` signs once before starting
sshd (**fail-closed** — a failure here exits the container, because a
worker's host identity has no degraded mode a client can fall back to
mid-connection, unlike the runner-side user certificates below, which
fail open) and then runs a background loop that re-signs every
`renewEvery` (must stay under `certificateTTL`, checked at render time)
and sends sshd SIGHUP. OpenSSH re-execs itself on SIGHUP, re-reading
`sshd_config` and reloading a replaced `HostCertificate` from disk
without dropping an already-established session — verified directly
against this image's `sshd_config` (same PID throughout; the presented
certificate's Key ID changed after the signal). A renewal failure logs
and retries with backoff; it never tears down a running sshd whose
current certificate is still valid.

Signing is a SIBLING binary to the runner-side `nix-worker-client`
(`image/nix-worker-host-cert`), not a mode flag on it: the two have
unrelated validation policies (host vs. user certificates, exact
principals vs. one fixed principal, no extensions at all vs.
permit-pty-only), unrelated failure contracts (fail-closed here,
fail-open there) and unrelated deployment targets (a persistent worker
pod vs. an ephemeral runner pod). See that binary's own top-of-file
comment for the full reasoning.

A second SSH login, `nix`, exists in the image alongside `nixremote`'s
legacy `nix-store --serve`: the modern ssh-ng protocol via `nix-daemon
--stdio`, gated by `AuthorizedPrincipalsFile` content the chart renders
from `nixWorkers.accounts.nix.principals` (empty by default — nobody).
`sshd_config`'s `Match User nix` block is baked into every image
unconditionally; whether the account is ever reachable is decided by
whether `AllowUsers` gains `nix`, which `nix-worker-entrypoint.sh`
computes at container start from that principals file. `trusted-users`
in the Nix daemon config gains `nix` only when
`nixWorkers.accounts.nix.trusted` is set — trusted users can import
store paths WITHOUT signature verification, which is what Nix's
sandboxing model exists to prevent, so it is off by default.

### Runners over ssh-ng (phase 2)

Phase 1 above built the worker's side of a second login; this phase is
the runner's side of actually using it, and both are configurable, not
mutually exclusive. `arc-runners`' `nixBuilders.login.{user, principal,
protocol}` chooses the machine: today's unchanged default is the legacy
`nixremote` account and `ssh://` (`nix-store --serve`); a cut-over sets
`user: nix`, a `principal` that is in the worker's
`nixWorkers.accounts.nix.principals` list (not necessarily `nix` itself
— `AuthorizedPrincipalsFile` can require a narrower name than the
account), and `protocol: ssh-ng`. `nixBuilders.openbao.sshRole` then
names whichever signing role issues for that principal — there is no
second role value, because the cut-over is which role `sshRole` points
at, not a parallel setting.

`login.user` absorbed the chart's former top-level `sshUser`, kept as a
DEPRECATED, non-breaking alias (removal in a later release) rather than
a hard rename: an existing installation that already sets `sshUser` — and
is auto-promoted onto new chart releases without a values change of its
own — must keep rendering exactly what it rendered before. Setting only
`sshUser` (its default value or a custom one) is honoured as `login.user`;
setting both to the SAME value is fine either way; setting both to
DIFFERENT non-default values is a render-time failure rather than a
silent pick of one (`arc-runners.nixBuilderUser` in `_helpers.tpl`). New
installs should set `login.user` alone.

`nix-worker-client` validates a returned certificate against the
configured principal and protocol exactly as it always validated against
the fixed `nixremote` principal: one principal, no extension but
`permit-pty`. Under `ssh-ng` it additionally allows ONE critical option,
`force-command`, and only when its value is exactly `nix-daemon --stdio`
— what the worker's `Match User nix` block already forces server-side
(`image/sshd_config`). This exists because OpenBao/Vault SSH roles apply
their own `default_critical_options` regardless of what the request
names, and this role's grant refuses a request that names
`critical_options` at all, so the client can neither ask for
`force-command` nor suppress it — it can only check what came back. A
certificate forcing anything else is refused before any credential is
published, the same fail-open contract as every other rejection here:
local Nix builds remain available.

`nixBuilders.knownHosts.pinned` (default `true`) is independent of the
login: it decides whether the runner's `known_hosts` still pins each
worker's static key (today's Secret) or is built ENTIRELY from
`certAuthorities` (the phase 1 host-certificate CA line) when set to
`false`. Setting it `false` with an empty `certAuthorities` list is
refused at render time — that combination would trust nothing.

**Verified directly**, no OpenBao involved: the real `nix-worker`
image, a locally generated host key, a local CA standing in for the
signing role (`ssh-keygen -s ‹ca› -n ci-nix -O force-command="nix-daemon
--stdio" -O clear -O extension:permit-pty`), and the worker started with
`nixWorkers.accounts.nix.{principals: [ci-nix], trusted: true}`'s
container-level equivalent. `nix store ping --store 'ssh-ng://nix@…'`
reports `Trusted: 1`; `nix build --store 'ssh-ng://nix@…' -f …` executes
the derivation ON the worker and returns its real output; the worker's
own daemon log reads `accepted connection from pid …, user nix
(trusted)`. Rebuilt with `trusted: false`, the same store reports
`Trusted: 0` and `nix copy --to` an unsigned path is refused
(`lacks a signature by a trusted key`) — the exact restriction the
values comment for `nixWorkers.accounts.nix.trusted` describes. An
explicit command and a `-t` PTY request are both silently replaced by
the forced command (`ForceCommand` overrides whatever the client asked
for), and `PermitTTY no` refuses the pseudo-terminal. **No worker-side
change was needed for any of this**: `Match User nix`'s `ForceCommand
nix-daemon --stdio`, unqualified by `NIX_REMOTE`, reads the entrypoint's
own exported `NIX_CONFIG` (including `trusted-users`) because OpenSSH
session children inherit the environment `sshd` itself was started
with — a real, load-bearing side effect of *when* the entrypoint script
exports it, not a documented daemon feature, which is why it is written
down here rather than left to be rediscovered.

## The image doctrine

The image contains only what devbox cannot deliver: the nix + devbox
bootstrap, bash-as-sh, the daemonless docker client + buildx, the Go
cache agent, and the goreleaser-pro binary (its license key is a secret
and never ships). Everything else arrives per job from each
repository's own `devbox.json`. Repo- or cluster-specific content in
the image is a bug; the one documented debt is the baked in-cluster nix
substituter (dead elsewhere, upstream fallback).

**Two Go cache binaries ship at once, on purpose.** `ci-cache` is the
agent of [truvity/ci-cache](https://github.com/truvity/ci-cache), which
is what a job should use; `go-cache-plugin` is the tool it replaces and
is kept for one release beside it. `GOCACHEPROG` names a binary that
must exist before the first `go` invocation of a job, and the shared
workflows pin their own version of this image on their own schedule --
so shipping the replacement and removing the original in one release
would fail every build using a pin that had not moved yet. The plugin
goes in the next image release.

A cache binary is in the image at all only because of that ordering
rule: it has to pre-exist the `go` invocation it caches, which is
exactly the kind of thing devbox cannot deliver in time. What it talks
to, and everything about the cache itself, belongs to that repository
and is not described here.

Every version pin in the Dockerfile carries a `# renovate:` annotation
— a pin without one is invisible, and invisible is indistinguishable
from current. That includes the upstream runner base: `latest` was
replaced with an annotated pin precisely so CVE pickups become renovate
PRs instead of side effects.
