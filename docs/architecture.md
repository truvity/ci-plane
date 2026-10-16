# Architecture

The CI plane is three artifacts released as **one versioned unit**: the
runner image and the two Helm charts that surround it. This document
records the load-bearing decisions and why each one holds.

## One version, transitively digest-pinned

A `v*` tag releases everything. The release workflow:

1. builds each image's heavy base **natively per architecture** (amd64
   and arm64 runners, no QEMU) with a registry layer cache, and merges
   the two halves into one manifest list;
2. assembles the release images from those bases without executing
   anything (`image/release/`), and stamps each digest into the chart
   defaults **by key** (`hack/stamp-digests.sh`), so the runner's digest
   can never land in `nixWorkerImage`;
3. stamps both charts' `version` and `appVersion` from the tag — the
   committed `0.0.0` is a placeholder — and publishes them.

A consumer therefore pins **one chart version** and is transitively
digest-pinned to both images. Every release builds the images: native
builds with a layer cache made an unchanged base cheap, so the old
skip-rebuild gate, and its "did `image/` change?" question, is gone.
The stamp is the compatibility statement: chart X was released against
image X.

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

## One different scale set does not need a second release

Through v3.0.0, everything about a scale set except `gomaxprocs`,
`resources` and (undocumented, but already read this way)
`minRunners` was release-wide: `nodeSelector`, `tolerations`,
`affinity`, `podAnnotations`, `extraEnv`, `extraEnvFrom` and `nixConfig`
applied identically to every profile in `scaleSets`. An estate that
needed ONE scale set to differ — a big Nix builder pinned to one node, an
amd64 set on a dedicated host — had no lever inside this chart for it,
and paid for the difference with a whole second release of `arc-runners`
sharing the same GitHub App secret (opwerm's `arc-runners-nix` release is
exactly this shape).

v4.1.0 adds `scaleSets.<name>.<key>` overrides for each of those keys,
each REPLACING the release-wide value when set on that one scale set
(never merging into it — a deep merge would leave no way to ask for an
explicitly empty map or list on one set while the release default is
non-empty) and falling back to the release-wide value otherwise. A
release that sets none of them on any scale set renders BYTE-IDENTICAL
to before this existed: the ConfigMap, the ConfigMap name and the
`values-hash` annotation are unaffected unless a scale set actually uses
a new key (`templates/_helpers.tpl`'s `nixConfigFor`/`nixConfigNameFor`,
and the conditional `hasKey` guards around `$spec` in
`templates/runnersets.yaml`, carry the full reasoning). `extraNixConfig`
is additive rather than a fallback value: it APPENDS to the release-wide
`nixConfig` for one set only, because Nix keeps the LAST occurrence of a
scalar key when it reads its config top-to-bottom.

## The Nix sandbox is per scale set, and the chart cannot install its half

`scaleSets.<name>.nixSandbox` (v4.1.0) is the chart-rendered half of
running Nix's own build sandbox in a pod at all: `hostUsers: false`,
`procMount: Unmasked` and a `Localhost` seccomp profile on that set's
pods, plus `sandbox = true`/`sandbox-fallback = false` in its
`NIX_CONFIG`. The chart cannot install the OTHER half — the profile
file on the node, and the node's own `user.max_user_namespaces` sysctl —
because both are node-shaped facts a Helm chart has no channel to. See
[docs/nix-sandbox.md](nix-sandbox.md) for the full mechanism, the
estate's checklist, and `nixSandbox.fence.enabled`'s
`ValidatingAdmissionPolicy`, which exists because a `Localhost` seccomp
profile is a file ANY pod scheduled on that node can name — PodSecurity
`baseline` never inspects what the profile actually contains.

