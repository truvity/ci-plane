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
