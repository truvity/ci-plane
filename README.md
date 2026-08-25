# ci-plane

The CI plane for GitHub Actions on Kubernetes, as one versioned unit:
the ARC **runner image** and the Helm charts that surround it — the
**in-cluster cache plane** and the **runner scale sets with the profile
doctrine baked in**. One `v*` tag releases everything at one version,
with the image digest stamped into the chart defaults, so a consumer
pins a single chart version and is transitively digest-pinned.

Supersedes [truvity/runner-image](https://github.com/truvity/runner-image).

## Documentation

- [Architecture](docs/architecture.md) — the load-bearing decisions and
  why each one holds
- [Day 1 — install](docs/day-1-install.md) — fresh-estate setup
- [Day 2 — operations](docs/day-2-operations.md) — releasing, the
  automatic chain, rollback, upgrades

## Contents

| Artifact | Published as |
|---|---|
| [`image/`](image/Dockerfile) — the runner image | `ghcr.io/truvity/ci-plane/runner:<version>` |
| [`charts/ci-cache`](charts/ci-cache) — buildkitd (per-arch, daemonless), nix read-through cache, Go module proxy, npm registry cache, Bazel-API remote cache | `oci://ghcr.io/truvity/charts/ci-cache` |
| [`charts/arc-runners`](charts/arc-runners) — AutoscalingRunnerSet CRs per profile + the #4307 stuck-runner janitor | `oci://ghcr.io/truvity/charts/arc-runners` |

The ARC **controller** (and its CRDs) is deliberately NOT here — install
it from the upstream `gha-runner-scale-set-controller` chart. The
`arc-runners` chart renders the CRs directly, so it cannot drift from
the controller's own chart-version stream.

## Package naming convention

Multi-artifact repositories nest their container packages under the
repository name (`ghcr.io/truvity/<repo>/<artifact>` — this repo's
`ci-plane/runner`); single-artifact repositories historically used flat
repo-named packages and are migrating to the nested form
(INF-574…INF-579). `ghcr.io/truvity/charts/<name>` stays the chart
namespace.

## Release model

**A `v*` tag is the release act** — tag creation is restricted by
ruleset, so who can tag is who can publish. Renovate keeps every pinned
tool current and automerges on green, so master is perpetually current;
a patch tag on an unchanged tree is the legitimate "pure refresh"
release (base-image/CVE pickup). The upstream runner base is pinned and
renovate-annotated for exactly this reason — `latest` was invisible to
renovate. With `vars.AUTO_RELEASE=true` (and the App configured plus a
tag-ruleset bypass for it), the weekly auto-release workflow cuts the
patch tag itself. **Promotion is the consumer's job**: this repo
releases and stops — gitops's own renovate tracks the published chart
version, bumps its pin, regenerates renders in-branch and merges on
green; ArgoCD syncs. Fully hands-off, with one identity per side.

**Skip-rebuild** is load-bearing: when `image/` is unchanged since the
previous release, the previous digest is re-tagged (manifest copy)
instead of rebuilt — a chart-only patch never rolls a consumer's runner
fleet.

## Consuming (the Truvity shape)

gitops pins one version:

```yaml
ciPlane: "1.0.0"
```

Argo Applications (estate-side, one per chart instance) consume the OCI
charts at that revision. The image reference is split
`{registry}/{repository}@{digest}` in values — an estate with an ECR
pull-through cache overrides `registry` only (e.g.
`712….amazonaws.com/github`) and inherits the stamped digest.

## Doctrine highlights

- The image contains only what devbox cannot deliver; everything else
  arrives per job from the repo's own `devbox.json`. Repo- or
  cluster-specific content in the image is a bug.
- Every cache is a CACHE: disposable storage, zero backups, losing one
  costs a re-warm; a dead cache degrades to upstream, never breaks.
- Scale-set profiles are MEASURED: memory request == limit (no
  overcommit — ceilings cannot sum past a node's allocatable),
  GOMAXPROCS pinned to the cgroup, no ephemeral-storage request but a
  hard limit, disruption protection on mid-job runners.
- Restrictive network defaults: only listed namespaces reach the
  builders.

Known Truvity-shaped debt: the image's baked nix config lists an
in-cluster substituter (`nix-cache.ci-cache.svc.cluster.local`) — a
dead substituter elsewhere, upstream fallback, slower never broken.

## License

[MIT](LICENSE)
