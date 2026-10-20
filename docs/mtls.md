# Mutual TLS for the BuildKit builders

The builders (`ci-builders`, `buildkitd`) serve plain TCP on :1234 by
default, gated by a NetworkPolicy alone. `buildkitd.mtls` adds builders that
terminate mutual TLS themselves, which is what a TLS-passthrough gateway in
front of them needs, and `arc-runners` `buildkitTLS` gives the runners the
client certificate. Both are off by default and change nothing until set.

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

## The pieces

| What | Where | Notes |
|---|---|---|
| Server certificate, one per arch | `buildkitd.mtls.serverCertificate` | cert-manager `Certificate` from an issuer you name, names you give (`dnsNames.<arch>`). The key and certificate are mounted at `/etc/buildkit/tls`. |
| Client CA | `buildkitd.mtls.clientCA` | The only authority the builders accept clients from. `create: true` builds it: self-signed Issuer and CA Certificate in cert-manager's namespace, a CA `ClusterIssuer`, an approver-policy `CertificateRequestPolicy` (one common name, client auth, ECDSA, listed namespaces), and a trust-manager `Bundle` publishing the CA certificate, never its key, to the ConfigMap the builders read. |
| Client certificate | `arc-runners` `buildkitTLS` | `certificate.create: true` renders the `Certificate` in the release namespace; the Secret is mounted with the trust bundle as `ca.pem`, `cert.pem`, `key.pem` at `mountPath`, and `CI_BUILDKIT_TLS_DIR` points there. |

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
  builder: schedule it in a quiet window (`kubectl rollout restart
  statefulset/buildkitd-mtls-<arch>`), and alert on a `Certificate` that is
  not Ready. There is no in-process reload to rely on.

## Fargate and other runners outside the cluster

A runner outside Kubernetes cannot mount the Secret. The same client
certificate (`tls.crt`, `tls.key`) and the trust bundle go to AWS Secrets
Manager and are injected into the task as files named `cert.pem`, `key.pem`
and `ca.pem` in one directory, with `CI_BUILDKIT_TLS_DIR` set to it.

The usual path is an External Secrets Operator `PushSecret` reading the
`buildkit-client-tls` Secret and writing a Secrets Manager secret (the key
and certificate as two keys, or as one JSON document). Renewal then flows from
cert-manager to the secret without a human. Not implemented here: it belongs
to the estate that owns the Secrets Manager store and the task definition.

## What this does not do

Per-job certificates (federated identity, a short-lived client certificate
per job) are the later step; this gives every runner one certificate.
