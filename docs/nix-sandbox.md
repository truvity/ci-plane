# Nix's build sandbox on a scale set (v3.1.0)

## Why this exists

The runner image's Nix is single-user (`image/runner/Dockerfile`): every
build runs as the same uid as the job, in the same pod, alongside
whatever else that job's workflow does. With Nix's own sandbox OFF (its
default when no other sandboxing is available, `sandbox-fallback =
true`), a derivation's builder can write into ANOTHER derivation's
output path before it is signed and uploaded, and can read
`/proc/<pid>/environ` of every other process in the pod — including the
runner's own, which holds `ACTIONS_ID_TOKEN_REQUEST_TOKEN` and
`ACTIONS_ID_TOKEN_REQUEST_URL`: the ability to mint the job's GitHub OIDC
token and, through whatever that token is federated to, assume the job's
cloud upload role. This is true on every estate that runs this image
today, Truvity's own included — nothing about it is estate-specific.

Nix's sandbox needs Linux namespaces it cannot create for itself inside
an ordinary pod: a fresh mount, PID, IPC and UTS namespace per build,
and — the part that actually blocks it here — its own user namespace,
because the sandbox's `chroot`-like isolation runs as an unprivileged
build user that needs `CAP_SYS_ADMIN`-gated syscalls (`unshare`, `mount`,
`pivot_root`, namespace-creating `clone`) without actually holding
`CAP_SYS_ADMIN` on the node. A Kubernetes pod gets exactly that trade
through `hostUsers: false`: the pod's own (outer) user namespace lets an
unprivileged process inside it act with root-equivalent power over
namespaces IT owns, never the node's.

## What `nixSandbox` renders

Per scale set (`scaleSets.<name>.nixSandbox`), off by default:

```yaml
scaleSets:
  nix-builder-arm64:
    nixSandbox:
      enabled: true
      seccompProfile: profiles/nix-sandbox.json
```

Enabling it renders, on that set's runner pods only:

- `hostUsers: false` on the pod — the outer user namespace above.
- `procMount: Unmasked` on the runner container — Nix's sandbox mounts a
  fresh `/proc` inside its own PID namespace per build, which the kernel
  refuses while the container's own `/proc` has masked paths (the
  default). PodSecurity `baseline` allows `procMount: Unmasked` ONLY
  when `hostUsers` is also `false` (Kubernetes ≥1.35's
  `check_procMount_baseline.go`) — the two always render together in
  this chart, never one without the other.
- `seccompProfile: {type: Localhost, localhostProfile: <path>}` on the
  runner container. Kubelet's own `seccompDefault: true` (or an
  unannotated pod on a cluster that sets it, which Talos does) applies
  containerd's DEFAULT profile, and that profile denies `unshare`,
  `mount`, `pivot_root` and namespace-creating `clone` to a container
  without `CAP_SYS_ADMIN` — exactly what `hostUsers: false` does NOT
  grant. `baseline` forbids both `CAP_SYS_ADMIN` and `Unconfined`, but
  allows ANY `Localhost` profile, so a profile naming the extra syscalls
  is the only way to stay inside `baseline`.
- `sandbox = true` and `sandbox-fallback = false` appended to that set's
  `NIX_CONFIG`, LAST — after any `extraNixConfig` — so nothing else that
  set sets can quietly turn the fallback back on. `sandbox-fallback =
  false` is deliberate: a pod missing any of the pieces above should
  FAIL the build loudly, not silently fall back to the unsandboxed
  behaviour this feature exists to stop.

`seccompProfile` is REQUIRED the moment `enabled: true` — refused at
render time otherwise (`templates/runnersets.yaml`, and the same check
in `templates/nix-sandbox-fence.yaml` for the fence below). There is no
default path: a profile is a node-side artifact this chart cannot ship
inside a values default.

## What the chart does NOT do for you

