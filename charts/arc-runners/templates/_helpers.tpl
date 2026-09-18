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
{{- if .Values.nixBuilders.enabled -}}
{{- if $conf }}{{ $conf = printf "%s\n" ($conf | trimSuffix "\n") }}{{ end -}}
{{- $conf = printf "%sbuilders = @/etc/nix-builders/machines\nbuilders-use-substitutes = true\n" $conf -}}
{{- end -}}
{{- $conf -}}
{{- end -}}

{{- define "arc-runners.nixMachines" -}}
{{- range $builder := .Values.nixBuilders.builders }}
{{- $supported := "-" }}
{{- if $builder.supportedFeatures }}{{ $supported = join "," $builder.supportedFeatures }}{{ end }}
{{- $mandatory := "-" }}
{{- if $builder.mandatoryFeatures }}{{ $mandatory = join "," $builder.mandatoryFeatures }}{{ end }}
ssh://{{ $.Values.nixBuilders.sshUser }}@{{ $builder.host }} {{ $builder.system }} /home/runner/.ssh/nix-builder {{ $builder.maxJobs }} {{ $builder.speedFactor }} {{ $supported }} {{ $mandatory }}
{{- end }}
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
