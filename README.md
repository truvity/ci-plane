# ci-plane

The CI plane for GitHub Actions on Kubernetes, released as one versioned
unit: the ARC **runner image**, the **nix-worker image**, and the two
Helm charts around them — **`ci-builders`** (the in-cluster builders and
caches) and **`arc-runners`** (the runner scale sets). One `v*` tag
publishes all four at one version, with each image's digest stamped into
the chart defaults, so a consumer pins one chart version and is
transitively digest-pinned.

| Artifact | What | Published as |
|---|---|---|
| [`image/runner`](image/runner/Dockerfile) | The runner: nix + devbox bootstrap, the daemonless docker client + buildx, `go-cache-plugin` | `ghcr.io/truvity/ci-plane/runner:vX.Y.Z` |
| [`image/nix-worker`](image/nix-worker/Dockerfile) | A persistent remote Nix builder: nix-daemon behind sshd | `ghcr.io/truvity/ci-plane/nix-worker:vX.Y.Z` |
| [`charts/ci-builders`](charts/ci-builders) | Rootless buildkitd and Nix workers per architecture; the Nix, npm and Bazel read-through caches | `oci://ghcr.io/truvity/charts/ci-builders` |
| [`charts/arc-runners`](charts/arc-runners) | AutoscalingRunnerSet CRs per measured profile, the stuck-runner janitor, remote Nix builders per job | `oci://ghcr.io/truvity/charts/arc-runners` |