**The profile file itself.** `localhostProfile` names a JSON file the
kubelet expects at `/var/lib/kubelet/seccomp/profiles/<path>` on every
node this scale set can land on. Generate it with
`hack/gen-nix-sandbox-seccomp.sh <containerd-version> <goarch> <out-file>`
— containerd's own default profile for that version/arch, plus the one
rule Nix's sandbox needs (see the script's own top-of-file comment for
exactly which syscalls and why). Regenerate it whenever the node's
containerd minor version bumps: the default profile grows syscalls over
time, and a stale copy denies new ones with `EPERM` in a build that used
to work. Installing the file is estate-specific (Talos: a
`machine.seccompProfiles` patch; see opwerm/nexus's
`talos/patches/nix-sandbox-seccomp.yaml` for a worked example this
script's output slots into).

**User namespaces on the node.** `hostUsers: false` asks containerd for
a user namespace; the kernel/node has to allow creating one at all.
Talos's KSPP hardening sets `user.max_user_namespaces = 0` by default,
which refuses EVERY user namespace, including this one. An estate that
wants a sandboxed set needs `user.max_user_namespaces` raised above zero
on whatever nodes that set can land on (Talos:
`SysctlConfig user.max_user_namespaces: "<N>"`; opwerm/nexus's
`talos/patches/user-namespaces.yaml` is a worked, heavily-commented
example, including the security trade-off it accepts and why it scopes
the sysctl to one node rather than the whole worker pool). This is a
kernel attack surface decision for your estate to make deliberately, not
a default this chart can pick for you.

## The fence: `nixSandbox.fence.enabled`

A `Localhost` seccomp profile is a FILE ON THE NODE, and `baseline` pod
security admits ANY `Localhost` profile — the profile's own content is
never inspected at admission. Once the profile above exists on a node,
ANY pod the scheduler puts on that node can name it, including one with
`hostUsers` left `true`, where the extra `mount`/namespace-creation
rules would act on the NODE's own namespaces rather than a contained
one. Turning `nixSandbox.enabled` on for one scale set, without also
fencing the profile, protects that set's OWN pods (they get the
namespace they need) but does nothing to stop a different, unrelated pod
on the same node from asking for the same file.

`nixSandbox.fence.enabled` (release-wide, off by default) renders a
`ValidatingAdmissionPolicy` and its binding — cluster-scoped resources,
named after the release so two organizations' releases in one cluster
never collide. `failurePolicy: Fail`. For each scale set with
`nixSandbox.enabled`, it adds one validation: any Pod, `kubectl debug`
ephemeral-container update, Deployment, StatefulSet, DaemonSet,
ReplicaSet, ReplicationController, Job or CronJob naming that set's
profile (pod-level OR any container's `securityContext`) is refused
UNLESS the object is a Pod in this release's own namespace, carrying
that set's `actions.github.com/scale-set-name` label (which ARC 0.14.2
copies down from the `AutoscalingRunnerSet` to every runner pod it
creates), with `hostUsers: false`. Every field is read with CEL's
optional-chaining (`?.`, `orValue`) so no expression can error on a
valid object that simply omits a field — under a cluster-wide `Fail`
policy, an expression error would refuse every pod in the cluster,
including whatever would have fixed it.

Refused at render time if `fence.enabled: true` with no scale set
actually sandboxed: a fence with nothing to protect is very likely a
values typo, not an intentional no-op.

**This is one node's worth of protection, not a network boundary.** The
fence stops another WORKLOAD from using the profile; it says nothing
about who may submit a workflow that lands on this scale set in the
first place (that is the GitHub-side runner-group/repository-visibility
question — see `runnerGroup` in `values.yaml`), and nothing about what
one job running on the sandboxed set can do to another job that also
lands there, beyond what Nix's own sandbox isolates per build.

## Estate checklist

1. Generate the profile: `hack/gen-nix-sandbox-seccomp.sh <containerd
   version> <goarch> profiles/nix-sandbox.json` (adjust the name if more
   than one sandboxed set needs its own — see below).
2. Install it on every node the sandboxed set can land on, and raise
   `user.max_user_namespaces` above zero there (both estate/node-level,
   not this chart).
3. Set `scaleSets.<name>.nixSandbox.enabled: true` and
   `seccompProfile:` to the installed path, and pin that set to those
   nodes (`nodeSelector`/`tolerations`/`affinity`, all overridable per
   set since v3.1.0 — see `values.yaml`).
4. Turn on `nixSandbox.fence.enabled` once step 3 is live, so the
   profile file protects only this set's own pods.
5. Regenerate the profile on a containerd minor bump on those nodes.

**Two sandboxed sets, two profiles.** The fence's validation is written
per scale set, naming that set's own profile literal — reusing one
profile path across two sets would let either set's pods satisfy the
other's fence rule (same file, same `contains` check), which defeats the
point of naming the set at all. Generate and install a distinct
`localhostProfile` path per sandboxed set if you need more than one.
