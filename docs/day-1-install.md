# Day 1 — installing the CI plane on a fresh estate

Order matters: caches before runners, controller before both charts.

## Prerequisites

1. **The ARC controller**, installed from the upstream
   `gha-runner-scale-set-controller` chart (deliberately not part of
   this repo — see [architecture.md](architecture.md)). Note its
   version and its ServiceAccount (name + namespace): arc-runners
   requires the namespace (`controllerServiceAccount.namespace`) and has
   no default for it.
2. **A GitHub App per organization** for runner registration, with
   `organization_self_hosted_runners: write`. Its credentials land in a
   Secret named `arc-github-app` (keys `github_app_id`,
   `github_app_installation_id`, `github_app_private_key`) in each
   runner namespace — delivered however your estate delivers secrets
   (ESO, sealed-secrets, by hand).
3. **Runner namespaces**, one per organization (e.g.
   `arc-runners-<org>`), plus a namespace for the build plane, written
   `<namespace>` below. Dedicated namespaces — a general-purpose janitor
   that sweeps old releases will eventually collect a long-lived
   AutoscalingRunnerSet. The charts take the namespace from the release;
   two things outside them still assume the maintainers' name,
   `ci-cache`:
   - the published runner and nix-worker images bake the Service
     `nix-cache` in namespace `ci-cache` as their first Nix substituter.
     Anywhere else, override it without rebuilding through arc-runners'
     `nixConfig`, e.g.
     `substituters = http://nix-cache.<namespace>.svc.cluster.local https://cache.nixos.org/`,
     or every Nix call retries a name that does not resolve;
   - truvity/ci-workflows' `node-cache: true` probes
     `npm-cache.ci-cache.svc` by that fixed name.
4. Optional but recommended: a **registry pull-through cache** per
   hosting account, so image pulls are same-region and unthrottled. The
   charts' image references are split `{registry}/{repository}` so you
   override only the registry.