This is the chart-side half of opwerm/nexus PR #306's stopgap: that PR
hand-wrote a `MutatingAdmissionPolicy` to fake `hostUsers`,
`procMount` and `seccompProfile` onto one scale set's pods because this
chart could not yet set them, and said so at the time ("the upstream fix
... truvity/ci-plane arc-runners gains per-scale-set hostUsers,
podSecurityContext and runnerSecurityContext values"). With this
release, that stopgap's mutating policy is no longer needed for a new
install; its Talos node patches (the sysctl and the seccomp profile
itself) stay either way, because those are node facts, not chart output.

## Runners pack; they do not spread

Left to the default scheduler, runners SPREAD. `NodeResourcesFit`
scores with `LeastAllocated`, and balanced allocation agrees with it, so
of all the nodes a runner fits, it lands on the emptiest. Bigger nodes
do not change this: a pool of nodes that each hold several runners still
ends up with one idle warm runner per node. And the spread cannot be
undone afterwards on a Karpenter estate, because runners there carry
`karpenter.sh/do-not-disrupt` (set via `podAnnotations`, empty by chart
default; `presets/karpenter.yaml` sets it, see docs/day-1-install.md)
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

## Runner watchdog: a stuck runner says so itself

Upstream ARC
[#4307](https://github.com/actions/actions-runner-controller/issues/4307)
strands an ephemeral runner after a cancelled or interrupted job: the
runner stays registered and idle, and never takes another job. The
janitor finds such runners by grepping the last 200 lines of each idle
runner's log every few minutes. `runner.watchdog` (off by default) lets
each runner judge itself, with job state the janitor cannot see from
outside.

`runner-watchdog` (image/runner-watchdog, in the runner image at
`/usr/local/bin/runner-watchdog`) becomes the runner container's PID 1
and runs `/home/runner/run.sh` as its child:

- **Transparent.** The child runs in its own process group. Its stdout
  and stderr are copied to the container's byte for byte, so
  `kubectl logs` reads the same. SIGTERM, SIGINT, SIGHUP, SIGQUIT and
  SIGUSR1/2 are forwarded to the child's group (run.sh traps INT/TERM
  and hands them to the listener, as it does as PID 1). Every orphan
  reparented to the wrapper is reaped, and the wrapper exits with the
  child's code (128+N if signal N killed it). It never exits while the
  child runs.
- **Never in the runner's way.** Lines go to the matcher through a
  bounded queue. When the matcher falls behind, lines are dropped and
  counted (`runner_watchdog_lines_dropped_total`), and the runner never
  waits. If the HTTP server cannot bind, the watchdog says so on stderr
  and the runner runs on.
- **Job-aware.** STUCK means no job is running AND a stuck signature,
  first seen while idle, has persisted for `grace` (45s by default). A
  signature during a job is counted and never acted on. A job starting
  clears it.

The lines it reads, from actions/runner v2.338.0 (the upstream image sets
`ACTIONS_RUNNER_PRINT_LOG_TO_STDOUT=1`, so the listener's trace reaches
stdout):

| Line | Source | Meaning |
|---|---|---|
| `<ts>Z: Running job: <name>` | `Runner.Listener/JobDispatcher.cs` | a job started |
| `<ts>Z: Job <name> completed with result: <result>` | `Runner.Listener/JobDispatcher.cs` | the job ended |
| `Skipping message Job. Job message already acquired '<id>'. …` (e.g. `job assignment is invalid: MissingKey`) | `Runner.Listener/Runner.cs`, HTTP 409 from `acquirejob` | stuck signature |
| `Skipping message Job. Job message not found '<id>'. …` (e.g. `job was canceled`) | the same, HTTP 404 | stuck signature |
| `Skipping message Job. Unprocessable job '<id>'. …` | the same, HTTP 422 | stuck signature |
| `Registration <id> was not found.` | the broker session's token refresh (`BrokerServer`) | stuck signature |

The job lines are matched only at the start of a line. The listener also
traces each of them as `[RUNNER … INFO Terminal] WRITE LINE: …`, and
counting that copy would count every job twice.

**The verdict is a readiness probe, never a liveness probe.** `/healthz`
answers 200, or 503 while stuck. A liveness failure would restart the
container, and a restarted runner cannot register again: its JIT
registration is single-use, and the restart ends in the "Registration …
was not found" crash loop instead. The remedy is deleting the
EphemeralRunner, which ARC then replaces. The janitor (every minute)
deletes any runner that holds no job and whose pod has a readiness probe on
its `runner` container, that container is running, and the pod has been
NotReady for `janitor.notReadySeconds` (60). The criterion keys on the pod,
not on a chart value: a pod without the probe (the watchdog off, or an
older pod) never matches, so it is inert until `runner.watchdog.enabled` is
on. The log grep stays as the backstop (idle, past `graceSeconds`), for a
runner whose watchdog cannot answer. `hack/janitor-cases.sh` runs both
against a stub `kubectl`.

**A drill proves the chain without waiting for #4307.** With
`runner.watchdog.drillFile` set (an absolute path, off by default), the
watchdog feeds itself one line under its own signature, `drill`, each time
that file appears. `kubectl exec <idle runner> -- touch <path>` then
drives the real path: the matcher arms on the line only because no job is
running, `/healthz` answers 503 after `grace`, the pod goes NotReady, the
janitor deletes the runner, ARC replaces it, and `ARCRunnerStuck` fires.
The line goes through the same matcher as the runner's output, so a drill
during a job is only counted. `runner_watchdog_signature_lines_total{signature="drill"}`
tells a drill from a real detection. An unset `drillFile` stays out of
the values-hash, so the key's arrival recycles no listener.

`/metrics` is the Prometheus text format, on a container port named
`watchdog` (`runner.watchdog.port`, 8091 by default). `monitoring.enabled`
renders the PodMonitor for it (and one for the scale-set listeners' `gha_*`
metrics, port `metrics`, which need the controller chart's `metrics:`); an
estate that scrapes some other way selects the port by name:

| Series | Type |
|---|---|
| `runner_watchdog_stuck` | gauge, 0/1 |
| `runner_watchdog_stuck_detections_total` | counter |
| `runner_watchdog_job_running` | gauge, 0/1 |
| `runner_watchdog_jobs_started_total`, `runner_watchdog_jobs_completed_total` | counter |
| `runner_watchdog_signature_lines_total{signature=…}` | counter, during a job or not |
| `runner_watchdog_lines_total`, `runner_watchdog_lines_dropped_total` | counter |
| `runner_watchdog_seconds_since_last_line`, `runner_watchdog_seconds_since_last_job` | gauge |
| `runner_watchdog_grace_seconds` | gauge |

The only lines the watchdog adds to the log are its own transitions,
prefixed `runner-watchdog:` (`stuck: …` and `recovered: …`), and a
server that could not bind.

What it does not cover: a runner that hangs WHILE it holds a job
(actions/runner#4569, a cancellation redelivery loop). The job is
running, so the watchdog judges nothing.

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
the update branch, and automerges on its own CI. See the consumer-side
wiring in [day-2-operations.md](day-2-operations.md).

## The cache doctrine

Every `charts/ci-builders` component is disposable: no backups, zero
IAM where possible, no backups; losing one costs a re-warm. A dead
cache degrades to upstream — slower, never broken. buildkitd's PVC is
the HOT layer only; the WARM layer is `cache-to type=registry` and
survives builder replacement. Restrictive network defaults: only listed
namespaces reach the builders.

## cache-redirect: a hit is a redirect, not a stream

A read-through cache that streams every byte of every hit through one
pod is limited by that pod's node network (on the order of 100 MiB/s
sustained), which a CI burst saturates: each job then crawls, however
warm the cache is. Yet almost every byte such a cache serves is an
immutable, content-addressed file: a NAR, a module zip.

`cache-redirect` (image `ghcr.io/truvity/ci-plane/cache-redirect`, chart
`cacheRedirect`, off by default) keeps the cache's index in an S3 (or
S3-compatible) bucket and answers a hit on a **large** immutable object
with an **HTTP 302 to a presigned GET URL** of that object, valid for
`presignTTL` (5 minutes). The runner downloads from the object store
directly; the service only ever handles small requests, so two small
replicas spread across nodes carry a fleet. **Small** objects (narinfo,
`.info`, `.mod`, checksum-database tiles) are served directly, from an
in-memory LRU or the bucket. The pods keep nothing on local disk.

Clients, both front-ends on one Service:

```text
nix:  substituters = http://cache-redirect.<ns>.svc.cluster.local/nix https://cache.nixos.org/
go:   GOPROXY=http://cache-redirect.<ns>.svc.cluster.local/goproxy|https://proxy.golang.org,direct
```

Nix (curl) and the go command both follow redirects; a runner must be
able to reach the bucket's endpoint itself (on AWS a gateway VPC endpoint
for S3 is free and keeps the bytes off a NAT gateway). Its presigned
request carries no credential of the runner's: the URL is the
credential, for one object, for minutes.