Up to 1.5.0, `ci-builders` was published as
`oci://ghcr.io/truvity/charts/ci-cache` and carried a Go module proxy;
2.0.0 renamed it and moved the proxy to
[truvity/ci-cache](https://github.com/truvity/ci-cache).

## Who it is for

A platform team running self-hosted GitHub Actions runners on Kubernetes
with the Actions Runner Controller (ARC), that wants the runners, the
image builders and the build caches sized, versioned and upgraded as one
thing. It assumes an installed ARC controller, a GitHub App per
organization, and — for the remote Nix builders — an OpenBao SSH
secrets engine to sign short-lived certificates.

It deliberately does not install the ARC controller or its CRDs (use the
upstream `gha-runner-scale-set-controller` chart; `arc-runners` renders
the CRs directly so it cannot drift from the controller's own chart
stream), the GitHub App, any Secret, or the cluster's node pools.

## The model

- **The image is thin.** It carries only what devbox cannot deliver in
  time: the nix + devbox bootstrap, bash as `sh`, the docker client and
  buildx, and the Go build cache client. Every other tool arrives per job
  from the repository's own `devbox.json`.
- **Builders are shared, daemons never ship to runners.** buildx on a
  runner talks to rootless buildkitd per architecture (`--driver
  remote`), and Nix hands remote builds to a persistent worker per
  architecture over SSH with a certificate signed per job.
- **Every cache is a cache.** Disposable storage, no backups; losing one
  costs a re-warm, and a dead cache degrades to upstream — slower, never
  broken.
- **Profiles are measured.** Memory request equals limit, `GOMAXPROCS`
  is pinned to the cgroup, disruption protection covers a runner
  mid-job, and runners pack onto nodes that already run runners.
- **Estate facts are inputs.** The organization, the controller's
  namespace, the runner namespaces allowed to reach each builder, and the
  worker hosts have no defaults; the charts refuse to render without
  them.

[docs/architecture.md](docs/architecture.md) has each decision and why
it holds.

## Install and a worked example

With the ARC controller installed and a runner namespace per
organization, install the builders into a namespace of their own
(`<namespace>` below), then one runner release per organization:

```sh
helm install ci-builders oci://ghcr.io/truvity/charts/ci-builders \
  --version <X.Y.Z> -n <namespace> \
  --set 'buildkitd.networkPolicy.consumerNamespaces={arc-runners-<org>}' \
  --set 'npmCache.networkPolicy.consumerNamespaces={arc-runners-<org>}'

helm install arc-runners oci://ghcr.io/truvity/charts/arc-runners \
  --version <X.Y.Z> -n arc-runners-<org> \
  --set githubConfigUrl=https://github.com/<org> \
  --set controllerServiceAccount.namespace=<controller namespace> \
  --set arcVersion=<installed controller version>
```

Pin both charts to the same version. A workflow then asks for a profile
by its scale-set name:

```yaml
jobs:
  build:
    runs-on: preview-small   # or preview-medium, preview-large
```

[docs/day-1-install.md](docs/day-1-install.md) covers the prerequisites,
the Nix workers, per-architecture pools and every required value.

## Consumers

- **truvity/gitops** — `ci-builders` and `arc-runners`, one version
  pinned for both and promoted by its own renovate.
- **opwerm/nexus** — `ci-builders` and `arc-runners`.
- **truvity/ci-workflows** — its `node-cache: true` option probes
  `ci-builders`' npm cache by a fixed Service name in namespace
  `ci-cache`.

## Neighbours

- **[truvity/ci-workflows](https://github.com/truvity/ci-workflows)** is
  the only thing a caller pins;
  **[truvity/ci-actions](https://github.com/truvity/ci-actions)** holds
  the composite steps; **[truvity/ci-cache](https://github.com/truvity/ci-cache)**
  owns cache wiring and the cache server; this repository is where the
  work executes — the runner and nix-worker images and the
  `arc-runners` and `ci-builders` charts. `ci-builders` still carries the
  Nix, npm and Bazel caches; moving them to ci-cache is tracked
  separately.
- **[truvity/policy](https://github.com/truvity/policy)** holds the
  [component contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md)
  this repository is held to.

## Documentation

- [docs/architecture.md](docs/architecture.md) — the load-bearing
  decisions and why each one holds
- [docs/day-1-install.md](docs/day-1-install.md) — a fresh estate, in
  order, with every required value
- [docs/day-2-operations.md](docs/day-2-operations.md) — releasing, the
  automatic chain, rollback, upgrades, the Nix workers and the caches
- [CHANGELOG.md](CHANGELOG.md) — what changed for a consumer, per
  version

## The rule that makes this repository public

**Mechanism only.** Nothing here names an organization's namespace, an
account, a registry host or an internal hostname; each is an input, and
the consuming estate supplies it from its own repository.
`hack/leak-canary.sh` enforces this in `just check`, because public
history cannot be unpublished. One known debt remains: both images bake
the maintainers' in-cluster Nix substituter as a build argument, which
consumers override at runtime through `arc-runners`' `nixConfig`
([day-1](docs/day-1-install.md#prerequisites)).

This repository follows the shared
[component contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md);
[docs/normalization.md](docs/normalization.md) now only points there.

## Status

Used in production by its maintainers, on 2.x. Every tag publishes
the images and the charts; there are no GitHub Releases for the tags,
so [CHANGELOG.md](CHANGELOG.md) and the
[tags](https://github.com/truvity/ci-plane/tags) are the record.

## Development

```sh
devbox shell        # or direnv
just check          # chart lint, golden renders + refused fixtures, leak canary
just golden-update  # regenerate tests/golden after a template change — review the diff
just test           # the two Go helper modules under image/
```

Every `tests/cases/<chart>/<case>/values.yaml` renders, in namespace
`example`, to `tests/golden/<chart>/<case>.yaml`, and every
`tests/invalid/<chart>/*.yaml` must be refused for the reason its first
line names. The chart assertions, image builds and Go tests in
[ci.yaml](.github/workflows/ci.yaml) run in CI beside `just check`;
their renders start from `tests/ci/<chart>.yaml`.

## Releasing

Push a tag `vX.Y.Z` after its `## vX.Y.Z` heading lands in the
CHANGELOG; tag creation is restricted by ruleset. The release workflow
builds both images natively per architecture, stamps each digest into
the charts by key, stamps both charts' `version` and `appVersion` from
the tag — `Chart.yaml` commits `0.0.0` — and publishes them.
`auto-release` is armed (`vars.AUTO_RELEASE` is `true`): the shared
workflow cuts the next patch tag itself when master is ahead of the last
release. Minors and majors are tagged by hand.
[docs/day-2-operations.md](docs/day-2-operations.md) has both paths.

## Licence

MIT — see [LICENSE](LICENSE).
