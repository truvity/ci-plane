{{- /*
Render an image reference from a {registry, repository, tag} block.
Empty registry means the repository's default registry.
*/ -}}
{{- define "ci-builders.image" -}}
{{- if .registry }}{{ .registry }}/{{ end }}{{ .repository }}:{{ .tag }}
{{- end -}}

{{- /*
The runner image: digest-pinned when the release stamped one, tag
otherwise (dev/lint renders).
*/ -}}
{{- define "ci-builders.runnerImage" -}}
{{- include "ci-builders.pinnedImage" .Values.runnerImage -}}
{{- end -}}

{{/*
The nix worker's own image (2026-09-24). It used to run the RUNNER
image, with Kubernetes choosing the role by overriding the command --
which worked, and meant neither half could be tuned for what it is: the
worker carried a docker client, buildx, devbox, goreleaser and two cache
clients it never used.

Falls back to runnerImage when nixWorkerImage names no repository, so a
consumer pinned to a chart version older than the split keeps rendering
exactly what it rendered before. Remove the fallback once no supported
values file sets only runnerImage.
*/ -}}
{{- define "ci-builders.nixWorkerImage" -}}
{{- $i := .Values.nixWorkerImage -}}
{{- if and $i $i.repository -}}
{{- include "ci-builders.pinnedImage" $i -}}
{{- else -}}
{{- include "ci-builders.runnerImage" . -}}
{{- end -}}
{{- end -}}

{{/*
One RELEASE image reference, digest-pinned when a release stamped one and
tagged otherwise. Distinct from `ci-builders.image` above, which is the
tag-only form the upstream components use and which has no digest to
stamp. Shared by the runner and the worker so the two cannot drift in HOW
they are referenced, only in what they point at.
*/ -}}
{{- define "ci-builders.pinnedImage" -}}
{{- if .digest -}}
{{ .registry }}/{{ .repository }}@{{ .digest }}
{{- else -}}
{{ .registry }}/{{ .repository }}:{{ .tag }}
{{- end -}}
{{- end -}}

{{- /*
The one architecture's worker's own SSH host-certificate principal name.
COMPUTED from the release namespace, never a raw value -- an estate names
its own hosts everywhere else, but a public repository must not invite
one into pasting a cluster- or estate-specific hostname into a values
file for the one thing that has to match the worker's actual DNS name
exactly. Matches the Service this chart already renders
(templates/nix-workers.yaml: `nix-worker-<arch>`).
*/ -}}
{{- define "ci-builders.nixWorkerHostPrincipal" -}}
{{- printf "nix-worker-%s.%s.svc.cluster.local" .arch .namespace -}}
{{- end -}}