### A miss is fetched once and committed only whole

A miss is pulled through from upstream **once per key**: the first
request leads, and requests for the same key that arrive meanwhile wait
for its outcome and are then redirected (or served the small body) --
they never start a second fetch. The leader's client is fed the bytes as
they arrive while the same bytes are uploaded to the bucket: one `PUT`
for an object smaller than a part (8 MiB), a multipart upload otherwise.
The fetch runs on the service's own context, so a leader whose client
goes away does not abort the upload the waiting requests count on. The
cost of this simplicity: a waiter for a large cold NAR waits for the
whole download before its redirect, rather than streaming alongside.

The object is **committed only when the upstream response completed
and is whole**: every byte its `Content-Length` promised and no more
and, for a NAR, the size and hash its narinfo announced (below).
S3 makes a multipart object visible at `CompleteMultipartUpload` and
never before, so anything short of that is aborted and nothing appears
under the key. A truncated object in a cache is worse than none -- it is
a hit forever; a truncated zip in a Go module cache (`zip: not a valid
zip file`) is a failure this plane has met. The client of a failed
stream sees a broken response, never a clean end. An upstream body that
sends nothing for `-idle-timeout` (1 minute) fails the same way. Give the
bucket an `AbortIncompleteMultipartUpload` lifecycle rule as the
backstop for an abort that itself never ran (a pod killed mid-upload),
and an expiration rule if it should not grow forever: an expired object
is only a miss.

