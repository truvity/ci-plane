{{- define "arc-runners.image" -}}
{{- if .registry }}{{ .registry }}/{{ end }}{{ .repository }}:{{ .tag }}
{{- end -}}

{{- define "arc-runners.runnerImage" -}}
{{- $i := .Values.runnerImage -}}
{{- if $i.digest -}}
{{ $i.registry }}/{{ $i.repository }}@{{ $i.digest }}
{{- else -}}
{{ $i.registry }}/{{ $i.repository }}:{{ $i.tag }}
{{- end -}}
{{- end -}}

{{- /*
Organization slug from githubConfigUrl (last path segment) — mirrors
the upstream chart's actions.github.com/organization label.
*/ -}}
{{- define "arc-runners.org" -}}
{{- .Values.githubConfigUrl | trimSuffix "/" | splitList "/" | last -}}
{{- end -}}

{{- /*
NIX_CONFIG is immutable and content-addressed: changing one transfer
knob changes the runner pod template reference and lets Argo prune the
old ConfigMap after ARC has converged. The image's nix.conf continues to
own substituters; this overlay names only retry/build scheduling keys.

With nixBuilders disabled the payload is `nixConfig` verbatim, so the
ConfigMap, its name and the scale sets' values hash are byte-identical
to a chart without remote builders. Enabled, it appends the machines
file reference; an EMPTY machines file means no remote builders and
leaves Nix's ordinary local builder in charge.
*/ -}}
{{- define "arc-runners.nixConfig" -}}
{{- $conf := .Values.nixConfig | default "" -}}
{{- /*
v4.1.0: nixCache.url, when set, names a full substituter list (the
estate's cache first, upstream as the automatic fallback) -- the same
"name the FULL list, not `extra-`" rule image/runner/Dockerfile's own
nix.conf follows, for the same reason: the `extra-` form APPENDS to
Nix's default, so the list reads upstream-first and the estate's own
cache is never chosen. This overlay BEATS the image's baked nix.conf
(NIX_CONFIG wins), so a fleet whose cache moved -- or never had one --
never needs a rebuilt image to say so. */ -}}
{{- if .Values.nixCache.url -}}
{{- if $conf }}{{ $conf = printf "%s\n" ($conf | trimSuffix "\n") }}{{ end -}}
{{- $conf = printf "%ssubstituters = %s https://cache.nixos.org/\n" $conf .Values.nixCache.url -}}
{{- end -}}
{{- if .Values.nixBuilders.enabled -}}
{{- if $conf }}{{ $conf = printf "%s\n" ($conf | trimSuffix "\n") }}{{ end -}}
{{- $conf = printf "%sbuilders = @/etc/nix-builders/machines\nbuilders-use-substitutes = true\n" $conf -}}
{{- end -}}
{{- $conf -}}
{{- end -}}

{{- /*
The effective machine-login user: `login.user`, absorbing the DEPRECATED
top-level `sshUser` (kept for the one existing consumer that already
sets it; see values.yaml). "nixremote" is the shared default for BOTH
fields, so it is the sentinel for "not customized" -- there is no other
way to tell "left at default" from "set back to the same string" in
Helm. Only when `sshUser` is customized does it matter at all:

  sshUser default, login.user anything  -> login.user (the normal path)
  sshUser custom,  login.user default   -> sshUser (the alias)
  sshUser custom,  login.user == sshUser -> either, they agree
  sshUser custom,  login.user custom, different -> fail: an upgrade must
    not silently pick one of two conflicting logins.
*/ -}}
{{- define "arc-runners.nixBuilderUser" -}}
{{- $b := .Values.nixBuilders -}}
{{- $sshUser := $b.sshUser -}}
{{- $loginUser := $b.login.user -}}
{{- if ne $sshUser "nixremote" -}}
{{- if and (ne $loginUser "nixremote") (ne $loginUser $sshUser) -}}
{{- fail (printf "nixBuilders.sshUser (%q) and nixBuilders.login.user (%q) disagree. nixBuilders.sshUser is DEPRECATED (removal in a later release) and only an alias for nixBuilders.login.user -- set login.user alone." $sshUser $loginUser) -}}
{{- end -}}
{{- $sshUser -}}
{{- else -}}
{{- $loginUser -}}
{{- end -}}
{{- end -}}

