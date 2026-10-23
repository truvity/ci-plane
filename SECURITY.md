# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/ci-plane/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

## What is in scope

This repository publishes:

- The runner, nix-worker and cache-redirect images, built from `image/`.
- The chart `ci-builders`, as published to `oci://ghcr.io/truvity/charts`.
- The scripts under `hack/` that build and check the images and the chart.

The `arc-runners` chart is archived: its last release, 7.8.3, receives no
further updates, security ones included.

Reports that matter most:

- A sandbox or seccomp profile, a container default or a chart value that lets a job escape its runner or reach a neighbour, the node or the cluster.
- A runner or a Nix worker that accepts a login, a host key or a certificate it should refuse, or that keeps a credential between jobs.
- A build cache or read-through cache that can be poisoned by a pull request, or that serves one tenant's artifact to another.
- A secret or token reaching an image layer, a log line or a rendered manifest, or an image digest in the chart that does not match what was built.

A finding that depends on how a particular deployment uses this repository
belongs with that deployment's owner.
