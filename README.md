# ci-plane

**This repository is retired (2026-10-29).** It is archived and receives
no further commits, tags, releases or dependency updates. Its contents
have moved to the successor repositories below, or are retired with no
successor.

Published images and charts stay pullable at their existing versions.

## What moved where

| Shipped here | Now at |
|---|---|
| Runner image (`image/runner`) | [truvity/grun](https://github.com/truvity/grun), `ghcr.io/truvity/grun/runner` (from grun v0.1.0) |
| `deploy/pulumi/runneridentity` | [truvity/ci-support](https://github.com/truvity/ci-support) `pkg/runneridentity` (ci-support v1.1.0, same API) |
| `charts/ci-builders` | [truvity/ci-support](https://github.com/truvity/ci-support), `oci://ghcr.io/truvity/ci-support/charts/ci-builders` |
| `image/cache-redirect` | [truvity/ci-support](https://github.com/truvity/ci-support), `ghcr.io/truvity/ci-support/cache-redirect` |
| `charts/arc-runners`, `image/runner-watchdog` | Retired, no successor |
| `image/nix-worker` | Retired, no successor |
