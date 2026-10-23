# Nix's build sandbox on a scale set — archived

This page documented `scaleSets.<name>.nixSandbox` and its fence, values
of the `arc-runners` chart, and `hack/gen-nix-sandbox-seccomp.sh`, which
generated the node-side seccomp profile those scale sets needed. That
chart is archived; its last release is 7.8.3, which stays published at
`oci://ghcr.io/truvity/charts/arc-runners`.

The page and the generator as they stood for that release:

- **<https://github.com/truvity/ci-plane/blob/v7.8.3/docs/nix-sandbox.md>**
- **<https://github.com/truvity/ci-plane/blob/v7.8.3/hack/gen-nix-sandbox-seccomp.sh>**