A **NAR is verified against its narinfo, not its file name**: on
cache.nixos.org `nar/<hash>.nar.zst` is not the hash of the compressed
body. Whenever the Nix front-end serves a narinfo (miss or hit) it parses
`URL`, `FileHash` and `FileSize` and writes an index record, keyed by the
NAR's URL path, to the bucket and to memory:

```text
<nix prefix>nar-index/<URL path>.json     e.g. nix/nar-index/nar/<file>.nar.zst.json
{"url":"nar/<file>","fileHash":"<sha256, Nix base 32>","fileSize":<bytes>}
```

It lives in the bucket because the narinfo request and the NAR request
can reach different replicas. On a NAR miss the record is looked up
(memory, then bucket); the NAR is committed only when the streamed body's
size and SHA-256 equal it. A mismatch aborts the upload (the client still
receives the whole pass-through body) and is counted `verify_failed`. A
NAR with no record (asked for without a narinfo through this cache) is
passed through and never stored, counted `no_index`. Neither is an
`upstream_error`.

An upstream **404 or 410 reaches the client unchanged** (GOPROXY's comma
falls through on exactly these two; Nix moves to its next substituter),
remembered in memory for `-negative-ttl` (1 minute), never stored. Any
other upstream failure is a 502.

### The store's errors are never misses

The bucket must answer a missing key with 404 (`NoSuchKey`), which needs
`s3:ListBucket` on the prefix; without it S3 answers 403. The service
treats only 404 as a miss. A 403 -- or any other store error -- is a 502
(`store_error`): a role that lost its grant must not turn every request
into an upstream fetch it can then never store. The presigned URLs are
signed with the service's own credentials, so `s3:GetObject` on the
prefix is also what authorizes every runner's redirected download; the
credentials are refreshed `presignTTL` + 1 minute before they expire, so
a URL is never signed with credentials about to lapse.
`deploy/pulumi/runneridentity`'s `CacheRedirect` builds exactly this
role for one prefix of a bucket (both front-end prefixes go under it,
e.g. `cache-redirect/nix/` and `cache-redirect/goproxy/`); credentials
come from the default AWS SDK chain, so EKS Pod Identity, IRSA, or the
keys of `cacheRedirect.s3.existingSecret` for an S3-compatible store.

### Nix front-end

`/nix/nix-cache-info` is answered locally (`StoreDir: /nix/store`,
`WantMassQuery: 1`, `Priority: 30` by default -- lower than
cache.nixos.org's 40, which is what makes Nix prefer it; the order of
`substituters` does not). `/nix/<hash>.narinfo` is stored **verbatim**:
its signatures are over its content, and its relative `URL: nar/...`
resolves against this cache. `/nix/nar/<file>` is the large object. A
`HEAD` of a stored NAR is answered with its size (a presigned GET cannot
answer a HEAD); a `HEAD` of one not stored is asked of upstream and
stores nothing. Everything else (`/realisations/`, `/log/`, `.ls`) is a
404.

### Go front-end and the checksum database

`/goproxy/<module>/@v/<version>.info` and `.mod` are stored and served
directly; `.zip` is the large object. Only a **canonical** version
(pseudo-versions and `+incompatible` included) is immutable: the go
command also resolves a query (`master`, a commit) through
`<query>.info`, whose answer moves, so that, `@v/list` and `@latest` are
passed through and remembered in memory for `-mutable-ttl` (30 seconds),
never stored. Module paths and versions keep the go command's `!`
case-encoding byte for byte, in the store key and in the upstream URL.

The checksum database is **proxied**: `/goproxy/sumdb/sum.golang.org/supported`
answers 200, tiles (full and partial, both immutable by construction)
are stored like any small object, and `lookup/` and `latest` are passed
through. Answering `supported` with 404 instead would send every go
command to sum.golang.org directly, so CI would need direct internet
access for the checksum database even with every module cached; proxying
removes that, and costs no trust: the go command verifies the database's
signed tree heads and tiles itself, so a proxy can withhold an answer but
cannot forge one. Set `goproxy.sumdb.name: ""` to send the go command to
the database directly instead.

