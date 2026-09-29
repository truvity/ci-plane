# Changelog

What changed for a consumer, per release. One tag releases everything —
both images and both charts — at one version, so each heading covers all
four. Reconstructed from the history for v2.0.0 through v2.11.0; the 1.x
line is summarised in one section.

## v4.1.0 (unreleased)

**arc-runners: one scale set no longer needs a second release to
differ.** `scaleSets.<name>` now accepts `nodeSelector`, `tolerations`,
`affinity`, `minRunners`, `podAnnotations`, `extraEnv`, `extraEnvFrom`
and `extraNixConfig`, each falling back to the release-wide value of the
same name when unset on that scale set and otherwise REPLACING it
outright (never merging — an explicitly empty map or list on one set has
to be reachable even when the release default is non-empty).
`extraNixConfig` is additive rather than a fallback: it APPENDS to the
release-wide `nixConfig` for that one set, because Nix's config format
keeps the LAST occurrence of a scalar key. Additive: a release that sets
none of these on any scale set renders byte-identical to v4.0.0,
ConfigMap name and `values-hash` annotation included — proved by golden
renders of the unchanged `minimal`, `nix-builders` and `aws-karpenter`
cases plus a new `per-set-overrides` case. See
[docs/architecture.md](docs/architecture.md#one-different-scale-set-does-not-need-a-second-release).

**arc-runners: Nix's own build sandbox, per scale set
(`scaleSets.<name>.nixSandbox`).** The runner image's Nix is
single-user, so with the sandbox off — its default when nothing else
provides isolation — one derivation's builder can write into another's
output before it is signed, and read `/proc/<pid>/environ` of every
other process in the pod, including the runner's own GitHub
OIDC-token-request variables. This is true on every estate running this
image today. Enabling `nixSandbox` on a scale set renders `hostUsers:
false`, `procMount: Unmasked` and a `Localhost` seccomp profile on that
set's pods, and appends `sandbox = true` / `sandbox-fallback = false` to
its Nix config (after any `extraNixConfig`, so nothing else can quietly
re-enable the fallback). `seccompProfile` is required the moment
`enabled: true`. The chart cannot install the profile file or raise the
node's `user.max_user_namespaces` — both are node-side facts outside a
Helm chart's reach — see the new
[docs/nix-sandbox.md](docs/nix-sandbox.md) and its generator,
`hack/gen-nix-sandbox-seccomp.sh` (ported from opwerm/nexus PR #306,
which hand-wrote a stopgap `MutatingAdmissionPolicy` for exactly this
gap; that stopgap is no longer needed for a new install once this
release is in use).

A release-wide `nixSandbox.fence.enabled` (default off) renders a
`ValidatingAdmissionPolicy` that refuses any OTHER pod, in any other
namespace, without that exact scale set's label, or with `hostUsers`
left `true`, from naming a sandboxed set's profile — PodSecurity
`baseline` admits any `Localhost` profile a node happens to carry,
without ever inspecting what it contains, so the profile file alone
protects nothing on a shared node pool.

**arc-runners gains `nixCache.url`** — an override for the Nix
substituter list, without a rebuilt image. Both `image/runner/Dockerfile`
and `image/nix-worker/Dockerfile` still bake the maintainers' own
in-cluster Service (`nix-cache` in namespace `ci-cache`) as their
default `NIX_SUBSTITUTERS`, unchanged, exactly as every release before
this one. That default has been STALE for any OTHER estate since
ci-builders 2.x stopped requiring that fixed namespace — the Nix cache
Service now lives in whatever namespace an estate installs ci-builders
into, so a consumer whose cache lives elsewhere previously had no way to
say so short of rebuilding the image. `nixCache.url`, when set, renders
a full `substituters = <url> https://cache.nixos.org/` line into the
chart's own `NIX_CONFIG` overlay (which already beat the image's baked
nix.conf) — an estate that needs it sets it; every other estate,
including the maintainers' own, sets nothing and sees no change at all.
Empty by default, so this is fully additive.

