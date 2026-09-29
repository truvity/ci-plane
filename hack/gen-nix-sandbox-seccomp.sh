#!/usr/bin/env bash
# Generates the seccomp profile a nixSandbox-enabled scale set needs
# (docs/nix-sandbox.md): containerd's own DEFAULT profile for a given
# containerd version and architecture, plus the ONE rule Nix's build
# sandbox needs on top of it. This is a generator, not part of `just
# check` -- it runs rarely (a containerd minor bump on the nodes a
# sandboxed set can land on) and needs things a laptop or CI runner does
# not always have (network access to fetch a pinned containerd module,
# and qemu-user to execute a foreign-arch binary). Ported from
# opwerm/nexus PR #306, which hand-generated one profile this way; see
# that PR's description for the worked example this script mechanizes.
#
# Usage:
#   hack/gen-nix-sandbox-seccomp.sh <containerd-version> <goarch> <out-file>
#
#   hack/gen-nix-sandbox-seccomp.sh v2.3.4 arm64 profiles/nix-sandbox.json
#
# <containerd-version> is a github.com/containerd/containerd/v2 module
# version (a `v*` tag). <goarch> is a Go GOARCH value (amd64, arm64, ...).
# The output is the raw JSON a Kubernetes `seccompProfiles` entry's
# `value` (Talos SysctlConfig-adjacent machine config) or a kubelet
# seccomp profile FILE expects verbatim -- write it to
# /var/lib/kubelet/seccomp/profiles/<name>.json on every node a
# nixSandbox-enabled scale set can land on (Talos: a `machine.
# seccompProfiles` patch, see docs/nix-sandbox.md).
#
# WHY THE TARGET ARCH HAS TO BE EXECUTED, NOT JUST CROSS-COMPILED.
# containerd's seccomp.DefaultProfile() branches on runtime.GOARCH to
# decide the 32-bit-compat syscall set (e.g. arm's fstat64/getresgid32
# family has no x86_64 equivalent) -- and runtime.GOARCH is a
# COMPILE-TIME constant baked into the binary by `GOARCH=<goarch> go
# build`, not read from the host CPU at run time. So an amd64 host CAN
# cross-compile an arm64 binary that would report the right answer --
# but it cannot RUN that binary at all without help, because it is
# foreign machine code. qemu-user (the `qemu-user-static` package,
# registered with binfmt_misc) is that help: it is the same path
# opwerm/nexus's generator used, and it is why this script insists on
# actually invoking the built binary rather than trusting the
# cross-compile alone.
#
# WHAT THE ADDED RULE IS AND WHY THESE SYSCALLS. containerd's default
# denies unshare/clone(CLONE_NEW*)/mount/pivot_root etc. to a container
# without CAP_SYS_ADMIN (contrib/seccomp/seccomp_default.go), and
# baseline pod security forbids adding that capability. Nix's own build
# sandbox (src/libstore/build/local-derivation-goal.cc's
# setupSandbox/enterChroot' in Nix, called from the derivation goal
# builder) unshares fresh mount, PID, IPC, UTS and (when available) user
# namespaces per build, bind-mounts an isolated root, and calls
# sethostname/setdomainname to rename the new UTS namespace before
# pivoting into it -- clone/clone3/unshare, mount and the new mount API
# (fsopen/fsconfig/fsmount/fspick/move_mount/open_tree/mount_setattr),
# umount2, pivot_root, sethostname, setdomainname. With the pod itself
# running `hostUsers: false` (rendered by nixSandbox.enabled;
# templates/runnersets.yaml), the kernel confines every one of those
# actions to namespaces the pod's OWN user namespace owns -- the same
# trade Docker's rootless mode makes. Nothing else containerd's default
# denies is un-denied (keyctl, bpf, perf_event_open, ptrace across
# namespaces, kexec, ... all stay refused).
set -euo pipefail

if [ "$#" -ne 3 ]; then
  echo "usage: $0 <containerd-version> <goarch> <out-file>" >&2
  exit 2
fi

