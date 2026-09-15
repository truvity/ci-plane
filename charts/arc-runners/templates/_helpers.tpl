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
NIX_CONFIG is immutable and content-addressed. The image's nix.conf
continues to own substituters; this overlay names only retry/build
scheduling keys. An empty machines file means no remote builders and
leaves Nix's ordinary local builder enabled.
*/ -}}
{{- define "arc-runners.nixConfig" -}}
{{- with .Values.nixConfig }}
{{ . | trimSuffix "\n" }}
{{- end }}
{{- if .Values.nixBuilders.enabled }}
builders = @/etc/nix-builders/machines
builders-use-substitutes = true
{{- end }}
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
Hash every immutable ConfigMap input, not only nix.conf: changing a
builder, SSH policy, OpenBao route or CA bundle must rotate the name and
the AutoscalingRunnerSet values hash rather than attempting to mutate an
immutable object in place.
*/ -}}
{{- define "arc-runners.nixConfigName" -}}
{{- $payload := dict "nixConfig" (include "arc-runners.nixConfig" .) "nixBuilders" .Values.nixBuilders -}}
runner-nix-config-{{ $payload | toJson | sha256sum | trunc 12 }}
{{- end -}}
