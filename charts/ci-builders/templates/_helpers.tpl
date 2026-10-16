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

v5.0.0 removed the fallback to runnerImage: the runner image no longer
carries sshd's config, the worker scripts or any machine login, so a
worker on it could never start. A values file that empties
nixWorkerImage.repository is refused instead.
*/ -}}
{{- define "ci-builders.nixWorkerImage" -}}
{{- $i := .Values.nixWorkerImage -}}
{{- if not (and $i $i.repository) -}}
{{- fail "nixWorkerImage.repository is required when nixWorkers is enabled: since v5.0.0 the workers no longer fall back to runnerImage, which carries no worker role" -}}
{{- end -}}
{{- include "ci-builders.pinnedImage" $i -}}
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

{{- /*
A node class by name (values.nodeClasses): the placement the caller
defines for a name a preset or a values file refers to, as JSON
({nodeSelector, tolerations}, each optional). An empty name is the empty
class. An undefined name is a render error -- a typo must not quietly
schedule a builder anywhere; a class that places nothing is defined empty
(`nodeClasses: {name: {}}`).

Takes a dict `{root, name}`.
*/ -}}
{{- define "ci-builders.nodeClass" -}}
{{- $root := .root -}}
{{- $name := .name -}}
{{- if $name -}}
{{- $classes := $root.Values.nodeClasses | default (dict) -}}
{{- if not (hasKey $classes $name) }}{{ fail (printf "nodeClass %q is not defined: add it under nodeClasses (defined: [%s]); a class that places nothing is `nodeClasses: {%s: {}}`" $name (keys $classes | sortAlpha | join ", ") $name) }}{{ end -}}
{{- get $classes $name | default (dict) | toJson -}}
{{- else -}}
{}
{{- end -}}
{{- end -}}

{{- /*
Where a pod goes, as JSON {nodeSelector, tolerations}.

`sched` is a per-architecture block (buildkitd.scheduling.<arch>,
nixWorkers.scheduling.<arch>) or empty; `arch` is the architecture for
those two, empty for a component with none.

  - sched.nodeSelector / sched.tolerations, when set, are used as given
    (they REPLACE; the arch selector is then the caller's to write).
  - else sched.nodeClass, when set: the class's placement, plus the
    kubernetes.io/arch selector for `arch` (the class's own selector wins
    on a clash).
  - else the release-wide nodeSelector / tolerations, when not empty
    (plus the arch selector), else the release-wide nodeClass's.
*/ -}}
{{- define "ci-builders.placement" -}}
{{- $root := .root -}}
{{- $arch := .arch | default "" -}}
{{- $sched := .sched | default (dict) -}}
{{- $class := dict -}}
{{- if $sched.nodeClass }}{{ $class = include "ci-builders.nodeClass" (dict "root" $root "name" $sched.nodeClass) | fromJson }}{{ end -}}
{{- $global := include "ci-builders.nodeClass" (dict "root" $root "name" $root.Values.nodeClass) | fromJson -}}
{{- $nodeSelector := dict -}}
{{- $tolerations := list -}}
{{- if $sched.nodeSelector -}}
{{- $nodeSelector = $sched.nodeSelector -}}
{{- else -}}
{{- if $arch }}{{ $_ := set $nodeSelector "kubernetes.io/arch" $arch }}{{ end -}}
{{- if $sched.nodeClass -}}
{{- $nodeSelector = mergeOverwrite $nodeSelector ($class.nodeSelector | default (dict)) -}}
{{- else -}}
{{- $nodeSelector = mergeOverwrite $nodeSelector ($root.Values.nodeSelector | default $global.nodeSelector | default (dict)) -}}
{{- end -}}
{{- end -}}
{{- if $sched.tolerations -}}
{{- $tolerations = $sched.tolerations -}}
{{- else if $sched.nodeClass -}}
{{- $tolerations = $class.tolerations | default (list) -}}
{{- else -}}
{{- $tolerations = $root.Values.tolerations | default $global.tolerations | default (list) -}}
{{- end -}}
{{- dict "nodeSelector" $nodeSelector "tolerations" $tolerations | toJson -}}
{{- end -}}

{{- /*
A size by name (values.sizes), as JSON: what a component's `size` key
names. Applied by buildkitd.yaml and nix-workers.yaml over a copy of the
component's values: a size REPLACES the component's own value for each key
it sets (the chart's defaults for those keys cannot be told from yours, so
a merge would leave stray defaults behind, e.g. a request). An undefined
name is a render error.

Takes a dict `{root, size, name}`; `name` is the component, for the message.
*/ -}}
{{- define "ci-builders.sizeOf" -}}
{{- $root := .root -}}
{{- $sizes := $root.Values.sizes | default (dict) -}}
{{- if not (hasKey $sizes .size) }}{{ fail (printf "%s.size %q is not defined: add it under sizes (defined: [%s])" .name .size (keys $sizes | sortAlpha | join ", ")) }}{{ end -}}
{{- get $sizes .size | default (dict) | toJson -}}
{{- end -}}
