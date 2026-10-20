# Changelog

What changed for a consumer, per release. One tag releases everything —
both images and both charts — at one version, so each heading covers all
four. Reconstructed from the history for v2.0.0 through v2.11.0; the 1.x
line is summarised in one section.

## v7.2.0

### Added: runner CA certificates, supplied at runtime

- **arc-runners: `extraCACertificates` (off by default).** Names a ConfigMap (`configMapName`) or a Secret (`secretName`) whose `*.pem` / `*.crt` certificates every job on the runner trusts, for an HTTPS endpoint under a private CA. Optional `keys` picks which keys to mount (default: all); `mountPath` defaults to `/var/run/extra-ca`. Off, a render is byte-identical to before. On, the runner container mounts the source read-only, sets `CI_EXTRA_CA_DIR` and starts behind the new `runner-extra-ca` (in front of `runner-watchdog` when that is on). Rendering refuses naming both sources, and a key that does not end in `.pem` or `.crt`. **Adoption:** run a runner image of this release or later first (an older image has no `runner-extra-ca`, so the container cannot start), then set the value.
- **Runner image: `/usr/local/bin/runner-extra-ca`.** An entrypoint prefix. When `CI_EXTRA_CA_DIR` names a directory it builds a bundle of the system roots plus the certificates in it, as the non-root `runner` user, and exports `SSL_CERT_FILE`, `NIX_SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE` and `NODE_EXTRA_CA_CERTS` before `exec`ing the runner. With the variable unset it does nothing. The image still contains no CA. See docs/architecture.md ("Runner CA certificates").

### Added: mutual-TLS BuildKit builders

- **ci-builders: mutual-TLS BuildKit builders (`buildkitd.mtls`, off by default).** A second, TLS-only StatefulSet and Service per architecture, `buildkitd-mtls-<arch>`, beside the plain `buildkitd-<arch>`. buildkitd applies its TLS settings to every `tcp://` listener of the process, so plain and mTLS cannot share a daemon, and two daemons cannot share a cache; the new builders have a cache of their own. The server certificate is a cert-manager `Certificate` from an issuer and names you give (`mtls.serverCertificate`); clients are verified against one dedicated client CA (`mtls.clientCA`), which the chart can build (`create: true`: CA `ClusterIssuer`, `CertificateRequestPolicy`, trust-manager `Bundle`). `mtls.retirePlain: true` stops rendering the plain builders once nothing uses them. With the key off, every render is byte-identical. See [docs/mtls.md](docs/mtls.md).
- **arc-runners: `buildkitTLS` (off by default).** Mounts a client certificate (a Secret, optionally the `Certificate` that produces it) and a trust bundle at `buildkitTLS.mountPath` as `ca.pem`, `cert.pem` and `key.pem`, and sets `CI_BUILDKIT_TLS_DIR` on the runner container (and `CI_BUILDKIT_TLS_SERVERNAME` from `buildkitTLS.serverName`, when set).

## v7.1.0

### Pulumi package: `deploy/pulumi/runneridentity` v0.3.0 (breaking)

- **Removed: `WithLegacyTopLevel`.** It set truvity/k8s podidentity's `LegacyTopLevel`, an alias from the URN a child had before it was wrapped in the component. A stack applied with the component holds the component URNs, so the alias resolves to nothing. Apply every stack that passes it with your current version, then drop the option: a preview shows no create or delete. Needed before truvity/k8s removes `podidentity.Args.LegacyTopLevel`. `WithArgs` can still set any podidentity field while it exists.

## v7.0.2

### Changed

- **Images: Nix is installed with its seccomp filter on.** The runner and nix-worker images no longer set `filter-syscalls = false` for the Nix install step. It existed only because the arm64 half used to build under QEMU user emulation, where Nix's seccomp BPF load fails; both architectures build natively now. The baked `nix.conf` is unchanged (it never carried the setting). Pull-request CI now builds every base on a native arm64 runner too (`image-arm64`), so an arm64 regression fails before a release. The nix-worker seed step (`nix-worker-seed.sh`, a `nix copy` between two local stores at pod start) keeps its own `filter-syscalls = false`; it is not a build and was not added for QEMU.

## v7.0.1

### Security / Fixed

- arc-runners: runner pods never automount the service-account token (previously only with nixBuilders on)

## v7.0.0

### The superseded caches are removed: `nixCache` and `goModproxy`