**Un-baking the image's own default is intentionally NOT part of this
release.** The old default resolves to a real, working cache on at
least one estate (Truvity's own `ci-cache` namespace), so removing it
would silently degrade that estate's runners the moment a release
picked up a rebuilt image — every Nix call falling back to fetching
straight from cache.nixos.org instead of the in-cluster cache (the
~155 GB/day from upstream measured the last time this cache was
bypassed, `image/runner/Dockerfile`'s own history). Moving the default
is a separate, coordinated change for a LATER release: every consumer
that relies on the baked default — the maintainers' own estate
included — sets `nixCache.url` explicitly first, in its own time, and
only once that is done everywhere does a later release change what the
image bakes.

- `charts/arc-runners`, `values.schema.json`: `nixCache` and
  `nixSandbox.fence` added (types only, open schema, matching the
  chart's existing convention); no new REQUIRED keys.
- New golden cases: `per-set-overrides`, `nix-sandbox`,
  `nix-sandbox-fence`, `nix-cache`. New invalid fixtures:
  `nix-sandbox-without-profile`, `nix-sandbox-fence-without-sandbox`.
- `hack/leak-canary.sh`'s Dockerfile-specific allowance for the baked
  substituter stays, unchanged — both Dockerfiles' defaults are
  unchanged too.

**arc-runners: the runner pod template gains generic hooks
(`extraVolumes`, `extraVolumeMounts`, `extraInitContainers`), one
convenience for the common case of a second identity
(`projectedServiceAccountTokens`), and an `awsConfig` block for a
`credential_process` profile.** THE CASE THIS EXISTS FOR: the Go build
cache's S3-compatible client (`GOCACHEPROG`) had no way to reach a
short-lived, prefix-scoped credential on a store with no pod identity
(Cloudflare R2, MinIO, Ceph) — only a static key in a Secret
(`extraEnvFrom`, unchanged). `awsConfig` renders a ConfigMap holding one
AWS config profile and mounts it read-only on the runner container
alone, wired through `AWS_CONFIG_FILE`/`AWS_PROFILE` — the shape
`accessctl r2` (access-roster v1.39+), fronting `truvity/cloudflare`'s
r2broker, needs as a `credential_process` line. See
[docs/architecture.md#an-aws-profile-for-a-broker](docs/architecture.md#an-aws-profile-for-a-broker)
for the worked example, including why nothing extra is needed to run
the helper (a job's own GitHub OIDC token, already read by `accessctl`
when `id-token: write` is granted).

- Refused at render time, never silently ignored:
  `awsConfig.enabled` alongside an `extraEnv` entry named
  `AWS_ACCESS_KEY_ID` (the AWS SDK's credential chain checks
  environment variables before `credential_process`, so the static key
  would keep winning and the broker would never be consulted), and two
  `projectedServiceAccountTokens` entries naming the same `path` (the
  kubelet projects every source of one volume into one directory, so
  the second would silently replace the first at apply time).
- All five keys are additive and release-wide only (no per-scale-set
  override, unlike `extraEnv`/`extraEnvFrom`); empty/disabled by
  default renders no new ConfigMap, volume, mount, init container or
  environment variable — byte-identical to v4.1.0 without this change,
  proved by the unchanged `minimal`/`nix-builders`/`aws-karpenter`
  goldens plus a new `r2-broker` case exercising every key at once.
  `values.schema.json` gained matching types and an `awsConfig`
  enabled/then block, the same shape `nixBuilders` already uses.
- `image/runner/Dockerfile` bakes `accessctl` (truvity/access-roster)
  and `r2broker` (truvity/cloudflare), pinned and verified against that
  release's own `checksums.txt` (downloaded at build time, so a
  Renovate version bump can never desync a pin from a hand-maintained
  digest the way two separately hand-pinned values could), each with a
  `# renovate:` annotation `hack/renovate-covers-dockerfiles.sh` already
  covers via the existing `image/**/Dockerfile` glob. Both run on
  amd64 and arm64 (verified with a native build and a QEMU-emulated
  one). Neither is required by a job that never names `accessctl r2` in
  a `credential_process` line.

## v4.0.0

**Breaking values: another major.** More estate facts are no longer
defaults (component contract C13; v3.0.0 already removed the first
five the same way). Each is now empty by default, and nothing renders
on it until you set it:

| chart | value | old default | now |
|---|---|---|---|
| ci-builders | `storageClassName` | `gp3` | empty (cluster's default StorageClass) |
| ci-builders | `buildkitd.podAnnotations` | `{karpenter.sh/do-not-disrupt: "true"}` | `{}` |
| ci-builders | `nixWorkers.podAnnotations` | `{karpenter.sh/do-not-disrupt: "true"}` | `{}` |
| arc-runners | `tolerations` | `[{key: arch, operator: Exists}, {key: ci, value: "true", effect: NoSchedule}]` | `[]` |
| arc-runners | `podAnnotations` | `{karpenter.sh/do-not-disrupt: "true"}` | `{}` |
| arc-runners | `listenerTolerations` | `[{key: arch, operator: Exists}]` | `[]` |

To upgrade, set each of these to what the old default resolved to in
your estate, or to its real value. [docs/day-1-install.md](docs/day-1-install.md)
has the shape, under "Values an AWS/Karpenter estate sets".

- None of these become `required` in `values.schema.json`: the chart
  works with each left empty (no toleration, no annotation, the
  cluster's default StorageClass), so C13's schema rule ("required only
  where the chart cannot work without the value") leaves them optional.
- `ci-builders`' PVC templates now omit `storageClassName` entirely when
  it is empty, rather than rendering `storageClassName: ""`, which
  Kubernetes reads as "no class" rather than "the cluster's default".
- Golden renders gained an `aws-karpenter` case per chart that sets all
  six values explicitly, alongside the existing bare-defaults case.

## v3.0.0

**Breaking values: the next release is a major.** Estate facts are no
longer defaults. Each is now required, and a values file that relied on
the old default fails to render (`values.schema.json`) instead of
quietly deploying one estate's shape somewhere else:

| chart | value | old default | now |
|---|---|---|---|
| arc-runners | `controllerServiceAccount.namespace` | `arc-system` | required |
| arc-runners | `nixBuilders.builders` | two hosts in namespace `ci-cache` | required while `nixBuilders.enabled` |
| ci-builders | `buildkitd.networkPolicy.consumerNamespaces` | one estate's runner namespace | required while the component and its policy are on |
| ci-builders | `nixWorkers.networkPolicy.consumerNamespaces` | the same | the same |
| ci-builders | `npmCache.networkPolicy.consumerNamespaces` | the same | the same |

To upgrade, set each of these to what the old default resolved to in
your estate, or to its real value. [docs/day-1-install.md](docs/day-1-install.md)
has the shape.

- Both charts carry a `values.schema.json`. It is open rather than
  strict: it checks types and the required inputs above.
- `Chart.yaml` commits `version: 0.0.0` and `appVersion: 0.0.0`, not
  `0.0.0-dev`. The release overwrites both from the tag, as before.
- Golden renders under `tests/golden/`, refused fixtures under
  `tests/invalid/`, and a `devbox.json` + `Justfile` whose `just check`
  runs lint, the goldens and the leak canary.
- The removed-`goModproxy` refusal points at the ci-cache server's chart,
  `oci://ghcr.io/truvity/charts/ci-cache-server`.

## v2.11.0

Released 2026-09-28. nix-worker: opkssh sign-in for people, as a pilot
on the architectures `nixWorkers.opkssh.archs` names; off by default.

## v2.10.1

Released 2026-09-28. Dependency updates only.

## v2.10.0

Released 2026-09-28. nix-worker: the legacy `nixremote` login can be
switched off (`nixWorkers.accounts.nixremote.enabled`), and readiness
watches the host certificate's remaining life.

## v2.9.1

Released 2026-09-28. nix-worker: the `nix` login's principals are copied
out of the ConfigMap mount, so sshd's StrictModes accepts them.

## v2.9.0

Released 2026-09-27. arc-runners: runners can reach the workers as `nix`
over ssh-ng with a forced-command certificate
(`nixBuilders.login`), and can trust a host CA instead of pinning host
keys (`nixBuilders.knownHosts.pinned: false`).

## v2.8.0

Released 2026-09-27. nix-worker: workers can sign their own host keys
with OpenBao SSH certificates (`nixWorkers.hostCertificate`, phase 1);
off by default.

## v2.7.0

Released 2026-09-27. arc-runners: runners pack onto nodes that already
run runners (`packing`, on by default). bazel-remote: an optional S3
backend, off by default and AWS-only (no `endpoint` key).

## v2.6.1

Released 2026-09-24. The runner image no longer carries the ci-cache
agent; `go-cache-plugin` is its only Go cache client.

## v2.6.0

Released 2026-09-24. The nix workers get their own image
(`nixWorkerImage`), and releases build both architectures natively
instead of emulating arm64. Chart publishing stays in this repository
for now: the shared workflow's `images:` convention does not fit these
charts yet.

## v2.5.0

Released 2026-09-24. arc-runners: the janitor also sweeps runners
wedged in deletion, not only stranded ones.

## v2.4.0

Released 2026-09-24. The runner image carries the ci-cache agent at
0.1.4.

## v2.3.0

Released 2026-09-24. The runner image carries the ci-cache agent at
0.1.3.

## v2.2.0

Released 2026-09-23. The runner image carries the ci-cache agent at
0.1.2.

## v2.1.0

Released 2026-09-23. The runner image ships the ci-cache agent beside
`go-cache-plugin`, for one release.

## v2.0.0

Released 2026-09-23. **Breaking:** the `ci-cache` chart is renamed
`ci-builders`, and its Go module proxy is removed — setting
`goModproxy` fails the render. The module proxy is now
[truvity/ci-cache](https://github.com/truvity/ci-cache). The registry
path `charts/ci-cache` keeps this repository's 1.x releases.

## v1.0.0 – v1.5.0

Released 2026-08-25 to 2026-09-21, 23 tags. The CI plane as one
versioned unit: the runner image, the `ci-cache` chart (rootless
buildkitd per architecture, the Nix read-through cache, the npm
registry cache, the Bazel remote cache, and an S3-backed Go module
proxy), and the `arc-runners` chart (per-profile scale sets with
measured sizes, the stuck-runner janitor). Along the way: the
`preview-medium` profile, OpenBao-backed persistent Nix workers with
per-job certificates, the Nix cache made the preferred substituter,
buildkitd sized from a build peak, `extraEnv`/`extraEnvFrom` on the
runner, and goreleaser-pro removed from the image because its licence
forbids redistribution.