{{- /*
The store URI scheme is login.protocol -- "ssh" (default) or "ssh-ng" --
so a cut-over to the modern protocol changes only this one token per
line; the field layout (system, ssh-key path, maxjobs, speed-factor,
supported/mandatory features) is Nix's machine-file format and is the
same for both schemes.
*/ -}}
{{- define "arc-runners.nixMachines" -}}
{{- $login := .Values.nixBuilders.login }}
{{- $user := include "arc-runners.nixBuilderUser" . }}
{{- range $builder := .Values.nixBuilders.builders }}
{{- $supported := "-" }}
{{- if $builder.supportedFeatures }}{{ $supported = join "," $builder.supportedFeatures }}{{ end }}
{{- $mandatory := "-" }}
{{- if $builder.mandatoryFeatures }}{{ $mandatory = join "," $builder.mandatoryFeatures }}{{ end }}
{{ $login.protocol }}://{{ $user }}@{{ $builder.host }} {{ $builder.system }} /home/runner/.ssh/nix-builder {{ $builder.maxJobs }} {{ $builder.speedFactor }} {{ $supported }} {{ $mandatory }}
{{- end }}
{{- end -}}

{{- /*
Host certificates, phase 1. Renders one `@cert-authority` line per
nixBuilders.knownHosts.certAuthorities entry, joined with newlines; an
empty list renders "". Appended by nix-worker-client's setup binary to
the pinned known_hosts Secret content -- both stay valid at once during
a migration from one static host key per worker to one CA line trusting
every host it signs for.
*/ -}}
{{- define "arc-runners.nixKnownHostsCertAuthorities" -}}
{{- $lines := list -}}
{{- range $i, $ca := .Values.nixBuilders.knownHosts.certAuthorities -}}
{{- if not $ca.hosts }}{{ fail (printf "nixBuilders.knownHosts.certAuthorities[%d].hosts is required" $i) }}{{ end -}}
{{- if not ($ca.key | trim) }}{{ fail (printf "nixBuilders.knownHosts.certAuthorities[%d].key is required" $i) }}{{ end -}}
{{- $lines = append $lines (printf "@cert-authority %s %s" (join "," $ca.hosts) $ca.key) -}}
{{- end -}}
{{- join "\n" $lines -}}
{{- end -}}

{{- define "arc-runners.nixSSHConfig" -}}
Host{{ range $builder := .Values.nixBuilders.builders }} {{ $builder.host }}{{ end }}
  BatchMode yes
  IdentitiesOnly yes
  IdentityFile /home/runner/.ssh/nix-builder
  CertificateFile /home/runner/.ssh/nix-builder-cert.pub
  UserKnownHostsFile /home/runner/.ssh/known_hosts
  StrictHostKeyChecking yes
  PasswordAuthentication no
  KbdInteractiveAuthentication no
{{- end -}}

{{- /*
ACTIONS_RUNNER_HOOK_JOB_STARTED. The runner runs it as the `runner`
user before the job's first step. A certificate lives at most one hour
(the signing role's ceiling) and a warm pod can idle far longer, so the
init container's certificate only serves a job that starts soon after
the pod; this hook signs a fresh one for the job that actually runs.

It never fails the job. The setup binary already writes an empty
machines file before contacting OpenBao and exits 0 when signing fails;
anything worse (bad config, a crash, the timeout) is caught here and
ALSO empties the machines file and drops the key, so Nix builds locally.
Neither the upstream runner image nor ARC sets this hook, so nothing is
replaced; an estate that adds its own must call this script from it.
*/ -}}
{{- define "arc-runners.nixJobStartedHook" -}}
#!/bin/bash
set -u
machines="${NIX_BUILDER_MACHINES_DIR}/machines"
disable_remote() {
  rm -f "${NIX_BUILDER_SSH_DIR}/nix-builder" \
        "${NIX_BUILDER_SSH_DIR}/nix-builder.pub" \
        "${NIX_BUILDER_SSH_DIR}/nix-builder-cert.pub" 2>/dev/null
  : > "$machines" 2>/dev/null || rm -f "$machines" 2>/dev/null
}
if ! timeout 120 /usr/local/bin/nix-worker-client-setup; then
  disable_remote
fi
if [ -s "$machines" ]; then
  echo "nix builders: signed a fresh certificate for this job; remote builders enabled"
else
  echo "nix builders: no certificate for this job; Nix builds locally"
fi
exit 0
{{- end -}}

