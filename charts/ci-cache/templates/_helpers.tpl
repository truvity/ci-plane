{{- /*
Render an image reference from a {registry, repository, tag} block.
Empty registry means the repository's default registry.
*/ -}}
{{- define "ci-cache.image" -}}
{{- if .registry }}{{ .registry }}/{{ end }}{{ .repository }}:{{ .tag }}
{{- end -}}

{{- /*
The runner image: digest-pinned when the release stamped one, tag
otherwise (dev/lint renders).
*/ -}}
{{- define "ci-cache.runnerImage" -}}
{{- $i := .Values.runnerImage -}}
{{- if $i.digest -}}
{{ $i.registry }}/{{ $i.repository }}@{{ $i.digest }}
{{- else -}}
{{ $i.registry }}/{{ $i.repository }}:{{ $i.tag }}
{{- end -}}
{{- end -}}
