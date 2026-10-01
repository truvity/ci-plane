# Restricted runners

`scaleSets.<name>.restricted: true` runs one scale set on the Kubernetes Pod
Security `restricted` profile, so the namespace that holds it can enforce
that profile instead of `baseline`. It is off by default and per set: a set
that does not ask for it renders exactly as before.

## What the profile changes

| Where | Before | With `restricted: true` |
|---|---|---|
| runner container | no `securityContext`; the image's `USER runner` (a name) | `runAsNonRoot`, `runAsUser`/`runAsGroup` from `runnerUser` (1001), `allowPrivilegeEscalation: false`, capabilities dropped |
| pod | none | `fsGroup` = `runnerUser.gid`, `seccompProfile: RuntimeDefault` |
| `prepare-nix-builder-ssh` init container (only with `nixBuilders`) | uid 0, adds `CHOWN` and `DAC_OVERRIDE` | the runner's uid, no capabilities |
| listener pod (controller namespace) | none | non-root, `RuntimeDefault`, no escalation, capabilities dropped |

The runner container already ran as the non-root `runner` user. What the
profile removes is what that user could still do: gain privilege through a
setuid binary or a file capability, and call the syscalls the container
runtime's default seccomp profile blocks.

The only process that ran as root was the init container. It created the two
`emptyDir` mounts' contents and handed them to `runner`. Under the profile the
pod's `fsGroup` makes those mounts group-writable, the init container runs as
`runner`, and `nix-worker-client-setup` skips the `chmod`/`chown` it may not do
on a mount point it does not own (it still checks that it can write there).

`runnerUser` is the numeric login of the image's `runner` user. The upstream
actions-runner base image uses 1001:1001; change it only for a runner image
built on a different uid.

The janitor CronJob carries `seccompProfile: RuntimeDefault` for every release
(its container was already restricted), so a namespace can enforce the profile
once every set in it is restricted.

## What a job loses

- **`sudo`, `su` and any setuid binary.** `allowPrivilegeEscalation: false`
  sets `no_new_privs`, so a setuid or file-capability binary runs without its
  privilege. `sudo` fails with "effective uid is not 0". A step that installs
  packages (`apt-get`) or writes under `/usr`, `/etc` or `/opt` fails; put the
  tool in the repository's `devbox.json` instead.
- **File capabilities**, such as `ping` (`cap_net_raw`).
- **Syscalls outside the runtime's default seccomp profile.** The ones a CI job
  can meet: `unshare`/`clone` with `CLONE_NEWUSER` (a browser's own sandbox,
  `bwrap`, `unshare -U`), `io_uring`, `perf_event_open`. Nix's build sandbox
  needs a user namespace; with the default `sandbox-fallback` it falls back to
  an unsandboxed build, and `nixSandbox` is the way to ask for it explicitly.
- **Rootless container tooling that needs `newuidmap`** (setuid).

What does not change: remote builds over the `docker buildx` remote driver and
the Nix remote builders (neither needs a daemon or root), `devbox`, the
single-user Nix store under `/nix` (owned by `runner`), Go and Node toolchains
delivered per job.

## Rolling out

1. Add the set beside the existing ones (never by editing a set that carries
   traffic) and send a few low-risk repositories to it by label.
2. Watch for `sudo`, `apt-get` and permission errors in those jobs, and for
   Pod Security warnings or audit events naming the set's pods: there should be
   none.
3. Only when every set in the namespace is restricted, label the namespace
   `pod-security.kubernetes.io/enforce: restricted`.
