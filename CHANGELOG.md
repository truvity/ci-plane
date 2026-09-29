# Changelog

What changed for a consumer, per release. One tag releases everything —
both images and both charts — at one version, so each heading covers all
four. Reconstructed from the history for v2.0.0 through v2.11.0; the 1.x
line is summarised in one section.

## Unreleased

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