- **Breaking: ci-builders' `nixCache` is removed.** The nginx read-through cache of cache.nixos.org (its Deployment, Service, the `nix-cache-data` PVC, NetworkPolicy and ConfigMap) is gone; the cache-redirect `/nix` front-end replaces it. A release that still sets any `nixCache.*` key fails to render with a message naming the replacement. **Adoption:** enable `cacheRedirect` (`cacheRedirect.nix`), delete the `nixCache` block, and point the runners at `http://cache-redirect.<namespace>.svc.cluster.local/nix` through arc-runners' `nixCache.url` (that arc-runners value stays). A PVC may outlive the release (retained by some installers): delete `nix-cache-data` yourself once nothing uses it.
- **Breaking: ci-builders' `goModproxy` is removed.** The go-cache-plugin module proxy component (Deployment, Service, ServiceAccount `go-modproxy`, NetworkPolicy) is gone; the cache-redirect `/goproxy` front-end replaces it. A release that still sets any `goModproxy.*` key fails to render with a message naming the replacement. **Adoption:** enable `cacheRedirect` (`cacheRedirect.goproxy`), delete the `goModproxy` block, and set `GOPROXY=http://cache-redirect.<namespace>.svc.cluster.local/goproxy|https://proxy.golang.org,direct`. `presets/karpenter.yaml` and `presets/stable.yaml` no longer carry `goModproxy` / `nixCache` entries.
- **Changed: the runner and nix-worker images' `NIX_SUBSTITUTERS` default is `https://cache.nixos.org/` only** (it was an in-cluster `nix-cache` Service name first). A fleet that relied on the baked name must set arc-runners' `nixCache.url` (see above) before moving to these images, or every Nix path comes from upstream. The leak canary's allowance for the old ARG is dropped. The runneridentity package is unchanged (its `Modproxy` type stays for now).
- The `go-modproxy` goldens and cases are removed; the other ci-builders goldens lose the nix-cache objects; arc-runners' `nix-cache` case now uses the cache-redirect URL.

### cache-redirect: a rollout no longer refuses connections

- **Fixed: rolling the cache-redirect Deployment made clients fail with "connection refused".** On SIGTERM the server closed its listener at once while the pod's endpoint was still programmed in the nodes' service routing for a few seconds, so a client routed to the terminating pod was refused (the go command does not fall back from `,` on a connection error, so the job failed). The pod now has a `preStop` sleep (`cacheRedirect.drainDelaySeconds`, default 10; 0 renders no hook) that keeps it serving until the endpoint is gone, and `cacheRedirect.terminationGracePeriodSeconds` rises from 30 to 45 so the sleep plus the 25s shutdown fit. The native `sleep` action (the image has no shell) needs Kubernetes 1.30+ (on by default as a beta; GA in 1.34).
- **New: explicit rollout strategy and a PodDisruptionBudget.** The Deployment is `RollingUpdate` with `maxUnavailable: 0`, `maxSurge: 1`, so a ready pod is never removed before its replacement is ready. `cacheRedirect.podDisruptionBudget` (`enabled: true`, `minAvailable: 1`) renders a PodDisruptionBudget only when `replicas` is above 1. The `cache-redirect` and `cache-redirect-s3-compatible` goldens move accordingly.
- **Changed (docs): the recommended `GOPROXY` is `http://cache-redirect.<ns>.svc.cluster.local/goproxy|https://proxy.golang.org,direct`.** The `|` makes the go command fall back to the next entry on ANY error, including a refused connection; keep the `,` before `direct`.

### Placement and sizes are data you define; no node pool is named

The presets named one platform's node pools (`ci`, `ci-stable`, `build`, `platform`, `volatile`) and the chart shipped three runner sizes. Both are now the caller's: the chart works the same on EKS with Karpenter, on Talos with fixed arm64 nodes and Cilium, and on kind.