{{- /*
The setup binary's environment, shared by the init container (cold
pod) and the runner container (the job-started hook). Nothing here is
secret: the credential is the projected token, mounted separately.
*/ -}}
{{- define "arc-runners.nixBuilderEnv" -}}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: POD_UID
  valueFrom:
    fieldRef:
      fieldPath: metadata.uid
- name: NIX_BUILDER_OPENBAO_ADDRESS
  value: {{ .Values.nixBuilders.openbao.address | quote }}
- name: NIX_BUILDER_OPENBAO_NAMESPACE
  value: {{ .Values.nixBuilders.openbao.namespace | quote }}
- name: NIX_BUILDER_OPENBAO_AUTH_MOUNT
  value: {{ .Values.nixBuilders.openbao.authMount | quote }}
- name: NIX_BUILDER_OPENBAO_AUTH_ROLE
  value: {{ .Values.nixBuilders.openbao.authRole | quote }}
- name: NIX_BUILDER_OPENBAO_SSH_MOUNT
  value: {{ .Values.nixBuilders.openbao.sshMount | quote }}
- name: NIX_BUILDER_OPENBAO_SSH_ROLE
  value: {{ .Values.nixBuilders.openbao.sshRole | quote }}
- name: NIX_BUILDER_CERTIFICATE_TTL
  value: {{ .Values.nixBuilders.openbao.certificateTTL | quote }}
- name: NIX_BUILDER_REQUEST_TIMEOUT
  value: {{ .Values.nixBuilders.openbao.requestTimeout | quote }}
- name: NIX_BUILDER_PRINCIPAL
  value: {{ .Values.nixBuilders.login.principal | quote }}
- name: NIX_BUILDER_PROTOCOL
  value: {{ .Values.nixBuilders.login.protocol | quote }}
- name: NIX_BUILDER_KNOWN_HOSTS_PINNED
  value: {{ .Values.nixBuilders.knownHosts.pinned | quote }}
{{- end -}}

{{- /*
Disabled: exactly the pre-nixBuilders name (hash of nixConfig alone).
Enabled: hash every immutable ConfigMap input, rendered — changing a
builder, SSH policy, OpenBao route, CA bundle or the hook script must
rotate the name and the AutoscalingRunnerSet values hash rather than
attempting to mutate an immutable object in place.
*/ -}}
{{- define "arc-runners.nixConfigName" -}}
{{- if .Values.nixBuilders.enabled -}}
{{- $payload := dict "nixConfig" (include "arc-runners.nixConfig" .) "nixBuilders" .Values.nixBuilders "machines" (include "arc-runners.nixMachines" .) "sshConfig" (include "arc-runners.nixSSHConfig" .) "jobStartedHook" (include "arc-runners.nixJobStartedHook" .) -}}
runner-nix-config-{{ $payload | toJson | sha256sum | trunc 12 }}
{{- else -}}
runner-nix-config-{{ .Values.nixConfig | sha256sum | trunc 12 }}
{{- end -}}
{{- end -}}

{{- /*
v4.1.0: a scale set's effective NIX_CONFIG payload — the release-wide
base (arc-runners.nixConfig) above, with THIS set's extraNixConfig
APPENDED (Nix reads its config top-to-bottom and keeps the LAST
occurrence of a scalar key, so appending is how a later key wins), and,
when nixSandbox.enabled, `sandbox = true` / `sandbox-fallback = false`
appended LAST of all — after extraNixConfig, not before — so nothing an
estate puts in extraNixConfig can quietly re-enable the unsandboxed
fallback on a set that asked to be sandboxed.

A scale set with neither key renders BYTE-IDENTICAL to the base
(arc-runners.nixConfig), which is what keeps an existing release's
ConfigMap name and values-hash unchanged across this upgrade — the
golden proof for the default case.

Takes a dict `{root, p}`: `root` is the chart's `.`, `p` the one scale
set's value block (`.Values.scaleSets.<name>`).
*/ -}}
{{- define "arc-runners.nixConfigFor" -}}
{{- $root := .root -}}
{{- $p := .p -}}
{{- $conf := include "arc-runners.nixConfig" $root -}}
{{- $extra := $p.extraNixConfig | default "" -}}
{{- if $extra -}}
{{- if $conf }}{{ $conf = printf "%s\n" ($conf | trimSuffix "\n") }}{{ end -}}
{{- $conf = printf "%s%s\n" $conf (trimSuffix "\n" $extra) -}}
{{- end -}}
{{- $sandbox := $p.nixSandbox | default (dict) -}}
{{- if $sandbox.enabled -}}
{{- if $conf }}{{ $conf = printf "%s\n" ($conf | trimSuffix "\n") }}{{ end -}}
{{- $conf = printf "%ssandbox = true\nsandbox-fallback = false\n" $conf -}}
{{- end -}}
{{- $conf -}}
{{- end -}}

