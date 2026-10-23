# Day 1 — installing the CI plane on a fresh estate

Order matters: the builders and caches before the runners that use them.

This guide installs `ci-builders`. The `arc-runners` chart (the ARC
runner scale sets) is archived, last released as 7.8.3; its install
steps are in
[this guide at `v7.8.3`](https://github.com/truvity/ci-plane/blob/v7.8.3/docs/day-1-install.md).

## Prerequisites

1. **A namespace for the build plane**, written `<namespace>` below,
   and the **runner namespaces** allowed to reach it. The chart takes
   the namespace from the release; two things outside it still assume
   the maintainers' name `ci-cache`, or want a cache URL:
   - the published runner and nix-worker images list only
     `https://cache.nixos.org/` as their Nix substituter (v7.0.0; earlier
     images baked an in-cluster name). To use the cache-redirect `/nix`
     front-end, give the runners a `NIX_CONFIG` with
     `substituters = http://cache-redirect.<namespace>.svc.cluster.local/nix https://cache.nixos.org/`,
     which beats the image's own nix.conf;
   - truvity/ci-workflows' `node-cache: true` probes
     `npm-cache.ci-cache.svc` by that fixed name.
2. Optional but recommended: a **registry pull-through cache** per
   hosting account, so image pulls are same-region and unthrottled. The
   charts' image references are split `{registry}/{repository}` so you
   override only the registry.
3. Optional: `cacheRedirect`
   (`cacheRedirect.enabled: true`), one service for the Nix cache, the
   Go module proxy, the Maven repositories and the npm registry (and, opt-in, a Gradle
   remote build cache) whose hits on NARs, module zips, jars and npm tarballs are
   redirects to presigned URLs of its bucket, so the bytes flow from the object store
   to the runner directly
   ([architecture](architecture.md#cache-redirect-a-hit-is-a-redirect-not-a-stream)).
   It needs `cacheRedirect.s3.bucket` and `region`,
   `cacheRedirect.networkPolicy.consumerNamespaces`, an identity on its
   ServiceAccount (`cacheRedirect.serviceAccount.name`, default
   `cache-redirect`; runneridentity's `CacheRedirect` builds the AWS role,
   for one prefix `cacheRedirect.nix.prefix`, `.goproxy.prefix`,
   `.maven.prefix`, `.gradle.prefix` and `.npm.prefix` must all sit under: the Maven,
   Gradle and npm defaults, `maven/`, `gradle/` and `npm/`, do not), and a route from the
   runners to the bucket's endpoint. Point runners at it with
   `substituters = http://cache-redirect.<ns>.svc.cluster.local/nix https://cache.nixos.org/`
   and
   `GOPROXY=http://cache-redirect.<ns>.svc.cluster.local/goproxy|https://proxy.golang.org,direct`.
   Gradle and Maven builds use `.../maven/central` (and `/google`,
   `/gradle-plugins`) as their repositories; the Gradle build cache, a
   WRITABLE cache and so off by default (`cacheRedirect.gradle.enabled`,
   on in `presets/preview.yaml` and `presets/stable.yaml`, each tier its own
   deployment and bucket), is `HttpBuildCache` with the url
   `http://cache-redirect.<ns>.svc.cluster.local/gradle/cache/`. npm, yarn and
   pnpm use `http://cache-redirect.<ns>.svc.cluster.local/npm` as their
   registry (`cacheRedirect.npm.enabled`, read-only, on in both presets; it
   replaces the verdaccio of `npmCache`). Behind a
   gateway that routes by name, `cacheRedirect.hosts.{nix,maven,gradle,npm}`
   serve each of those at the root of a host name of its own, so the gateway
   rewrites no path.
4. For persistent Nix workers:
   - an OpenBao SSH user CA (secrets engine mount, default `ssh`) with a
     signing role (default `ci-nix`) that allows the principal `ci-nix`
     and caps certificates at one hour; its public keys go to
     `nixWorkers.ssh.trustedUserCAKeys` here;
   - per runner pool, an OpenBao login role bound exactly to the
     runners' identity, whose token may only sign on that role;
   - an OpenBao SSH HOST CA (`nixWorkers.openbao.hostSshMount`/
     `hostSshRole`, `cert_type=host`) with a login role for the workers'
     own ServiceAccount (`nixWorkers.openbao.*`); its public key goes
     into the runners' `known_hosts` as an `@cert-authority` line for the
     worker Service names.

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
   `ssh_host_ed25519_key` — and the runners pin it in a `known_hosts`
   for both worker Service FQDNs.

## Placement and sizes: node classes and sizes

The chart knows no node pool, label, taint or builder size of any
platform. Two maps, which you define, carry them.

### Node classes: where a pod goes

`nodeClasses` maps a name you choose to a `nodeSelector` and
`tolerations`. Everything else refers to the name: `nodeClass` (the whole
release), `buildkitd.scheduling.<arch>.nodeClass` and
`nixWorkers.scheduling.<arch>.nodeClass`. The tier presets refer to class
names only:

| preset | class names it refers to |
|---|---|
| `presets/preview.yaml`, `presets/stable.yaml` | `builders-arm64`, `builders-amd64`, `cache-plane` |
| `presets/nix-workers.yaml` | `nix-workers-arm64`, `nix-workers-amd64` |

A class a preset or a values file names but you did not define is a render
error, so a typo cannot schedule a builder anywhere. A class that places
nothing is defined empty (`cache-plane: {}`). The chart adds
`kubernetes.io/arch` to a builder's selector itself, so a class carries
the pool, not the architecture.

- **Talos, kind, fixed nodes.** Map a class to labels you put on the
  nodes (Talos: `machine.nodeLabels`, and `registerWithTaints` for a taint),
  or to nothing. [examples/talos](../examples/talos) is a complete
  values file; CI renders it and fails if the render mentions Karpenter or
  EKS.
- **EKS with Karpenter or Auto Mode.** Map a class to the pool's label and
  taint, e.g. `nodeClasses.builders-arm64.nodeSelector:
  {karpenter.sh/nodepool: <pool>}`, and list `presets/karpenter.yaml`.

An explicit release-wide `nodeSelector` / `tolerations`, when not empty,
wins over the release-wide class's own for that one key.

### Sizes: how big a builder is

`sizes` maps a name you choose to what a size means; the chart ships none
and its schema requires none. A size is `resources` and `maxParallelism`
(buildkitd) or `resources`, `maxJobs` and `cores` (nixWorkers), picked
with `buildkitd.size` / `nixWorkers.size`. A size REPLACES the component's
own value for each key it sets, because the chart's defaults for those
keys cannot be told from yours. [examples/talos](../examples/talos) has a
taxonomy.

### Moving from the v6 presets

v6's presets carried a Karpenter pool, its taint and the sizes. v7's carry
names. To keep v6's render byte for byte: state the old pool as a class and
the old numbers as sizes, and list
`presets/karpenter.yaml` for `karpenter.sh/do-not-disrupt` and
`storageClassName: gp3`.

### The Karpenter preset

`presets/karpenter.yaml` is the opt-in for the one assumption a platform
adds beyond placement: `buildkitd` and `nixWorkers` `podAnnotations:
{karpenter.sh/do-not-disrupt: "true"}`, and `storageClassName: gp3`
(EKS's EBS class).

An empty `storageClassName` renders no `storageClassName` field on a PVC
at all, which Kubernetes reads as "use the cluster's default
StorageClass" — not the same as an explicit empty string, which
Kubernetes reads as "no class".

### Presets: the same inputs, as opt-in files

Tier presets are plain values files under `presets/`, so Argo CD takes them
as `helm.valueFiles` (with `helm.valuesObject` after them for the estate's
own facts, which win) and Helm as `-f`:

| preset | what it sets |
|---|---|
| `presets/karpenter.yaml` | the Karpenter assumptions above; opt-in |
| `presets/preview.yaml` | buildkitd per arch on classes `builders-arm64`/`-amd64`, 10Gi, `maxParallelism`, all caches on |
| `presets/stable.yaml` | buildkitd on the same classes, pull-through caches only, a sized nix cache |
| `presets/nix-workers.yaml` | the persistent Nix workers on, CI-only login, host certificates |

A preset holds no registry, organization, CA key, OpenBao address,
namespace or bucket: those stay the caller's values, and the chart still
refuses to render without the ones it requires.

## Install the cache plane

```bash
helm install ci-builders oci://ghcr.io/truvity/charts/ci-builders \
  --version <X.Y.Z> -n <namespace> \
  --set buildkitd.networkPolicy.consumerNamespaces={<runner namespace>} \
  --set npmCache.networkPolicy.consumerNamespaces={<runner namespace>} \
  --set nixWorkers.enabled=true \
  --set-string 'nixWorkers.ssh.trustedUserCAKeys[0]=ssh-ed25519 <CA-PUBLIC-BODY>' \
  --set nixWorkers.networkPolicy.consumerNamespaces={<runner namespace>} \
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
(empty by default; see "Placement and sizes" above). A Nix worker is privileged so
its daemon can create Linux sandboxes: enabling it without selecting a
dedicated, tainted CI/build pool is not a supported production shape.

## Wiring workflows

Runners in the cluster reach the caches by cluster DNS (from outside it,
buildkitd over mutual TLS, [mtls.md](mtls.md)):
`buildkitd-<arch>.<namespace>.svc:1234` (buildx `--driver remote`),
`cache-redirect.<namespace>.svc/nix` (nix substituter), `/goproxy` (GOPROXY) and `/maven/<repo>` (Gradle and Maven repositories),
`bazel-remote.<namespace>.svc:9092` (moon remote cache; the port declares `appProtocol: kubernetes.io/h2c`, so a gateway can publish it over an HTTPRoute),
`npm-cache.<namespace>.svc` (yarn/npm/pnpm `npmRegistryServer`, public
packages, behind a health-probe fallback to registry.npmjs.org).