### Metrics and operations

`/metrics` (Prometheus text, `cacheRedirect.monitoring` renders a
PodMonitor): `cache_redirect_requests_total{frontend,result}` with
`result` one of `hit` (served from memory or the store), `redirect`,
`miss` (pulled through), `not_found`, `upstream_error`, `store_error`,
`verify_failed` (a NAR not matching its narinfo: served, not stored),
`no_index` (a NAR with no narinfo record: served, not stored);
`cache_redirect_uploads_total{frontend,result="ok|failed"}`;
`cache_redirect_upload_bytes_total{frontend}`;
`cache_redirect_upstream_request_duration_seconds{frontend}` (a
histogram of the time to upstream response headers); and
`cache_redirect_fetches_in_flight`. `/healthz` answers 200 while the
process serves. On SIGTERM it stops accepting, gives in-flight requests
25 seconds, then aborts the uploads still running -- discarded, never
committed.

A down service degrades like every cache here: Nix falls through to the
next substituter, and a GOPROXY comma fails the job loudly while a pipe
would hide the outage.

## Persistent Nix workers are CI-only and separate from BuildKit

The optional `nixWorkers` plane is one native StatefulSet per
architecture, each with its own RWO `/nix` store. It deliberately does
not reuse BuildKit pods or PVCs: rootless BuildKit and a privileged,
sandboxed multi-user Nix daemon have incompatible security models, and
sharing CPU, disk, GC and failure domains would let one cache evict or
stall the other.

The workers run their own image (`image/nix-worker`, split from the
runner image on 2026-09-24). An init container imports the image's Nix
closure into the persistent alternate-root store through Nix itself,
including the validity database; raw copying or mounting an empty PVC
over the baked store would produce an unusable image. The worker then
runs `nix-daemon` with build users and exposes one machine login: `nix`,
over the ssh-ng protocol, forced to `nix-daemon --stdio`. `sshd` forbids
passwords, forwarding, TTYs and arbitrary commands. (v5.0.0 removed the
legacy `nixremote` login, whose force-command accepted only
`nix-store --serve`.)