{{- /*
The ConfigMap name for a scale set's effective payload above. A set
with neither extraNixConfig nor nixSandbox.enabled gets EXACTLY
arc-runners.nixConfigName — the same object every other unmodified set
already shares — so nix-config.yaml renders one ConfigMap for the whole
release, as before. A set that customises either gets its OWN
content-addressed name, so changing one set's extra config or turning
its sandbox on never touches another set's ConfigMap or recycles its
listener.
*/ -}}
{{- define "arc-runners.nixConfigNameFor" -}}
{{- $root := .root -}}
{{- $p := .p -}}
{{- $extra := $p.extraNixConfig | default "" -}}
{{- $sandbox := $p.nixSandbox | default (dict) -}}
{{- if or $extra $sandbox.enabled -}}
{{- $payload := dict "nixConfig" (include "arc-runners.nixConfigFor" (dict "root" $root "p" $p)) "nixBuilders" $root.Values.nixBuilders -}}
{{- if $root.Values.nixBuilders.enabled -}}
{{- $_ := set $payload "machines" (include "arc-runners.nixMachines" $root) -}}
{{- $_ := set $payload "sshConfig" (include "arc-runners.nixSSHConfig" $root) -}}
{{- $_ := set $payload "jobStartedHook" (include "arc-runners.nixJobStartedHook" $root) -}}
{{- end -}}
runner-nix-config-{{ $payload | toJson | sha256sum | trunc 12 }}
{{- else -}}
{{- include "arc-runners.nixConfigName" $root -}}
{{- end -}}
{{- end -}}

{{- /*
Runner packing (see values.packing). The label is the chart's own, not
one the ARC controller stamps: the controller's labels are internal to
its version, and a renamed one would turn packing back into spreading
without a single error. Every runner pod of every release of this chart
carries it, whatever the scale set and whatever the organization.
*/ -}}
{{- define "arc-runners.packingLabels" -}}
ci-plane.io/packing-group: runners
{{- end -}}

{{- /*
The runner pod's affinity: this scale set's own `affinity` if it set
one (v4.1.0, REPLACING the release-wide value, same hasKey rule as the
other per-set overrides), else the release-wide `affinity`, with the
packing term APPENDED to podAffinity's preferred list, so either way a
caller's own node or pod affinity keeps working. Built on a deep copy:
this runs once per scale set, and appending to the values map itself
would give the second set two packing terms and the third set three.

namespaceSelector {} is load-bearing: without it a pod affinity term
matches pods in the pod's OWN namespace only, and each organization is
its own namespace, so two organizations' runners would never share a
node. Empty output means no affinity stanza at all.

Takes a dict `{root, p}`: `root` is the chart's `.`, `p` the one scale
set's value block.
*/ -}}
{{- define "arc-runners.affinity" -}}
{{- $root := .root -}}
{{- $p := .p -}}
{{- $base := $root.Values.affinity -}}
{{- if hasKey $p "affinity" }}{{ $base = $p.affinity }}{{ end -}}
{{- $aff := deepCopy ($base | default (dict)) -}}
{{- if $root.Values.packing.enabled -}}
{{- $weight := int $root.Values.packing.weight -}}
{{- if or (lt $weight 1) (gt $weight 100) }}{{ fail (printf "packing.weight must be between 1 and 100, got %v" $root.Values.packing.weight) }}{{ end -}}
{{- $podAffinity := get $aff "podAffinity" | default (dict) -}}
{{- $preferred := get $podAffinity "preferredDuringSchedulingIgnoredDuringExecution" | default (list) -}}
{{- $term := dict
      "weight" $weight
      "podAffinityTerm" (dict
        "topologyKey" "kubernetes.io/hostname"
        "namespaceSelector" (dict)
        "labelSelector" (dict "matchLabels" (include "arc-runners.packingLabels" $root | fromYaml))) -}}
{{- $_ := set $podAffinity "preferredDuringSchedulingIgnoredDuringExecution" (append $preferred $term) -}}
{{- $_ := set $aff "podAffinity" $podAffinity -}}
{{- end -}}
{{- if $aff }}{{ toYaml $aff }}{{ end -}}
{{- end -}}