- **Breaking: the presets name node classes and sizes, not pools.** `presets/preview.yaml` and `presets/stable.yaml` of both charts no longer carry `nodeSelector`, `tolerations`, `listenerTolerations`, `podAnnotations`, `storageClassName` or a `scheduling` block with a pool label. They refer to class names you define under `nodeClasses` (arc-runners: `preview-runners`, `stable-runners`; ci-builders: `builders-arm64`, `builders-amd64`, `cache-plane`, and for `presets/nix-workers.yaml` `nix-workers-arm64`, `nix-workers-amd64`) and arc-runners' to size names you define under `sizes` (`large`, `medium`, `small`). A name left undefined is a render error naming it. **Adoption:** state the old pool as a class and the old numbers as sizes (docs/day-1-install.md, "Moving from the v6 presets") and list `presets/karpenter.yaml`; the render is then byte for byte v6's (the goldens `preset-preview`, `preset-preview-nix-builders`, `preset-stable` and the ci-builders ones are unchanged by this release).
- **New: `nodeClasses` (both charts).** A map from a name you choose to `nodeSelector`, `tolerations` and (arc-runners) `affinity`. Referred to by `nodeClass` (the release; arc-runners' runner pods, ci-builders' caches and proxies), `scaleSets.<name>.nodeClass` (arc-runners), `buildkitd.scheduling.<arch>.nodeClass` and `nixWorkers.scheduling.<arch>.nodeClass` (ci-builders, which adds `kubernetes.io/arch` itself). A class that places nothing is defined empty. An explicit `nodeSelector` / `tolerations` / `affinity`, where not empty, wins over the class's for that key. The release-wide class is not part of a scale set's values-hash, as the release-wide `nodeSelector` never was; a set's own `nodeClass` is, like its own `nodeSelector`.
- **New: `sizes` (both charts).** A user-defined taxonomy, any names, no default and none required by the schema. arc-runners: `gomaxprocs`, `resources` (the ephemeral-storage limit included) and `maxRunners`, picked by `scaleSets.<name>.size`; keys the set writes win, `resources` merging key by key. ci-builders: `resources` and `maxParallelism` (`buildkitd.size`), `resources`, `maxJobs` and `cores` (`nixWorkers.size`); a size replaces the component's value for each key it sets. The size is resolved before the values-hash, so moving a set's inline numbers into a size recycles no warm runner. An example taxonomy is in docs/day-1-install.md.
- **Breaking: arc-runners ships no scale sets.** `scaleSets` defaults to `{}` (it was `preview-large`, `preview-medium`, `preview-small`, so a caller's own set made four). `presets/preview.yaml` now carries those three sets, on sizes `large`, `medium`, `small`; a release that listed no preset and relied on the three must add the preset or write them (a set may be an empty block). A set without `size` or `resources` renders no `resources` block, and without `gomaxprocs` no `GOMAXPROCS` variable. `name: null` entries in `scaleSets` are skipped instead of failing.
- **New: `presets/karpenter.yaml` (both charts), opt-in.** What was Karpenter-only in the old presets: `karpenter.sh/do-not-disrupt` (arc-runners `podAnnotations`; ci-builders `buildkitd`, `nixWorkers` `podAnnotations`) and ci-builders' `storageClassName: gp3`. Nothing else in the charts assumes Karpenter or EKS.
- **New: examples/talos and the `talos` golden cases.** A complete render-only values pair for fixed arm64 nodes with no autoscaler, built on classes and sizes; `just golden` fails if either render mentions Karpenter, EKS or a node pool label. New cases `sizes-and-classes` (both charts), the `base.yaml` the old default sets moved to for the cases that do not list a preset, and the refusals `unknown-size`, `unknown-node-class`, `unknown-release-node-class` (arc-runners), `unknown-size`, `unknown-node-class`, `size-key-not-for-buildkitd` (ci-builders).

## v6.7.1

### cache-redirect: logged request values are single-line

- **Changed: request-derived values (object keys, upstream URLs) are passed through `logSafe` before they are logged.** Line breaks become spaces in the logged copy only; storage keys and lookups are untouched. The JSON log handler already escapes control characters, so there is no behaviour change; this keeps entries on one line under any handler and satisfies the code scanner's log-injection check.

## v6.7.0

### runner: git-lfs is in the image

- **New: `git-lfs` in the runner image**, so `actions/checkout` with `lfs: true` works on ARC runners as on GitHub-hosted runners (which ship it); without it the checkout failed with "Unable to locate executable file: git-lfs" (truvity/docs' release job). It counts as part of git, not as a tool devbox should deliver: the checkout action execs it before any devbox environment exists. A static release binary pinned by `GIT_LFS_VERSION` (v3.8.0, annotated for Renovate) and verified against the release's `sha256sums.asc`; `git lfs install --system` writes the filters to `/etc/gitconfig`. The `image` CI job now runs the built runner image as the `runner` user and asserts `git lfs version` and the system filters. Requires re-pinning `ciRunnerImage` to the digest of the release that carries it.

## v6.6.2

### arc-runners: the runner nix ConfigMap is named by its full content

- **Fixed: setting `nixCache.url` with `nixBuilders` off changed an immutable ConfigMap in place.** The `runner-nix-config-<hash>` ConfigMap is immutable, but with `nixBuilders.enabled: false` its name hashed `nixConfig` alone, while the rendered `nix.conf` also carries the `substituters = <nixCache.url> https://cache.nixos.org/` line. Adding or changing `nixCache.url` therefore rewrote the data under the same name and Argo CD failed with `field is immutable when immutable is set`. The name now hashes the whole rendered `nix.conf` in every branch (with builders on it already covered the whole payload). A render without `nixCache.url` keeps its name (the rendered text is `nixConfig` verbatim), so only a release that sets `nixCache.url` gets a new name: one rollout of the listener and runner template, harmless with `minRunners: 0`. The `nix-cache` golden moves to the new name, and `hack/golden.sh` now fails if `minimal` and `nix-cache` (which differ only in `nixCache.url`) share a name.

## v6.6.1

### cache-redirect: NARs are now cached (verified against their narinfo)

- **Fixed: the Nix front-end never stored a NAR.** It verified the body against the hash in the NAR's file name, which on cache.nixos.org is not the hash of the compressed body, so every NAR was streamed through and discarded (and counted as `upstream_error`). The file-name check is removed (and with it the `-nix-verify-nar-hash` flag). Serving a narinfo now records its `URL`, `FileHash` and `FileSize` in an index record at `<nix prefix>nar-index/<URL path>.json` in the bucket (plus memory; replicas share it through the bucket). A NAR miss is committed only when the body's size and SHA-256 match the record; a mismatch is served but not stored, and a NAR with no record (requested without a narinfo through this cache) is passed through and not stored. New `cache_redirect_requests_total` results: `verify_failed` and `no_index`, never `upstream_error`. A truncated body still aborts as before. See docs/architecture.md ("cache-redirect").

## v6.6.0

### cache-redirect: a Nix and Go cache whose hits are redirects (`cacheRedirect`)

- **New image: `ghcr.io/truvity/ci-plane/cache-redirect`** (image/cache-redirect, its own Go module; a static binary on distroless, built and published by every release like the other two, digest stamped into `cacheRedirectImage`). A pull-through cache for the Nix binary cache (`/nix`) and the GOPROXY protocol (`/goproxy`) that keeps its index in an S3 or S3-compatible bucket and answers a hit on a large immutable object (a NAR, a module `.zip`) with a 302 to a presigned GET URL (`-presign-ttl`, default 5m), so the bytes flow from the object store to the runner and never through the pod. Small objects (narinfo, `.info`, `.mod`, sumdb tiles) are served directly. A miss is fetched from upstream once per key (concurrent requests wait for that one fetch), streamed to its client while uploaded, and committed only when the upstream body is whole (its `Content-Length`, and a NAR's file hash from its name); a truncated or failed fetch is aborted and never stored. Upstream 404/410 reach the client unchanged and are never stored; a store 403 is a 502, never a miss. Go's mutable endpoints (`@v/list`, `@latest`, a non-canonical `<query>.info`, sumdb `lookup`/`latest`) are passed through with a 30s in-memory TTL; the checksum database `sum.golang.org` is proxied (tiles stored). Metrics on `/metrics`: `cache_redirect_requests_total{frontend,result}`, `cache_redirect_uploads_total`, `cache_redirect_upload_bytes_total`, `cache_redirect_upstream_request_duration_seconds`, `cache_redirect_fetches_in_flight`. See docs/architecture.md ("cache-redirect").
- **New: `cacheRedirect` in the ci-builders chart** (default `enabled: false`; an off render is byte-identical to before, every existing golden unchanged). On: a Deployment (`replicas: 2`, one per node by a topology spread constraint), a Service `cache-redirect` (port 80), a ServiceAccount for the pod's identity, the consumer-namespaces NetworkPolicy (plus `networkPolicy.additionalIngress`), and an optional PodMonitor (`monitoring.enabled`). Keys: `s3.bucket`/`region` (required), `s3.endpoint`/`pathStyle`/`existingSecret` (S3-compatible stores), `presignTTL`, `nix.enabled`/`prefix`/`upstream`/`priority` (default `nix/`, cache.nixos.org, 30), `goproxy.enabled`/`prefix`/`upstream`/`sumdb.name`/`sumdb.upstream` (default `goproxy/`, proxy.golang.org, sum.golang.org), `extraArgs`. Clients: `substituters = http://cache-redirect.<ns>.svc.cluster.local/nix https://cache.nixos.org/` and `GOPROXY=http://cache-redirect.<ns>.svc.cluster.local/goproxy,https://proxy.golang.org,direct`; runners need a route to the bucket's endpoint. New goldens `cache-redirect`, `cache-redirect-s3-compatible`; new refusals `cache-redirect-without-bucket`, `cache-redirect-bad-presign-ttl`, `cache-redirect-no-consumer-namespaces`.

### Pulumi package: `deploy/pulumi/runneridentity` v0.2.0

- **New: `CacheRedirect` / `NewCacheRedirect`**, cache-redirect's Pod Identity role, the same shape and options as `Modproxy`: one prefix of a bucket, `s3:ListBucket` limited to `<prefix>/*` (so a missing key is a 404, not a 403) and `s3:GetObject`, `s3:PutObject`, `s3:AbortMultipartUpload`, `s3:ListMultipartUploadParts` on `<bucket>/<prefix>/*`; no delete. Both of the chart's front-end prefixes go under that one prefix.

## v6.5.0

### runner-watchdog: a drill of the stuck-runner remedy (`runner.watchdog.drillFile`)

- **New: `-drill-file PATH`** on `runner-watchdog`, and **`runner.watchdog.drillFile`** in the arc-runners chart (default `""`; unset renders byte-identical to before: the key is left out of the values-hash, every existing golden unchanged). Set, the watchdog feeds itself one simulated stuck line each time that file appears, counted under its own signature `drill` (`runner_watchdog_signature_lines_total{signature="drill"}`), never as a real #4307 one. `kubectl exec <idle runner> -- touch <path>` then exercises the whole remedy: `/healthz` 503 after `grace`, the pod NotReady, the janitor deleting the runner, ARC replacing it, `ARCRunnerStuck` firing. During a job the line is only counted. The schema requires an absolute path. New golden `runner-watchdog-drill`; new refusal `runner-watchdog-drill-relative`. Requires the runner image of this release.

## v6.4.0

### arc-runners chart: `networkPolicy.containment.metricsFrom`, a narrow ingress rule for the metrics agent

- **New: `networkPolicy.containment.metricsFrom`** (`namespaceSelector` and/or `podSelector`; unset by default, and an unset render is byte-identical to before, every existing golden unchanged). The containment policy admits ingress from its own namespace only, so a metrics agent in another namespace cannot reach the runner watchdog's `/metrics` (`monitoring.enabled`). Set, the policy gains ONE ingress rule: TCP on the watchdog's port (`runner.watchdog.port`, default 8091) from exactly that selector, nothing else; with both selectors set they match the same pod (namespace AND pod). At least one selector is required: an empty one would admit every pod, and the schema and the template refuse it. New golden `containment-metrics`.

## v6.3.0

### arc-runners chart: PodMonitors for the runner watchdog and the scale-set listeners (`monitoring`)

- **New: `monitoring.enabled`** (default `false`; an off render is byte-identical to before, every existing golden unchanged). On, the release renders two `monitoring.coreos.com/v1` PodMonitors (the CRD must exist; the VictoriaMetrics operator converts them): `runner-watchdog`, selecting the release namespace's runner pods on the container port named `watchdog` (rendered only while `runner.watchdog.enabled`), and `scale-set-listeners`, selecting this release's listener pods in the controller's namespace (`controllerServiceAccount.namespace`) on the container port named `metrics`. Both copy the `actions.github.com/scale-set-name` pod label onto the series. Keys: `monitoring.interval`, `monitoring.labels`, `monitoring.watchdog.enabled`, `monitoring.listener.enabled`, `monitoring.listener.path` (default `/metrics`).
- **The listeners export nothing until the controller chart's `metrics:` is set** (`controllerManagerAddr`, `listenerAddr`, `listenerEndpoint`; ARC 0.14.2 and 0.15.0). This chart does not install the controller, so it cannot set it. New golden `monitoring`.

## v6.2.0

### arc-runners chart: the janitor runs every minute and reads the readiness verdict

- **`janitor.schedule` defaults to `* * * * *`** (was every 3 minutes). A consumer that sets its own schedule keeps it.
- **New: `janitor.notReadySeconds`** (default 60). The janitor deletes an EphemeralRunner that holds no job when its pod's `runner` container has a readiness probe (the `runner.watchdog` probe), is running, and the pod's Ready condition has been False for at least this long. It keys on the POD: a pod whose runner container has no readiness probe never matches, so with `runner.watchdog.enabled` off this changes nothing. This replaces the earlier `runner.watchdog.enabled` template gate and the age-grace wait for that criterion; the log-signature sweep stays, unchanged, as the backstop. The janitor ConfigMap is now `arc-runner-janitor-v3`.

## v6.1.0

### Runner image and arc-runners chart: the runner watchdog (`runner.watchdog`)

- **New in the runner image: `/usr/local/bin/runner-watchdog`.** An optional entrypoint wrapper for `/home/runner/run.sh` (image/runner-watchdog, standard library only). It runs the runner as its child and copies the runner's output to the log unchanged. It forwards signals to the runner's process group, reaps orphans, and exits with the runner's code. It reports a runner stranded by upstream ARC #4307 on `/healthz` (503 while stuck). STUCK means no job is running and an idle stuck signature (`Skipping message Job. Job message already acquired` / `Job message not found` / `Unprocessable job`, or `Registration <id> was not found`) has persisted for the grace period. Prometheus metrics are on `/metrics`. See docs/architecture.md ("Runner watchdog").
- **New: `runner.watchdog.enabled` / `.port` / `.grace`** in arc-runners. Off by default, and an off render is byte-identical to before. On (port 8091 and grace 45s by default), the runner container runs `runner-watchdog -- /home/runner/run.sh`, exposes a `watchdog` container port, and gets a READINESS probe on `/healthz`. It gets no liveness probe: a restarted runner cannot register again. The janitor then also deletes an idle runner, past its grace period, whose `runner` container is running but NotReady; the log grep stays as the fallback. Enable it only with a runner image from this release or later. A `grace` that is not a Go duration is refused by the schema.
- **The runner image is on actions/runner 2.338.0** (was 2.337.0).

## v6.0.1

### Pulumi package: `deploy/pulumi/runneridentity` v0.1.1

- **Fix: the module declares `go 1.27.0` with `toolchain go1.27.2`** (v0.1.0
  said `go 1.27.2`), so a consumer on an older `go` line is not forced to
  raise it by adding the package, while the module itself still builds and
  is vulnerability-checked with the fixed standard library.

## v6.0.0

### Runner image: `r2broker` removed

- **Breaking: the runner image no longer ships `r2broker`.** truvity/cloudflare retires its R2 credentials broker; sluis's Cloudflare module mints R2 credentials itself, so `sluisctl cloudflare r2 <preset>` (already in the image) replaces `sluisctl r2 --service-url <broker> -- credentials ...` in an `awsConfig.credentialProcess` line. A caller whose `credentialProcess` still names `sluisctl r2` or `r2broker` must move to an R2 preset first. `docs/architecture.md` ("An AWS profile for a credential helper") shows both shapes: a job signing in with its own GitHub token, and a credential a secrets operator projected (`--file`, mounted through `extraVolumes`). The arc-runners test case `r2-broker` is now `r2-credential-process`. No chart value changes.

### Tier presets: `presets/preview.yaml`, `presets/stable.yaml` and friends

- **New: opt-in values files under each chart's `presets/`.** They ship in
  the chart package, so a consumer lists them ahead of its own values (Argo
  CD: `helm.valueFiles: [presets/stable.yaml]`; Helm: `-f
  presets/stable.yaml -f mine.yaml`); a file listed later wins key by key.
  Nothing changes for a release that lists none.
  - `arc-runners/presets/preview.yaml` and `stable.yaml`: the two tier
    shapes of an AWS/Karpenter fleet (per-set fairness limit, runner pool
    selector and tolerations, listener tolerations, `do-not-disrupt`; the
    stable tier also replaces the three preview profiles with one fat, cold
    `stable` set). `arc-runners/presets/nix-builders.yaml` turns the Nix
    remote builders on, to be listed on the tier whose workers run.
  - `ci-builders/presets/preview.yaml` and `stable.yaml`: buildkitd
    scheduling and sizing per tier, which caches run, `maxParallelism: 4`.
    `ci-builders/presets/nix-workers.yaml` turns the persistent Nix workers
    on with the CI-only SSH login and host-certificate wiring.
  - Estate facts are never in a preset: registries, the organization, CA
    keys, OpenBao routing, consumer namespaces and buckets stay the
    caller's values. Node pool names (`ci`, `ci-stable`, `build`, `platform`,
    `volatile`) are the conventional ones; an estate whose pools are named
    otherwise overrides `nodeSelector`/`tolerations`/`scheduling` (they
    replace, they do not merge).
- **New: `arc-runners` `restricted` (release-wide, default `false`).** The
  default of `scaleSets.<name>.restricted` for every set that does not set
  its own, so a release can be put on the Pod Security `restricted` profile
  without naming sets. A set's own key wins; the values hash a set carries is
  the one an explicit per-set `restricted: true` produced.

### NetworkPolicies an estate used to render itself, now opt-in chart values

- **New: `arc-runners` `networkPolicy.containment`.** Off by default. One
  NetworkPolicy that selects every pod of the release namespace and allows
  cluster DNS, the cache plane's namespace, the namespace itself, the
  network CIDR on 443 (VPC endpoints) and the internet, and nothing else in
  the cluster or the network, nor the cloud metadata address. `serviceCidr`,
  `vpcCidr` and `cacheNamespace` are required while it is on (the first
  NetworkPolicy that selects a pod is a default deny of it). `name`
  (default `runner-containment`) and `annotations` (an Argo CD guard, a
  sync wave) are values, so an estate that already renders the same object
  elsewhere keeps its name.
- **New: `ci-builders` `buildkitd.networkPolicy.additionalIngress` and
  `nixWorkers.networkPolicy.additionalIngress`.** Empty by default. Each entry
  (`name`, `from`, optional `annotations`, optional `arch`) is one additive
  NetworkPolicy beside the consumer-namespaces policy, on the component's
  own port, for a peer the consumer namespaces do not describe (developers
  reaching a builder through a tailnet router, a pilot on one worker).

### Pulumi package: `deploy/pulumi/runneridentity`

- **New: the runner pools' AWS identities as a Go Pulumi package.** Its own
  module (`github.com/truvity/ci-plane/deploy/pulumi/runneridentity`),
  versioned by tags of the form `deploy/pulumi/runneridentity/vX.Y.Z`, so a
  chart release does not publish it and it does not ride a chart version.
  It builds, through `github.com/truvity/k8s/pkg/aws/podidentity`, the Pod
  Identity role of a runner pool (`Pool`: pull its runner images, read the
  registry's pull-through caches and the chart repositories, use the CI
  cache bucket, optionally share the BuildKit warm cache) and of the Go
  module proxy (`Modproxy`: one prefix of the cache bucket). Every name is
  an input; functional options (`WithArgs`, `WithNames`,
  `WithLegacyTopLevel`, `WithIgnoreAssociationTags`) let an estate that
  already runs these roles keep their logical names and URNs. Nothing
  deploy-shaped (cluster access, publishing) can be granted through it.

## v5.4.0

### ci-builders chart: cap buildkitd parallelism (`buildkitd.maxParallelism`)

- **New: `buildkitd.maxParallelism`.** A positive integer, rendered as
  `--oci-max-parallelism=<n>` in the buildkitd args. Unset by default,
  so existing renders are byte-identical. It bounds the build steps a
  daemon runs at once, so a burst of concurrent builds queues instead of
  OOM-killing the daemon. A value below 1 or a non-integer is refused by
  the schema.

## v5.3.0

### ci-builders chart: the Go module proxy is back (`goModproxy`)

- **`goModproxy` returns, off by default.** `go-cache-plugin serve
  --modproxy` as a Deployment + Service (`go-modproxy`, port 80, path
  `/mod`) over an S3 prefix, with the same values as before the 2.0.0
  removal (`s3.bucket`, `s3.region`, `s3.keyPrefix`, `s3.endpoint`,
  `s3.pathStyle`, `s3.existingSecret`, `sumdb`, `serviceAccount`,
  `stagingSize`, `podAnnotations`, `resources`). The runner image already carries the
  v5.2.1 `go-cache-plugin`, so the truncated-zip fix applies to it.
- **New: `goModproxy.networkPolicy`.** An ingress policy admitting only
  `consumerNamespaces`, required and non-empty while the proxy is on,
  like the other components.
- **`goModproxy` is no longer refused.** The 2.0.0 render guard on the key
  is gone; the guard on `nixWorkers.accounts.nixremote` stays.

## v5.2.1

### runner image: `go-cache-plugin` no longer serves truncated module zips

- **go-cache-plugin is built from upstream commit 3031b5d** (was v0.1.1),
  which carries creachadair/atomicfile v0.4.2. v0.1.1's atomicfile
  committed a half-written file when the read failed, so a request
  cancelled while `go-modproxy` faulted a module in from S3 left a
  truncated zip in the local cache, served afterwards with HTTP 200
  (`zip: not a valid zip file`, 2026-09-13). The write is now aborted.
  Needs a release plus a promotion of the ci-cache chart's image tag to
  take effect on `go-modproxy`.

## v5.2.0

### runner image: `sluisctl` 1.74.0, `accessctl` is a symlink to it

- **`sluisctl` is truvity/sluis v1.74.0** (was v1.71.0), still verified
  against the release's own `checksums.txt`, now matched on the whole
  file name: from v1.74.0 that file also lists each archive's SBOM, which
  the old substring match caught too.
- **`accessctl` is now `sluisctl` under its old name** (a symlink), no
  longer access-roster v1.39.2. Every subcommand and flag a job used
  works the same; each call also prints a deprecation notice on stderr
  (stdout, which a credential helper's caller parses, is unchanged).
  Move `awsConfig.credentialProcess` lines and scripts to `sluisctl`;
  the symlink goes in a later release.
- `r2broker` stays, since `sluisctl r2` execs it.

## v5.1.3

### runner image: `sluisctl`

- **The runner image bakes `sluisctl`** (truvity/sluis v1.71.0,
  checksum-verified), so `awsConfig.credentialProcess` can name
  `sluisctl r2` with the same flags as `accessctl r2`. `accessctl` and
  `r2broker` stay in the image, so a consumer can move back by changing
  its manifest alone.

## v5.1.2

### ci-builders: nix-cache seccomp

- **The `nix-cache` pod sets seccomp `RuntimeDefault`.** It already ran
  non-root with no escalation, every capability dropped and a read-only
  root filesystem; the missing seccomp profile was the only thing keeping
  it off Pod Security `restricted`.

## v5.1.0

Released 2026-10-02.

### arc-runners: `scaleSets.<name>.restricted`

- **A scale set can run on the Pod Security `restricted` profile.**
  `scaleSets.<name>.restricted: true` renders that set's runner pods and the
  `prepare-nix-builder-ssh` init container as
  non-root (numeric `runnerUser`, default 1001:1001), seccomp
  `RuntimeDefault`, no privilege escalation, every capability dropped, with
  the pod's `fsGroup` making the init container's `emptyDir` mounts
  writable. Off by default; a set that leaves it off renders exactly as in
  v5.0.0 (the listener is the exception, see below). What a job loses (`sudo`, setuid binaries, file capabilities,
  user-namespace syscalls) is in `docs/restricted-runners.md`.
- **Behaviour change: every scale set's listener is restricted, whatever
  `restricted` says.** Each `AutoscalingRunnerSet`'s `listenerTemplate` now
  carries pod `runAsNonRoot` and seccomp `RuntimeDefault`, and on the
  `listener` container `runAsNonRoot`, `allowPrivilegeEscalation: false` and
  every capability dropped. The listener image already runs as the numeric uid
  65532, so nothing it does changes, but a changed template recycles every
  listener pod on the next pin bump (a few seconds of job-pickup pause per
  set). The `restricted` key keeps meaning the runner pods only, and stays off
  by default.
- **The janitor's pod carries `seccompProfile: RuntimeDefault`** for every
  release (its container was already restricted). The only rendered change for
  a release that does not use the new key.
- **The runner image's `nix-worker-client-setup` accepts a mount point it does
  not own** when it can write to it, instead of failing on the `chmod` and
  `chown` a non-root process may not do. Behaviour as root is unchanged.

## v5.0.0

Released 2026-09-30.

### Breaking: the Nix workers have one login, and no stored host key

**The worker generates an ephemeral ed25519 host key per pod, and the
defaults are the v4 cut-over shape: `nix` / `ci-nix` over ssh-ng,
known_hosts via `@cert-authority` only.** Image and charts ship at one
version as always; do not run a v5 image with a v4 chart or the other
way round (a v4 chart rendering `nixremote` on makes the v5 worker
refuse to start, naming why).

- **ci-builders `nixWorkers.hostCertificate.enabled` defaults to
  `true`.** With it on, the worker generates its host key at container
  start in the (now memory-backed) runtime `emptyDir`, signs it on the
  OpenBao host CA as before, and mounts NO host key Secret:
  `nixWorkers.ssh.existingSecret` (formerly defaulting to
  `nix-builder-server`) is no longer read, and has no default. The
  readiness probe additionally requires the certificate to certify this
  pod's own key. `nixWorkers.openbao.{address, caBundle, namespace,
  authMount, authRole}` are therefore required whenever the workers are
  enabled.
- **The static host key is an explicit option:**
  `hostCertificate.enabled: false` plus `ssh.existingSecret` (required
  then), with runners pinning it (`knownHosts.pinned: true`).
- **The legacy `nixremote` login is gone**: the account, the
  `nix-worker-ssh-command` shim (`nix-store --serve`) and the
  `nix-store` alias left the worker image, and the runner image lost
  its leftover worker role (the `nixremote` account, worker scripts and
  `sshd_config`). `nixWorkers.accounts.nixremote` is REFUSED at render
  time (any value, including `enabled: false`). sshd's global
  `ForceCommand` is now `nologin`; `nix` and `admin` override it.
- **ci-builders `nixWorkers.accounts.nix` defaults** to
  `principals: [ci-nix]`, `trusted: true` (the trust `nixremote` always
  had; a remote build worker needs it). An empty `principals` list is
  refused: it would leave no machine login.
- **arc-runners `nixBuilders` defaults**: `login.user: nix`,
  `login.principal: ci-nix`, `login.protocol: ssh-ng`,
  `openbao.sshRole: ci-nix` (was `user`), `knownHosts.pinned: false`.
  `knownHosts.certAuthorities` is therefore required while nixBuilders
  is enabled (unless you pin); `knownHosts.revision` is required only
  when pinned. `nixBuilders.sshUser` (the deprecated alias) is REFUSED
  at render time.
- **nix-worker-client defaults** (used only when the chart does not set
  them, which it always does): principal `ci-nix`, protocol `ssh-ng`,
  known_hosts unpinned.
- **ci-builders `nixWorkerImage.repository` is required**: the fallback
  to `runnerImage` is gone, since the runner image no longer carries the
  worker role.

#### Migrating a consumer

A consumer already on the v4 cut-over shape (the Truvity devel estate
since 2026-09-27) mostly DELETES values:

1. ci-builders: delete `nixWorkers.accounts.nixremote` (required: it is
   refused), `nixWorkers.hostCertificate.enabled: true` (now the
   default), and `nixWorkers.accounts.nix` if it says
   `principals: [ci-nix]`, `trusted: true`. Keep `nixWorkers.openbao.*`.
   Stop delivering the `nix-builder-server` Secret: nothing mounts it.
   Retire the host key it holds (secret manager row, KV path) once the
   v5 workers are rolled -- it never has to be kept or rotated again.
2. arc-runners: delete `nixBuilders.sshUser` (required: it is refused),
   and optionally `login`, `knownHosts.pinned: false` and
   `openbao.sshRole: ci-nix`, now the defaults. Keep
   `knownHosts.certAuthorities`.
3. Bump both charts together (one version, as always). The workers roll
   (new host keys, which clients never see: they trust the CA) and the
   runners recycle (the values hash changes).

A consumer still on the v4 legacy shape (`nixremote` over `ssh://`,
pinned known_hosts) must first move to host certificates on the host
CA and to a ci-nix signing role. For a consumer with Nix disabled
(`nixWorkers.enabled`/`nixBuilders.enabled` false) nothing changes
unless it sets one of the removed keys.

## v4.2.0

Released 2026-09-29.

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

## v4.1.0

Released 2026-09-29.

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