The estate supplies only the OpenBao HTTPS CA, one or more public keys
of the environment's SSH user CA (exported, for example, as
`sshUserCaPublicKeys`; the workers trust the same keys), and the public
key of its SSH HOST CA, which the runners trust as an `@cert-authority`
line. No host key is stored anywhere: each worker pod generates its own
(see [below](#worker-host-certificates-and-ephemeral-host-keys)).
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

### Worker host certificates and ephemeral host keys

`nixWorkers.hostCertificate` is ON by default since v5.0.0. The worker
generates an **ephemeral ed25519 host key per pod** at container start
(`nix-worker-entrypoint.sh`, into the memory-backed runtime `emptyDir`;
a fresh one on every container start, whatever an earlier container of
the same pod left there discarded first) and signs it with an OpenBao
SSH secrets engine — `cert_type=host`, `valid_principals` set to the
worker's own DNS name, COMPUTED by the chart from the release namespace
(`nix-worker-<arch>.<namespace>.svc.cluster.local`) rather than taken as
a raw value, so a public repository never invites an estate-specific
hostname into a values file. A client that trusts the signing CA's public
key as an `@cert-authority` line (`arc-runners`'
`nixBuilders.knownHosts.certAuthorities`) then trusts every worker that
CA signs for. Nothing trusts the key itself, only the CA's signature on
it, so a key that dies with its pod is strictly less to protect than a
stable one in a Secret, and there is nothing to rotate: every restart is
a rotation. The readiness probe (`nix-worker-healthcheck --readiness`)
reports ready only while a certificate exists, parses, has at least
`minRemaining` of validity left, AND certifies exactly this pod's host
key — a fresh certificate for some other key is refused like a missing
one.

The static host key (`nixWorkers.ssh.existingSecret`, no default since
v5.0.0) remains only as the explicit `hostCertificate.enabled: false`
path: without a certificate a client can only pin the key, so it has to
be stable, and the runners then set `nixBuilders.knownHosts.pinned:
true`.

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

The machine login, `nix` — the ssh-ng protocol via `nix-daemon
--stdio` — is gated by `AuthorizedPrincipalsFile` content the chart
renders from `nixWorkers.accounts.nix.principals` (`[ci-nix]` by default
since v5.0.0; an empty list is refused, since it would leave no machine
login at all).
`sshd_config`'s `Match User nix` block is baked into every image
unconditionally; whether the account is ever reachable is decided by
whether `AllowUsers` gains `nix`, which `nix-worker-entrypoint.sh`
computes at container start from that principals file. `trusted-users`
in the Nix daemon config gains `nix` only when
`nixWorkers.accounts.nix.trusted` is set — trusted users can import
store paths WITHOUT signature verification, which is what Nix's
sandboxing model exists to prevent. A remote build worker needs it
(runners upload unsigned derivation inputs), so it is on by default
since v5.0.0 — the same trust the removed `nixremote` login always had.

### Runners over ssh-ng

`arc-runners`' `nixBuilders.login.{user, principal, protocol}` chooses
the machine login; since v5.0.0 the defaults are the only one the
workers have: `user: nix`, `principal: ci-nix` (which must be in the
worker's `nixWorkers.accounts.nix.principals` list — not necessarily
`nix` itself, `AuthorizedPrincipalsFile` can require a narrower name
than the account), `protocol: ssh-ng`. `nixBuilders.openbao.sshRole`
(default `ci-nix`) names the signing role that issues for that
principal. v5.0.0 removed the legacy `nixremote`/`ssh://` login and the
deprecated top-level `sshUser` alias of `login.user`; setting `sshUser`
is a render-time failure (`templates/removed-values.yaml`), not a
silently ignored key.

`nix-worker-client` validates a returned certificate against the
configured principal and protocol: one principal, no extension but
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

`nixBuilders.knownHosts.pinned` (default `false` since v5.0.0) is
independent of the login: unpinned, the runner's `known_hosts` is built
ENTIRELY from `certAuthorities` (the workers' host CA line) and the
pinned Secret is not mounted; `true` is the explicit static-key path,
pinning each worker's static key from a Secret (for workers with
`hostCertificate.enabled: false`). Unpinned with an empty
`certAuthorities` list is refused at render time — that combination
would trust nothing.

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

### People, over opkssh (the people pilot)

Phases 1-2 above are machines only (CI runners, an OpenBao-signed
certificate). This pilot adds a SEPARATE, independent credential path
for PEOPLE onto the same `nix` account, plus a new `admin` account no
certificate ever reaches: OpenPubkey SSH (opkssh), the same
no-CA OIDC sign-in `truvity/tailscale`'s EC2 subnet routers already use.
`AuthorizedKeysCommand` and `TrustedUserCAKeys`/`AuthorizedPrincipalsFile`
are independent sshd mechanisms tried in turn for pubkey auth, so this
never displaces or reorders phases 1-2 — a worker can run every
combination of `accounts.nix`, `hostCertificate` and `opkssh` at once,
and a worker with `opkssh.archs` empty (the default) renders exactly as
it did before this feature existed.

`opkssh`, `opksshuser` and the `admin` account are baked into every
`nix-worker` image unconditionally (`image/nix-worker/Dockerfile`) —
whether either is ever REACHABLE is decided entirely by
`nixWorkers.opkssh.archs`, an explicit allow-list into `nixWorkers.archs`
(the same shape as `truvity/tailscale`'s `opksshEnvs`): naming an
architecture there is "one pilot worker", every other architecture's
StatefulSet shares the image and the opkssh ConfigMap but never sets
`NIX_WORKER_OPKSSH_ENABLED`, so `AllowUsers` never gains `admin` and
`/etc/opk/{providers,auth_id}` render empty (`nix-worker-entrypoint.sh`).

The OWN install steps (never opkssh's upstream `install-linux.sh`) for
the same reason `truvity/tailscale` v1.11.0 stopped calling it: that
script's OS-family detection does not recognize every base image, and
owning the handful of steps it would have taken — create `opksshuser`,
install the checksum-verified binary, write the sshd
`AuthorizedKeysCommand` drop-in — costs less than depending on it.

`/etc/opk` is a writable `emptyDir` in the chart, not the image's own
directory: `readOnlyRootFilesystem: true` means opkssh's hardcoded
`/etc/opk/{providers,auth_id}` paths (there is no flag to relocate them)
could never be written at container start otherwise — the same reason
`$runtime_dir` under `/run/nix-worker` exists for the CA-certificate
files phases 1-2 use. `nix-worker-entrypoint.sh` rebuilds both files
from the chart's ConfigMap every start, exactly the "empty means nobody"
default the principals ConfigMap already establishes.

`admin` is the one account on this worker with a real interactive shell
and `sudo` (passwordless: there is no password authentication method to
enter one against). `Match User admin` in `image/sshd_config` sets
`ForceCommand none` — overriding the GLOBAL `ForceCommand` (`nologin`
since v5.0.0 removed the legacy `nixremote` shim it used to be) that
would otherwise force `admin`'s session through it too — and `PermitTTY yes`, the one place this worker grants a real
terminal. `nix` stays `ForceCommand nix-daemon --stdio` regardless of
which credential (certificate or opkssh) reached it: opkssh only adds a
SECOND way in, never a different account or a different forced command.

Trust in `nix.conf`'s `trusted-users` (`nixWorkers.accounts.nix.trusted`)
is an EXISTING, independent toggle (phase 1) opkssh does not touch,
override, or gate on: it is a property of the ACCOUNT (`nix`), not of
which credential mechanism reached it, so it applies identically to a
certificate-authenticated session and an opkssh-authenticated one.
**This installation keeps `nix` trusted** (an install that already sets
`accounts.nix.trusted: true` for its own CI principal, as the pre-existing
value comment describes) — opkssh does not narrow this. That means
EVERYONE who can reach `nix`, by either credential, can import a store
path without signature verification, which CI's own builds then trust
implicitly. Whether that population should stay this wide, or narrow to
whoever also operates CI, is a decision for each installation's own
identity policy to name explicitly (see the consuming estate's own
access-control docs) — this chart does not decide it.

## The image doctrine

The image contains only what devbox cannot deliver: the nix + devbox
bootstrap, bash-as-sh, the daemonless docker client + buildx, git-lfs, and the
Go build cache client. goreleaser-pro is deliberately absent: its
licence forbids redistributing the binary, and this image is public.
Everything else arrives per job from each repository's own
`devbox.json`. Repo- or cluster-specific content in
the image is a bug; the one documented debt is the baked in-cluster nix
substituter (dead elsewhere, upstream fallback).

**`git-lfs` ships because it is part of git** (v6.7.0, owner decision
D627). GitHub-hosted runners carry it, and `actions/checkout` with
`lfs: true` execs `git lfs` before any devbox environment exists, so on
a runner without it the checkout fails with "Unable to locate
executable file: git-lfs". It is a pinned static release binary
(`GIT_LFS_VERSION`, Renovate-tracked, checked against the release's
`sha256sums.asc`), and `git lfs install --system` configures the LFS
filters in `/etc/gitconfig`. CI runs the built image as the runner
user and asserts it.

**One Go cache client ships: `go-cache-plugin`.** The
[truvity/ci-cache](https://github.com/truvity/ci-cache) agent sat beside
it while the two were measured against each other, and was removed once
the plugin won. `GOCACHEPROG` names a binary that must exist before the
first `go` invocation of a job, and callers pin their own version of
this image on their own schedule, so a client leaves the image only
after nothing can still name it. The plugin itself leaves once
ci-cache's setup action fetches it per job.

A cache binary is in the image at all only because of that ordering
rule: it has to pre-exist the `go` invocation it caches, which is
exactly the kind of thing devbox cannot deliver in time. What it talks
to, and everything about the cache itself, belongs to that repository
and is not described here.

**`sluisctl` ships for the same reason** (v4.2.0; `sluisctl` replaced
`accessctl` in v5.2.0). On a store with no pod identity (Cloudflare R2,
MinIO, Ceph), the Go cache client above reads its credential through
the ordinary AWS SDK chain — an `AWS_CONFIG_FILE` naming a profile
whose `credential_process` is `sluisctl cloudflare r2 <preset>`
(`arc-runners`' `awsConfig` value, below). That credential_process is
invoked BY the cache client, and the cache client already has to
pre-exist the first `go` invocation of a job — so whatever it in turn
execs has to pre-exist it too, by the same ordering rule. `sluisctl`
(truvity/sluis) asks the estate's sluis for a credential its Cloudflare
module mints; a job that never names it leaves it idle on PATH. It is
pinned and verified against that release's own `checksums.txt`,
downloaded at build time rather than hand-pinned per architecture — see
`image/runner/Dockerfile`'s own comment for why that is safer under a
Renovate-driven version bump, not just shorter. Earlier releases of the image
also carried truvity/cloudflare's `r2broker`, which `sluisctl r2`
fronted; the broker is retired and sluis mints R2 credentials itself.

Every version pin in the Dockerfile carries a `# renovate:` annotation
— a pin without one is invisible, and invisible is indistinguishable
from current. That includes the upstream runner base: `latest` was
replaced with an annotated pin precisely so CVE pickups become renovate
PRs instead of side effects.

## An AWS profile for a credential helper (v4.2.0)

The shape from ["Runner pod expectations"](day-1-install.md) upward,
worked through for one store: Cloudflare R2, with the credential minted
by the Cloudflare module of sluis (truvity/sluis,
`docs/guides/sluis/cloudflare-tokens.md`), so the estate's own sluis —
not a static key — decides who gets a credential and for how long. The
estate declares an R2 preset (a disabled prototype token scoped to the
bucket, an `endpoint`, a `lifetime` and a `rotation`) and grants it to
a group its CI jobs earn through `github` matchers.

**Nothing extra runs the credential helper.** A GitHub Actions job
granted `permissions: id-token: write` already has
`ACTIONS_ID_TOKEN_REQUEST_URL`/`ACTIONS_ID_TOKEN_REQUEST_TOKEN` in its
environment — the Actions Runner process sets them per job, self-hosted
or not — and `sluisctl` reads them on its own
(sluis's docs/reference/sluis/sluisctl.md). So the workflow
side of this is one line in the caller's own `permissions:` block; this
chart's `awsConfig` value only has to get the credential_process line
onto `PATH`, in a place `AWS_CONFIG_FILE` names.

```yaml
# values.yaml for one arc-runners release
awsConfig:
  enabled: true
  profile: ci-cache
  credentialProcess: sluisctl cloudflare r2 <preset>
```

```yaml
# the calling workflow, unchanged apart from the permission
permissions:
  id-token: write
env:
  GOCACHEPROG: go-cache-plugin
  AWS_ENDPOINT_URL_S3: https://<account>.r2.cloudflarestorage.com
```

That renders a `runner-aws-config` ConfigMap (`templates/aws-config.yaml`)
holding one INI profile, mounted read-only on the runner container at
`/var/run/aws-config/config`, with `AWS_CONFIG_FILE` pointed at it and
`AWS_PROFILE` set to `ci-cache` — both on the runner container alone,
never `envFrom`, never the pod. `go-cache-plugin` (or any other AWS
SDK/CLI call in the same job) then resolves credentials the ordinary
way: no profile named explicitly falls through to this one, which execs
`sluisctl cloudflare r2`, which exchanges the job's own GitHub identity
token at sluis for an R2 access key pair that expires within the
preset's lifetime, never written to a Secret. The preset is
bucket-wide: sluis cannot scope an R2 credential to a prefix, so one
prefix per cache tool is a naming convention, not a boundary.

**A pool-wide credential instead.** When the jobs should not each sign
in, a secrets operator can project the credential sluis stores for the
preset (`external/cloudflare/<preset>`, a `cloudflare/v1` document,
replaced on every rotation) into a Secret, mounted through
`extraVolumes`/`extraVolumeMounts`, and the line becomes
`sluisctl cloudflare r2 <preset> --file <mounted path>`, which signs in
to nothing and refuses an expired document. Every job of the pool then
shares one credential.

**This is JOB-scoped, not CLIENT-scoped.** An ephemeral runner's
`runner` container is the one place every step of one job runs, so
`AWS_PROFILE` here is the *default* for any AWS call that job makes —
not narrowed to the Go cache client alone. A step in the same job that
needs a genuinely different AWS identity (a real AWS account through
pod identity, say) sets its own `AWS_PROFILE` or `--profile` for that
call, the same way it would override any other inherited default.

**Refused, never silently ignored, the other direction.** Setting
`awsConfig.enabled` alongside an `extraEnv` entry named
`AWS_ACCESS_KEY_ID` fails to render: the AWS SDK's credential chain
checks environment variables *before* a config file's
`credential_process`, so a static key left over from the old
`extraEnvFrom` Secret pattern would keep winning and sluis would
never be consulted — exactly the migration this feature exists for, not
happening, with nothing in a job log to say so. Drop the static key (and
its Secret) first.

**`projectedServiceAccountTokens` is a separate, more general hook**,
not required for the shape above — GitHub's own job OIDC token is
already what `sluisctl` in a job reads. It exists for a workload that
calls a SAME-CLUSTER service by presenting a ServiceAccount token
projected for that service's own audience instead
(sluis documents the service-to-service shape), which is a
different identity path than a GitHub Actions job's own token. Use it
when something in the pod needs that shape; the R2 path above needs
only `awsConfig` and the workflow's own `id-token: write`.
