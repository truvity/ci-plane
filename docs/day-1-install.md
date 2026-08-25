# Day 1 — installing the CI plane on a fresh estate

Order matters: caches before runners, controller before both charts.

## Prerequisites

1. **The ARC controller**, installed from the upstream
   `gha-runner-scale-set-controller` chart (deliberately not part of
   this repo — see [architecture.md](architecture.md)). Note its
   version and its ServiceAccount (name + namespace).
2. **A GitHub App per organization** for runner registration, with
   `organization_self_hosted_runners: write`. Its credentials land in a
   Secret named `arc-github-app` (keys `github_app_id`,
   `github_app_installation_id`, `github_app_private_key`) in each
   runner namespace — delivered however your estate delivers secrets
   (ESO, sealed-secrets, by hand).
3. **Runner namespaces**, one per organization (e.g.
   `arc-runners-<org>`), plus a `ci-cache` namespace. Dedicated
   namespaces — a general-purpose janitor that sweeps old releases will
   eventually collect a long-lived AutoscalingRunnerSet.
4. Optional but recommended: a **registry pull-through cache** per
   hosting account, so image pulls are same-region and unthrottled. The
   charts' image references are split `{registry}/{repository}` so you
   override only the registry.
5. For the Go module proxy: an **S3 bucket** and pod-identity/IRSA
   wiring for its ServiceAccount, scoped to the cache prefix. Skip it
   (`goModproxy.enabled=false`, the default) if you have neither.

## Install the cache plane

```bash
helm install ci-cache oci://ghcr.io/truvity/charts/ci-cache \
  --version <X.Y.Z> -n ci-cache \
  --set buildkitd.networkPolicy.consumerNamespaces={arc-runners-<org>} \
  # per-arch pool split, storage class, sizes, registry overrides as needed
```

Key values (see the chart's values.yaml for the full annotated set):
`buildkitd.archs`, `buildkitd.scheduling.<arch>` (nodeSelector +
tolerations per arch, REPLACING the default when set),
`storageClassName`, `nixCache.upstream`, `goModproxy.s3.*`.

## Install the runner scale sets (one release per org)

```bash
helm install arc-runners oci://ghcr.io/truvity/charts/arc-runners \
  --version <X.Y.Z> -n arc-runners-<org> \
  --set githubConfigUrl=https://github.com/<org> \
  --set arcVersion=<INSTALLED CONTROLLER VERSION> \
  --set runnerServiceAccountName=<plumbing SA> \
  --set nodeSelector.<your CI pool label>=<value>
```

**`arcVersion` is not optional in spirit**: the controller deletes any
scale set whose `app.kubernetes.io/version` label differs from its
build version. Feed it from the same pin that installs the controller.

The scale-set names (`preview-large`, `preview-small` by default — the
`scaleSets` map keys) ARE the workflows' `runs-on` labels and the
GitHub-side scale-set identities. Rename by adding alongside and
migrating callers, never in place: a rename strands queued jobs.

## Runner pod expectations

- One ephemeral pod per job; a warm runner per set (`minRunners: 1`)
  removes the cold-start window that upstream ARC#4307 turns into a
  permanent stall.
- Run CI pools on-demand, not spot: the `karpenter.sh/do-not-disrupt`
  annotation stops consolidation, not reclaims.
- The `#4307` janitor CronJob ships enabled — it deletes only runners
  that hold no job, match a stuck-log signature, and outlived a grace
  period.

## Wiring workflows

Point `runs-on` at the scale-set names (via org variables so a rename
is one change, not N). Runners reach the caches by cluster DNS:
`buildkitd-<arch>.ci-cache.svc:1234` (buildx `--driver remote`),
`nix-cache.ci-cache.svc` (nix extra-substituter),
`go-modproxy.ci-cache.svc` (GOPROXY with a pipe-fallback to upstream),
`bazel-remote.ci-cache.svc:9092` (moon remote cache),
`npm-cache.ci-cache.svc` (yarn/npm/pnpm `npmRegistryServer`, public
packages, behind a health-probe fallback to registry.npmjs.org).
