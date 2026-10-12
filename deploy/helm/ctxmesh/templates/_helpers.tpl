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

{{/*
ctxmesh.thirdPartyImage — a third-party image reference from its thirdPartyImages entry.

Takes (list $ <entry>). thirdPartyImages.registryOverride, when set, replaces the entry's registry
(one value to move every third-party image to a private registry). A digest is appended after the
tag; the runtime pulls by digest and the tag is left for a reader.
*/}}
{{- define "ctxmesh.thirdPartyImage" -}}
{{- $root := index . 0 -}}
{{- $img := index . 1 -}}
{{- if not $img.repository -}}
{{- fail "every thirdPartyImages entry needs a repository" -}}
{{- end -}}
{{- if not (or $img.tag $img.digest) -}}
{{- fail (printf "thirdPartyImages entry %s needs a tag or a digest" $img.repository) -}}
{{- end -}}
{{- if and $img.digest (not (regexMatch "^sha256:[a-f0-9]{64}$" $img.digest)) -}}
{{- fail (printf "thirdPartyImages entry %s has digest %q; a digest is sha256: followed by 64 hex characters" $img.repository $img.digest) -}}
{{- end -}}
{{- $registry := $root.Values.thirdPartyImages.registryOverride | default $img.registry | default "" | trimSuffix "/" -}}
{{- $ref := $img.repository -}}
{{- if $registry -}}{{- $ref = printf "%s/%s" $registry $img.repository -}}{{- end -}}
{{- if $img.tag -}}{{- $ref = printf "%s:%s" $ref $img.tag -}}{{- end -}}
{{- if $img.digest -}}{{- $ref = printf "%s@%s" $ref $img.digest -}}{{- end -}}
{{- $ref -}}
{{- end -}}

{{/*
ctxmesh.imagePullSecrets — the pod-spec imagePullSecrets block, or nothing.

Takes (list $ <indent>) and is written on the same line as the pod spec's `spec:` key, so with no
secrets configured the template renders exactly as it would without it. Entries may be Secret names
or {name: <secret>} maps.
*/}}
{{- define "ctxmesh.imagePullSecrets" -}}
{{- $root := index . 0 -}}
{{- $indent := index . 1 -}}
{{- $secrets := list -}}
{{- range (($root.Values.global | default dict).imagePullSecrets | default list) -}}
{{- if kindIs "string" . -}}
{{- $secrets = append $secrets (dict "name" .) -}}
{{- else if and (kindIs "map" .) .name -}}
{{- $secrets = append $secrets (dict "name" .name) -}}
{{- else -}}
{{- fail "global.imagePullSecrets entries are Secret names or {name: <secret>}" -}}
{{- end -}}
{{- end -}}
{{- if $secrets -}}
{{- printf "\n%s" (toYaml (dict "imagePullSecrets" $secrets) | trimSuffix "\n" | indent $indent) -}}
{{- end -}}
{{- end -}}
