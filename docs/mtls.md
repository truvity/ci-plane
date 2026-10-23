# Mutual TLS for the BuildKit builders

The builders (`ci-builders`, `buildkitd`) serve plain TCP on :1234 by
default, gated by a NetworkPolicy alone. `buildkitd.mtls` adds builders that
terminate mutual TLS themselves, which is what a TLS-passthrough gateway in
front of them needs; a client presents a certificate from the builders' client
CA ([Clients outside the cluster](#clients-outside-the-cluster) below). It is
off by default and changes nothing until set. (In-cluster runners got theirs
from the `arc-runners` chart's `buildkitTLS`; that chart is archived, last
released as 7.8.3.)

## Why a second set of builders

BuildKit's TLS settings (`--tlscert`, `--tlskey`, `--tlscacert`) are one
configuration for the whole process, applied to every `tcp://` `--addr`
(only `unix://` sockets stay plain; `cmd/buildkitd` `getListener`, checked
against v0.33.0). One daemon therefore cannot serve plain TCP to runners that
have not moved and mTLS to those that have. Two daemons cannot share a cache
directory either. So the transition is a second StatefulSet per architecture,
`buildkitd-mtls-<arch>`, TLS-only, with its own cache volume, beside the
plain `buildkitd-<arch>`:

1. `buildkitd.mtls.enabled: true`. Both sets run. A client chooses its
   builder by endpoint; moving, and moving back, is a client setting.
2. Clients move (in the shared workflows, by `CI_BUILDKIT_TLS_DIR`).
3. `buildkitd.mtls.retirePlain: true`. The plain StatefulSets, Services and
   policies stop rendering. Their cache volumes stay (a StatefulSet does not
   delete its claims); delete them by hand when the plain builders are
   confirmed unused.

The TLS builders start with a cold cache. The warm layer is
`cache-to type=registry`, which a new builder reads from the first build.

## In place: the existing builders become the TLS ones

`buildkitd.mtls.mode: inPlace` is the other way to the same end, for an
estate that wants neither a second node nor a cold cache. No second set
renders. The plain StatefulSet `buildkitd-<arch>` itself is changed:

- the server Secret and the client CA ConfigMap are mounted, and
  `--tlscert/--tlskey/--tlscacert` are set;
- the plain `tcp://` listener is gone (one process cannot serve both; every
  `tcp://` `--addr` is wrapped in the same TLS config);
- the unix socket stays plain, for the readiness probe;
- the Service, the `cache` volume claim and the node are the same, so the
  warm cache survives the restart.

There is no period in which both kinds are served. Every client of the
builders must dial with TLS from the moment the pod restarts, so the runners'
`buildkitTLS` and the endpoint they dial change in the **same release**. The
sequence:

1. Run the CI components that understand `CI_BUILDKIT_TLS_DIR` (callers on
   the release that carries them), and have the client CA, its policy and
   bundle in place (`clientCA.create: true` renders them whatever the mode).
2. In one change: `buildkitd.mtls.mode: inPlace` (with `enabled: true`), the
   runners' `buildkitTLS`, and the clients' endpoint and TLS variables. The
   builder pods restart once, onto the same volumes.
3. Rollback is the same change reversed: `mode: parallel` brings the plain
   listener back on the same cache, with the clients moved back.

`retirePlain` is refused in this mode: there is no plain set to retire. The
`mtls.networkPolicy.additionalIngress` entries admit the `buildkitd` pods
(the private gateway, for example), not a second set.

## The pieces

| What | Where | Notes |
|---|---|---|
| Server certificate, one per arch | `buildkitd.mtls.serverCertificate` | cert-manager `Certificate` from an issuer you name, names you give (`dnsNames.<arch>`). The key and certificate are mounted at `/etc/buildkit/tls`. |
| Client CA | `buildkitd.mtls.clientCA` | The only authority the builders accept clients from. `create: true` builds it: self-signed Issuer and CA Certificate in cert-manager's namespace, a CA `ClusterIssuer`, an approver-policy `CertificateRequestPolicy` (one common name, client auth, ECDSA, listed namespaces), and a trust-manager `Bundle` publishing the CA certificate, never its key, to the ConfigMap the builders read. |
| Client certificate | `buildkitd.mtls.clientCA.externalClients` | One `Certificate` per client name, delivered as `ca.pem`, `cert.pem`, `key.pem`; see [Clients outside the cluster](#clients-outside-the-cluster). (The archived `arc-runners` chart's `buildkitTLS` rendered the in-cluster runners' one and mounted it at `CI_BUILDKIT_TLS_DIR`.) |

A runner that dials an address which is not a SAN on the builders'
certificate (the in-cluster Service name, before the runners move to the
private names) sets `buildkitTLS.serverName`, which becomes
`CI_BUILDKIT_TLS_SERVERNAME` (the buildx `servername` driver option). It is
one value for both architectures, so list that name in both architectures'
`serverCertificate.dnsNames`.

The client CA is dedicated on purpose. Reusing a general private CA would
admit any certificate it issues for any other workload.

## Renewal and rotation

- **Client certificates** (runners) are read by a job when it starts; a pod is
  ephemeral. A renewal reaches the next job and breaks nothing in flight. The
  default is 90 days with renewal 30 days before expiry.
- **The client CA** lives ten years and its key is never rotated by
  cert-manager (`rotationPolicy: Never`). Replacing it is a new issuer name,
  both CAs in the ConfigMap for a while, then the old one dropped.
- **Server certificates** are loaded by `buildkitd` once, at start. cert-manager
  renews the Secret in place, and the daemon keeps serving the old pair until
  the pod restarts. The restart must happen before the old certificate
  expires, which with 90 days / renewal at 60 interrupts running builds on that
  builder. There is no in-process reload to rely on. With
  `mode: inPlace` the StatefulSets carry stakater/Reloader annotations
  (`buildkitd.mtls.reloader.enabled`, default on): where Reloader runs it
  restarts the pod when the Secret (or the client CA ConfigMap) changes, and
  that restart interrupts the builds running on that builder, once per
  renewal. Without Reloader, or in `parallel` mode, schedule it in a quiet
  window (`kubectl rollout restart statefulset/buildkitd-<arch>`, or
  `buildkitd-mtls-<arch>` in `parallel`), and alert on a `Certificate` that
  is not Ready.

## Clients outside the cluster

A runner outside Kubernetes (a serverless task, a VM) cannot mount the
Secret. It gets its own client identity instead of a copy of the in-cluster
runners', and the three files (`ca.pem`, `cert.pem`, `key.pem`) reach it from
a store the estate owns. `buildkitd.mtls.clientCA.externalClients` renders
both halves, per client name:

- a cert-manager `Certificate` in the release namespace, from the client CA's
  `ClusterIssuer`, with the entry's **own common name** (not `clientCA.commonName`;
  the chart refuses the runners' name, and two entries with one name);
- a `CertificateRequestPolicy` that admits exactly that common name, from
  exactly the release namespace, plus the `use` grant for cert-manager there.
  The in-cluster runners' policy is not bound in this namespace, and this one
  is bound nowhere else, so neither identity can ask for the other's name. The
  release namespace must not be in `requesterNamespaces`;
- with `push.enabled`, an External Secrets `PushSecret` that writes `ca.pem`
  (`push.caPem`, the bundle the client trusts the builders' server
  certificate by), `cert.pem` (`tls.crt`) and `key.pem` (`tls.key`) to the
  store named by `push.storeRef`, as three values named by `push.names`.
  `kmsKeyId` and `tags` go to the provider (Parameter Store `SecureString`,
  for one). It never deletes from the store (`deletionPolicy: None`).

The builders do not change: they verify the chain to the one client CA, and
nothing else about the client. Who is issued a certificate is the policy's
decision, so a new identity is a new entry, reviewed as one. The estate owns
what the chart cannot know: the store (and its credentials and prefix), the
key that encrypts the values, who may read them, and the consumer that writes
the three files into one directory and sets `CI_BUILDKIT_TLS_DIR`.

Renewal needs no human: cert-manager renews the Secret `renewBefore` ahead
of expiry, the `PushSecret` pushes when the Secret changes and every
`refreshInterval`, and a client that starts after that reads the new pair. A
client in flight keeps the one it read, which is the damage window of the
runners' certificate as well. Set `refreshInterval` well under `renewBefore`.

## What this does not do

Per-job certificates (federated identity, a short-lived client certificate
per job) are the later step; this gives every runner one certificate.
