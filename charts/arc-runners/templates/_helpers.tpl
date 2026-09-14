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
old ConfigMap after ARC has converged.
*/ -}}
{{- define "arc-runners.nixConfigName" -}}
runner-nix-config-{{ .Values.nixConfig | sha256sum | trunc 12 }}
{{- end -}}