5. Nothing here for Go any more. The module proxy left in 2.0.0 for
   [truvity/ci-cache](https://github.com/truvity/ci-cache). Setting
   `goModproxy` here fails the render rather than being ignored.
6. For persistent Nix workers:
   - `<namespace>/nix-builder-server`, delivered by the estate's secret
     manager, containing the stable `ssh_host_ed25519_key` (and its
     public half for operations);
   - `<runner namespace>/nix-builder-known-hosts`, containing only
     `known_hosts` for both worker Service FQDNs;
   - an OpenBao SSH user CA (secrets engine mount, default `ssh`) with a
     signing role (default `user`) that allows the principal `nixremote`
     and caps certificates at one hour; its public keys go to
     `nixWorkers.ssh.trustedUserCAKeys` here and to
     `nixBuilders.openbao.sshCAPublicKeys` in each runner release;
   - per organization, an OpenBao JWT login role on the environment's
     auth mount, bound exactly to the runner ServiceAccount, whose token
     may only sign on that role.

   Each runner generates its client key inside its own pod and receives a
   short-lived certificate. No client private key or `authorized_keys`
   Secret exists. Do not add engineer keys or expose worker Services
   outside the cluster.

   **Host certificates (`nixWorkers.hostCertificate`) are an additive
   alternative to the pinned `ssh.existingSecret` host key above, off by
   default.** Enabling them signs each worker's own host key against a
   SECOND OpenBao SSH role (`nixWorkers.openbao.hostSshMount`/
   `hostSshRole`, `cert_type=host`), and a caller trusts the CA instead of
   pinning one host key Secret per architecture by adding it to
   `nixBuilders.knownHosts.certAuthorities` on the runner side — both stay
   valid at once during a migration. See
   [architecture.md](architecture.md#worker-host-certificates-phase-1).

## Values an AWS/Karpenter estate sets

Neither chart bakes in a storage class, a toleration or a scheduling
annotation any more (component contract C13: estate facts are inputs,
never defaults). Before v4.0.0 these rendered unconditionally as
Truvity's own AWS/Karpenter shape; now each is empty or absent until you
set it, and the chart works with each left empty. What Truvity's own
fleet sets explicitly:

| chart | value | what an AWS/Karpenter estate sets |
|---|---|---|
| ci-builders | `storageClassName` | `gp3` |
| ci-builders | `buildkitd.podAnnotations` | `{karpenter.sh/do-not-disrupt: "true"}` |
| ci-builders | `nixWorkers.podAnnotations` | `{karpenter.sh/do-not-disrupt: "true"}` |
| arc-runners | `tolerations` | `[{key: arch, operator: Exists}, {key: ci, value: "true", effect: NoSchedule}]` |
| arc-runners | `podAnnotations` | `{karpenter.sh/do-not-disrupt: "true"}` |
| arc-runners | `listenerTolerations` | `[{key: arch, operator: Exists}]` |

An empty `storageClassName` renders no `storageClassName` field on a PVC
at all, which Kubernetes reads as "use the cluster's default
StorageClass" — not the same as an explicit empty string, which
Kubernetes reads as "no class". A second, non-AWS estate sets its own
storage class and its own tolerations instead; neither of these has a
chart default to fall back on.

## Install the cache plane

```bash
helm install ci-builders oci://ghcr.io/truvity/charts/ci-builders \
  --version <X.Y.Z> -n <namespace> \
  --set buildkitd.networkPolicy.consumerNamespaces={arc-runners-<org>} \
  --set npmCache.networkPolicy.consumerNamespaces={arc-runners-<org>} \
  --set nixWorkers.enabled=true \
  --set-string 'nixWorkers.ssh.trustedUserCAKeys[0]=ssh-ed25519 <CA-PUBLIC-BODY>' \
  --set nixWorkers.networkPolicy.consumerNamespaces={arc-runners-<org>} \
  # per-arch dedicated pool split, storage class, sizes, registry overrides as needed
```

**Required, with no default:** each enabled component's
`networkPolicy.consumerNamespaces` — `buildkitd`, `npmCache`, and
`nixWorkers` when enabled — while its `networkPolicy.enabled` is true
(the default). A builder behind a policy that admits nobody cannot work,
so the chart's `values.schema.json` refuses the empty list instead of
rendering it. List your runner namespaces, or set that component's
`networkPolicy.enabled: false`.

Key values (see the chart's values.yaml for the full annotated set):
`buildkitd.archs`, `buildkitd.scheduling.<arch>` (nodeSelector +
tolerations per arch, REPLACING the default when set),
`nixWorkers.scheduling.<arch>`, `storageClassName` (empty by default —
see "Values an AWS/Karpenter estate sets" above),
`nixCache.upstream`. A Nix worker is privileged so
its daemon can create Linux sandboxes: enabling it without selecting a
dedicated, tainted CI/build pool is not a supported production shape.

## Install the runner scale sets (one release per org)

```yaml
# builders.yaml -- one entry per architecture ci-builders runs
nixBuilders:
  builders:
    - host: nix-worker-amd64.<namespace>.svc.cluster.local
      system: x86_64-linux
      maxJobs: 2
      speedFactor: 1
    - host: nix-worker-arm64.<namespace>.svc.cluster.local
      system: aarch64-linux
      maxJobs: 2
      speedFactor: 1
```

```bash
helm install arc-runners oci://ghcr.io/truvity/charts/arc-runners \
  --version <X.Y.Z> -n arc-runners-<org> -f builders.yaml \
  --set githubConfigUrl=https://github.com/<org> \
  --set controllerServiceAccount.namespace=<CONTROLLER NAMESPACE> \
  --set arcVersion=<INSTALLED CONTROLLER VERSION> \
  --set nixBuilders.enabled=true \
  --set nixBuilders.openbao.address=https://<OPENBAO-ENDPOINT> \
  --set nixBuilders.openbao.namespace=<ENVIRONMENT> \
  --set nixBuilders.openbao.caBundle=<BASE64-HTTPS-CA-PEM> \
  --set-string 'nixBuilders.openbao.sshCAPublicKeys[0]=ssh-ed25519 <CA-PUBLIC-BODY>' \
  --set nixBuilders.knownHosts.revision=host-v1 \
  --set nixBuilders.openbao.authMount=<JWT-AUTH-MOUNT> \
  --set nixBuilders.openbao.authRole=<ORG-LOGIN-ROLE> \
  --set runnerServiceAccountName=<SIGNER-BOUND SA> \
  --set nodeSelector.<your CI pool label>=<value>
```

**Required, with no default:** `githubConfigUrl`,
`controllerServiceAccount.namespace` (wherever the controller runs), and
`nixBuilders.builders` while `nixBuilders.enabled` — the worker hosts
live in whatever namespace ci-builders went into, so the chart cannot
name them. `values.schema.json` refuses a render without them.

**`arcVersion` is not optional in spirit**: the controller deletes any
scale set whose `app.kubernetes.io/version` label differs from its
build version. Feed it from the same pin that installs the controller.

The scale-set names (`preview-large`, `preview-medium`, `preview-small`
by default — the `scaleSets` map keys) ARE the workflows' `runs-on`
labels and the GitHub-side scale-set identities. Rename by adding
alongside and migrating callers, never in place: a rename strands
queued jobs.

## Runner pod expectations

- One ephemeral pod per job; a warm runner per set (`minRunners: 1`)
  removes the cold-start window that upstream ARC#4307 turns into a
  permanent stall.
- Run CI pools on-demand, not spot: an estate that sets `podAnnotations`
  to `karpenter.sh/do-not-disrupt: "true"` (see "Values an AWS/Karpenter
  estate sets" above; empty by default) stops consolidation, not
  reclaims.
- Runners prefer nodes that already run runners, across every scale
  set and organization (`packing`, on by default). The same annotation
  is why packing matters: without it, nothing can repack runners after
  they are scheduled, so they have to be placed together in the first
  place. It is a preference only, so a full pool still scales out as
  before. Set `packing.enabled: false` to get the scheduler's default
  spreading back; a custom `affinity` is merged with it, not replaced.
- The `#4307` janitor CronJob ships enabled — it deletes only runners
  that hold no job, match a stuck-log signature, and outlived a grace
  period.

## Wiring workflows

Point `runs-on` at the scale-set names (via org variables so a rename
is one change, not N). Runners reach the caches by cluster DNS:
`buildkitd-<arch>.<namespace>.svc:1234` (buildx `--driver remote`),
`nix-cache.<namespace>.svc` (nix substituter),
`bazel-remote.<namespace>.svc:9092` (moon remote cache),
`npm-cache.<namespace>.svc` (yarn/npm/pnpm `npmRegistryServer`, public
packages, behind a health-probe fallback to registry.npmjs.org).
