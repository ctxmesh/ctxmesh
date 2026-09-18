{{/*
ctxmesh.injectedImage — a controller-injected image reference, tagged.

An UNTAGGED repository resolves to :latest, and no ctxmesh image is published at :latest (the
release workflow pushes ${GITHUB_REF_NAME} only). So a bare `ghcr.io/ctxmesh/agent-discovery`
default would be an ImagePullBackOff dressed up as a default — which is the defect class this
chart just spent a milestone removing. Append the chart's appVersion unless the operator has
already pinned a tag or a digest, and pass an empty value through untouched so an explicit
"" still means "use the controller's compiled-in constant".
*/}}
{{- define "ctxmesh.injectedImage" -}}
{{- $ref := .ref | default "" -}}
{{- if eq $ref "" -}}
{{- "" -}}
{{- else if or (contains "@" $ref) (regexMatch "^[^/]+(/[^/]+)*:[^/:]+$" $ref) -}}
{{- $ref -}}
{{- else -}}
{{- printf "%s:%s" $ref .ctx.Chart.AppVersion -}}
{{- end -}}
{{- end -}}