containerd_version=$1
goarch=$2
out_file=$3

case "$goarch" in
  amd64) qemu_bin=qemu-x86_64-static ;;
  arm64) qemu_bin=qemu-aarch64-static ;;
  arm) qemu_bin=qemu-arm-static ;;
  *) echo "::error::unsupported goarch $goarch (add its qemu-user-static binary name here)" >&2; exit 2 ;;
esac

host_arch=$(go env GOARCH)
run_native=0
if [ "$goarch" = "$host_arch" ]; then
  run_native=1
elif ! command -v "$qemu_bin" >/dev/null 2>&1; then
  echo "::error::need $qemu_bin (qemu-user-static) to run a $goarch binary on this $host_arch host" >&2
  exit 2
fi

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT

cat > "$workdir/go.mod" <<EOF
module gen-nix-sandbox-seccomp

go 1.27

require github.com/containerd/containerd/v2 ${containerd_version}
EOF

# The generator itself: containerd's own default profile for the
# capability bounding set an ordinary (non-privileged) ARC runner pod
# gets, plus the one rule this sandbox needs, minus the ENOSYS stub
# containerd's default carries for clone3 -- without dropping that stub
# first, the new ALLOW rule for clone3 would sit BEHIND an existing
# ERRNO rule for the same syscall in the rendered profile's syscall
# list, and the kernel's BPF evaluates rules in order: the first match
# wins, so clone3 would still be denied.
cat > "$workdir/main.go" <<'EOF'
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/contrib/seccomp"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func main() {
	// The default CRI capability bounding set for a non-privileged pod
	// (containerd's cri/server default; no CAP_SYS_ADMIN, CAP_SYS_PTRACE
	// or other elevated caps a runner container does not get either).
	caps := []string{
		"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FSETID", "CAP_FOWNER",
		"CAP_MKNOD", "CAP_NET_RAW", "CAP_SETGID", "CAP_SETUID",
		"CAP_SETFCAP", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT", "CAP_KILL", "CAP_AUDIT_WRITE",
	}
	p := seccomp.DefaultProfile(&specs.Spec{
		Process: &specs.Process{
			Capabilities: &specs.LinuxCapabilities{Bounding: caps},
		},
	})

	extra := []string{
		"clone", "clone3", "unshare", "mount", "umount2", "pivot_root",
		"sethostname", "setdomainname", "fsopen", "fsconfig", "fsmount",
		"fspick", "move_mount", "open_tree", "mount_setattr",
	}
	extraSet := make(map[string]bool, len(extra))
	for _, n := range extra {
		extraSet[n] = true
	}

	// Drop any EXISTING rule that names one of the extra syscalls -- the
	// default profile's own ENOSYS/ERRNO handling for clone3 is the one
	// known instance, but this drops any other overlap the same way, so
	// the new ALLOW rule appended below is never shadowed by an earlier,
	// more restrictive rule for the same syscall.
	kept := p.Syscalls[:0]
	for _, s := range p.Syscalls {
		names := s.Names[:0]
		for _, n := range s.Names {
			if !extraSet[n] {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			s.Names = names
			kept = append(kept, s)
		}
	}
	p.Syscalls = kept

	p.Syscalls = append(p.Syscalls, specs.LinuxSyscall{
		Names:  extra,
		Action: specs.ActAllow,
	})

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	if err := enc.Encode(p); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
EOF

(
  cd "$workdir"
  GOFLAGS=-mod=mod GOOS=linux GOARCH="$goarch" go mod tidy
  GOFLAGS=-mod=mod GOOS=linux GOARCH="$goarch" go build -o gen .
)

mkdir -p "$(dirname "$out_file")"
if [ "$run_native" = 1 ]; then
  "$workdir/gen" > "$out_file"
else
  "$qemu_bin" "$workdir/gen" > "$out_file"
fi

echo "wrote $out_file (containerd $containerd_version, $goarch)"
echo "review the diff, then point a sandboxed scale set's nixSandbox.seccompProfile at it and install it on every node that set can land on (docs/nix-sandbox.md)"
