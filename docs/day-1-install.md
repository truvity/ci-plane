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
     `nixCache.url` (v4.1.0), e.g.
     `nixCache.url: http://nix-cache.<namespace>.svc.cluster.local`
     (renders a full `substituters = <url> https://cache.nixos.org/`
     line into `NIX_CONFIG`, which beats the image's own nix.conf), or
     every Nix call retries a name that does not resolve. Empty by
     default, so an estate that already resolves the baked name (or
     already overrides it through the general-purpose `nixConfig`) sees
     no change from setting nothing. Un-baking the image's own default
     is a separate, later, coordinated change — see CHANGELOG.md;
   - truvity/ci-workflows' `node-cache: true` probes
     `npm-cache.ci-cache.svc` by that fixed name.
4. Optional but recommended: a **registry pull-through cache** per
   hosting account, so image pulls are same-region and unthrottled. The
   charts' image references are split `{registry}/{repository}` so you
   override only the registry.
5. Optional: the Go module proxy (`goModproxy.enabled: true`), a
   caching module + sumdb proxy (`go-cache-plugin serve --modproxy`) over
   an S3 prefix. It needs `goModproxy.s3.bucket` and `region`, the
   runner namespaces allowed to reach it in
   `goModproxy.networkPolicy.consumerNamespaces`, and a ServiceAccount
   (`goModproxy.serviceAccount.name`, default `go-modproxy`) carrying
   the pod's own object-store identity. The identity must exist BEFORE
   the pod starts (EKS Pod Identity injects its environment at pod
   creation). Point runners at it with
   `GOPROXY=http://go-modproxy.<ns>.svc/mod,https://proxy.golang.org,direct`.
   Use a comma, not a pipe: with a comma Go falls through only on
   404/410, so a down proxy fails the job instead of silently costing
   the cache. Module zips and Go toolchain downloads both travel through
   it, so a cluster's egress to the public proxy shrinks to cache misses.
   Optional, beside or instead of either proxy: `cacheRedirect`
   (`cacheRedirect.enabled: true`), one service for the Nix cache and the
   Go module proxy whose hits on NARs and module zips are redirects to
   presigned URLs of its bucket, so the bytes flow from the object store
   to the runner directly
   ([architecture](architecture.md#cache-redirect-a-hit-is-a-redirect-not-a-stream)).
   It needs `cacheRedirect.s3.bucket` and `region`,
   `cacheRedirect.networkPolicy.consumerNamespaces`, an identity on its
   ServiceAccount (`cacheRedirect.serviceAccount.name`, default
   `cache-redirect`; runneridentity's `CacheRedirect` builds the AWS role,
   for one prefix both `cacheRedirect.nix.prefix` and
   `cacheRedirect.goproxy.prefix` must sit under), and a route from the
   runners to the bucket's endpoint. Point runners at it with
   `substituters = http://cache-redirect.<ns>.svc.cluster.local/nix https://cache.nixos.org/`
   and
   `GOPROXY=http://cache-redirect.<ns>.svc.cluster.local/goproxy|https://proxy.golang.org,direct`.
6. For persistent Nix workers:
   - an OpenBao SSH user CA (secrets engine mount, default `ssh`) with a
     signing role (default `ci-nix`) that allows the principal `ci-nix`
     and caps certificates at one hour; its public keys go to
     `nixWorkers.ssh.trustedUserCAKeys` here and to
     `nixBuilders.openbao.sshCAPublicKeys` in each runner release;
   - per organization, an OpenBao JWT login role on the environment's
     auth mount, bound exactly to the runner ServiceAccount, whose token
     may only sign on that role;
   - an OpenBao SSH HOST CA (`nixWorkers.openbao.hostSshMount`/
     `hostSshRole`, `cert_type=host`) with a login role for the workers'
     own ServiceAccount (`nixWorkers.openbao.*`); its public key goes to
     each runner release as a `nixBuilders.knownHosts.certAuthorities`
     entry for the worker Service names.

   Each runner generates its client key inside its own pod and receives a
   short-lived certificate; each worker generates its HOST key inside its
   own pod and receives a host certificate. No client private key, host
   private key, `authorized_keys` or `known_hosts` Secret exists. Do not
   add engineer keys or expose worker Services outside the cluster. See
   [architecture.md](architecture.md#worker-host-certificates-and-ephemeral-host-keys).

   **Without host certificates** (`nixWorkers.hostCertificate.enabled:
   false`, the explicit static-key path) the workers need a stable host
   key instead — a Secret named by `nixWorkers.ssh.existingSecret`,
   delivered by the estate's secret manager, containing
   `ssh_host_ed25519_key` — and each runner release pins it:
   `nixBuilders.knownHosts.pinned: true` with a `known_hosts` Secret
   (`nixBuilders.knownHosts.existingSecret`) for both worker Service
   FQDNs.

## Placement and sizes: node classes and sizes

Neither chart knows a node pool, a label, a taint or a runner size of any
platform. Two maps, which you define, carry them.

### Node classes: where a pod goes

`nodeClasses` maps a name you choose to a `nodeSelector`, `tolerations`
and (arc-runners) `affinity`. Everything else refers to the name:
`nodeClass` (the whole release), `scaleSets.<name>.nodeClass`
(arc-runners), `buildkitd.scheduling.<arch>.nodeClass` and
`nixWorkers.scheduling.<arch>.nodeClass` (ci-builders). The tier presets
refer to class names only:

| chart | preset | class names it refers to |
|---|---|---|
| arc-runners | `presets/preview.yaml` | `preview-runners` |
| arc-runners | `presets/stable.yaml` | `stable-runners` |
| ci-builders | `presets/preview.yaml`, `presets/stable.yaml` | `builders-arm64`, `builders-amd64`, `cache-plane` |
| ci-builders | `presets/nix-workers.yaml` | `nix-workers-arm64`, `nix-workers-amd64` |

A class a preset or a values file names but you did not define is a render
error, so a typo cannot schedule runners anywhere. A class that places
nothing is defined empty (`cache-plane: {}`). The chart adds
`kubernetes.io/arch` to a builder's selector itself, so a class carries
the pool, not the architecture.

- **Talos, kind, fixed nodes.** Map a class to labels you put on the
  nodes (Talos: `machine.nodeLabels`, and `registerWithTaints` for a taint),
  or to nothing. [examples/talos](../examples/talos) is a complete
  arc-runners and ci-builders values pair; CI renders both and fails if the
  render mentions Karpenter or EKS.
- **EKS with Karpenter or Auto Mode.** Map a class to the pool's label and
  taint, e.g. `nodeClasses.preview-runners.nodeSelector:
  {karpenter.sh/nodepool: <pool>}`, and list `presets/karpenter.yaml`.

An explicit release-wide `nodeSelector` / `tolerations` / `affinity`, when
not empty, wins over the release-wide class's own for that one key. A scale
set's own `nodeClass` replaces the release-wide placement outright; a
`nodeSelector`, `tolerations` or `affinity` written on the set wins over its
class's.

### Sizes: how big a runner or a builder is

`sizes` maps a name you choose to what a size means; the chart ships none
and its schema requires none. For arc-runners a size is `gomaxprocs`,
`resources` (requests and limits, the ephemeral-storage limit included) and
`maxRunners`, and a scale set picks one with `size:`. For ci-builders it is
`resources` and `maxParallelism` (buildkitd) or `resources`, `maxJobs` and
`cores` (nixWorkers), picked with `buildkitd.size` / `nixWorkers.size`.

An example taxonomy for arc-runners (any names, any numbers: size them from
the peak memory your jobs print, with memory request equal to limit):

```yaml
sizes:
  small:                  # linters, small tests
    gomaxprocs: 2
    maxRunners: 40
    resources:
      requests: {cpu: "1", memory: 6Gi}
      limits: {cpu: "2", memory: 6Gi, ephemeral-storage: 20Gi}
  medium:                 # a typical Go or JS build
    gomaxprocs: 4
    maxRunners: 18
    resources:
      requests: {cpu: "2", memory: 14Gi}
      limits: {cpu: "4", memory: 14Gi, ephemeral-storage: 30Gi}
  large:                  # cold monorepo builds
    gomaxprocs: 4
    maxRunners: 12
    resources:
      requests: {cpu: "2", memory: 24Gi}
      limits: {cpu: "4", memory: 24Gi, ephemeral-storage: 40Gi}
scaleSets:
  lint: {size: small}
  build: {size: medium, minRunners: 0}
```

A key a scale set writes itself wins over its size's (`resources` merge key
by key). The tier presets refer to the sizes `small`, `medium` and `large`
(`presets/preview.yaml`) and `large` (`presets/stable.yaml`); define them or
write your own `scaleSets`. In ci-builders a size REPLACES the component's own
value for each key it sets, because the chart's defaults for those keys
cannot be told from yours.

### Moving from the v6 presets

v6's presets carried a Karpenter pool, its taint and the sizes. v7's carry
names. To keep v6's render byte for byte: state the old pool as a class and
the old numbers as sizes (the tables above name them), and list
`presets/karpenter.yaml` for `karpenter.sh/do-not-disrupt` (and, for
ci-builders, `storageClassName: gp3`). The chart's three `preview-*` scale
sets are no longer a default: they come with `presets/preview.yaml`, or
you write them. The release's `scaleSets` was a map with three inherited
entries; it is now empty, so nothing needs deleting with null.

### The Karpenter preset

`presets/karpenter.yaml` (one per chart) is the opt-in for the one
assumption a platform adds beyond placement:

| chart | what `presets/karpenter.yaml` sets |
|---|---|
| arc-runners | `podAnnotations: {karpenter.sh/do-not-disrupt: "true"}` |
| ci-builders | `buildkitd`, `nixWorkers` and `goModproxy` `podAnnotations` the same; `storageClassName: gp3` (EKS's EBS class) |

An empty `storageClassName` renders no `storageClassName` field on a PVC
at all, which Kubernetes reads as "use the cluster's default
StorageClass" — not the same as an explicit empty string, which
Kubernetes reads as "no class".

### Presets: the same inputs, as opt-in files

Tier presets are plain values files under `presets/`, so Argo CD takes them
as `helm.valueFiles` (with `helm.valuesObject` after them for the estate's
own facts, which win) and Helm as `-f`:

| chart | preset | what it sets |
|---|---|---|
| arc-runners | `presets/preview.yaml` | fairness limit 16, class `preview-runners`, three scale sets (`preview-large`/`-medium`/`-small`) on sizes `large`/`medium`/`small` |
| arc-runners | `presets/stable.yaml` | class `stable-runners`, one fat cold `stable` set on size `large` |
| arc-runners | `presets/nix-builders.yaml` | the Nix remote builders on, without the estate's hosts, CA and OpenBao routing |
| arc-runners, ci-builders | `presets/karpenter.yaml` | the Karpenter assumptions above; opt-in |
| ci-builders | `presets/preview.yaml` | buildkitd per arch on classes `builders-arm64`/`-amd64`, 10Gi, `maxParallelism`, all caches on |
| ci-builders | `presets/stable.yaml` | buildkitd on the same classes, pull-through caches only, a sized nix cache |
| ci-builders | `presets/nix-workers.yaml` | the persistent Nix workers on, CI-only login, host certificates |

A preset holds no registry, organization, CA key, OpenBao address,
namespace or bucket: those stay the caller's values, and the chart still
refuses to render without the ones it requires.

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
`buildkitd.archs`, `buildkitd.scheduling.<arch>` (a `nodeClass`, or
nodeSelector + tolerations per arch, REPLACING the default when set),
`nixWorkers.scheduling.<arch>`, `nodeClasses`, `sizes`, `storageClassName`
(empty by default; see "Placement and sizes" above),
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

## One scale set that needs to differ

`scaleSets.<name>.{nodeSelector,tolerations,affinity,minRunners,
podAnnotations,extraEnv,extraEnvFrom,extraNixConfig,nixSandbox}`
(v4.1.0) override the release-wide value of the same name for ONE scale
set, falling back to it otherwise — no second release needed for a
differently-scheduled or differently-sandboxed set any more. See the
comments beside each release-wide value in `values.yaml`, and
[nix-sandbox.md](nix-sandbox.md) if the different set also needs Nix's
own build sandbox (single-user Nix, which this image ships, has no
sandbox without extra pod/node plumbing this chart can now render its
half of).

## Runner pod expectations

- One ephemeral pod per job; a warm runner per set (`minRunners: 1`)
  removes the cold-start window that upstream ARC#4307 turns into a
  permanent stall.
- Run CI pools on-demand, not spot: an estate that sets `podAnnotations`
  to `karpenter.sh/do-not-disrupt: "true"` (`presets/karpenter.yaml`;
  empty by default) stops consolidation, not reclaims.
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
- `runner.watchdog.enabled: true` (off by default) runs the runner under
  `runner-watchdog`, which reports a stuck runner as a failing readiness
  probe. The janitor then also deletes an idle runner whose `runner`
  container is NotReady. It needs a runner image from the same release
  or later. See docs/architecture.md ("Runner watchdog").

## A Go cache credential with no pod identity

`extraEnvFrom`/`extraEnv` above are for a store the runner reaches with
a long-lived key in a Secret. For a short-lived one instead —
Cloudflare R2 minted by the Cloudflare module of sluis, through
`sluisctl cloudflare r2 <preset>` (truvity/sluis, baked into this
image) — set `awsConfig` and grant the calling workflow
`id-token: write`; see
[architecture.md#an-aws-profile-for-a-credential-helper-v420](architecture.md#an-aws-profile-for-a-credential-helper-v420)
for the worked example and what it renders.

## Wiring workflows

Point `runs-on` at the scale-set names (via org variables so a rename
is one change, not N). Runners reach the caches by cluster DNS:
`buildkitd-<arch>.<namespace>.svc:1234` (buildx `--driver remote`),
`nix-cache.<namespace>.svc` (nix substituter),
`bazel-remote.<namespace>.svc:9092` (moon remote cache),
`npm-cache.<namespace>.svc` (yarn/npm/pnpm `npmRegistryServer`, public
packages, behind a health-probe fallback to registry.npmjs.org).
