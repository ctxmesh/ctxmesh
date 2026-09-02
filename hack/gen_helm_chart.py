#!/usr/bin/env python3
"""Generate ctxmesh Helm chart templates from `kustomize build config/default`.

Reads the rendered kustomize output on stdin, splits it into individual resource
documents, buckets each by component, and writes three template files:

  templates/crds.yaml          — the CustomResourceDefinitions (always installed)
  templates/control-plane.yaml — namespace, RBAC, controller-manager, gateway,
                                 metrics service (the production control plane)
  templates/dev-data-plane.yaml— statelayer (Valkey) + objectstore (MinIO) +
                                 their dev creds/config — gated by
                                 `.Values.devDataPlane.enabled` (dev/trial ONLY,
                                 PRD §23; production brings its own data plane)
  templates/bff.yaml           — the Go BFF (UI server-side layer) Deployment +
                                 Service + its least-privilege SA/RBAC (ADR 0011:
                                 no agent-CRD access) — gated by
                                 `.Values.ui.enabled` (default true; the UI ships
                                 with the platform, spec §6)

THE NO-DRIFT INVARIANT: the resource *bodies* are copied verbatim from the
kustomize render — only two textual substitutions are applied, and only to make
the two knobs an operator needs into Helm values:

  1. the controller image `controller:latest`  -> {{ controllerManager image }}
  2. the install namespace `ctxmesh`-> {{ .Values.namespace }}

Both default (in values.yaml) to the exact kustomize strings, so with defaults
`helm template` == `kustomize build config/default`. `make helm-verify` proves it.
"""
from __future__ import annotations

import os
import re
import sys

NS_KUSTOMIZE = "ctxmesh"
IMG_KUSTOMIZE = "controller:latest"

# The BFF's connect-a-provider kill-switch (ADR 0015). config/bff hardcodes the
# env value "true" (so `kustomize build` and `make deploy` stay valid); the chart
# templates that ONE value from a Helm value so a hardened install can disable it
# with `--set bff.providerConnect.enabled=false`. values.yaml ships the default
# `true`, so with DEFAULT values the render is quoted "true" == the kustomize
# literal → no drift; an operator's `--set …=false` renders "false". We quote the
# value directly (no `| default true`) because Helm's `default` treats the boolean
# `false` as empty and would silently override a real `--set …=false` back to true.
PROVIDER_CONNECT_ENV_KUSTOMIZE = (
    '        - name: PROVIDER_CONNECT_ENABLED\n' '          value: "true"'
)
PROVIDER_CONNECT_ENV_HELM = (
    "        - name: PROVIDER_CONNECT_ENABLED\n"
    "          value: {{ .Values.bff.providerConnect.enabled | quote }}"
)

# The BFF's durable-run-store dispatch (ADR 0051, M55). config/bff has NO run-store
# env (the dev default is in-process/mem-store), so there's no kustomize placeholder
# to swap — instead INJECT a conditional block after the provider-connect env (a
# BFF-only anchor). With bff.runStore.enabled=false (default) the `{{- if }}` renders
# nothing, so the BFF env is byte-identical to kustomize (no-drift); enabled, the BFF
# gets RUN_STORE_DSN (from the operator Secret) + RUN_WORKER_DISPATCH=true so it
# dispatches runs to the run-worker (run-worker.yaml) instead of executing in-process.
RUN_STORE_ENV_INJECT = (
    PROVIDER_CONNECT_ENV_HELM + "\n"
    "{{- if .Values.bff.runStore.enabled }}\n"
    "        - name: RUN_STORE_DSN\n"
    "          valueFrom:\n"
    "            secretKeyRef:\n"
    "              name: {{ .Values.bff.runStore.dsnSecretName }}\n"
    "              key: dsn\n"
    "        - name: RUN_WORKER_DISPATCH\n"
    '          value: "true"\n'
    "{{- end }}"
)

# The BFF's create-from-prompt platform generation-model pin (ADR 0014). config/bff
# hardcodes the env value "" (unpinned — the default; so `kustomize build`/`make
# deploy` stay valid); the chart templates it from a Helm value so an operator can
# pin a governed generation-model list with `--set bff.generation.platformModels=…`.
# values.yaml ships the default "" so with DEFAULT values the render is the quoted
# empty string == the kustomize literal → no drift.
PLATFORM_GEN_MODELS_ENV_KUSTOMIZE = (
    '        - name: PLATFORM_GENERATION_MODELS\n' '          value: ""'
)
PLATFORM_GEN_MODELS_ENV_HELM = (
    "        - name: PLATFORM_GENERATION_MODELS\n"
    "          value: {{ .Values.bff.generation.platformModels | quote }}"
)

# The BFF's BYO-MCP kill-switch (ADR 0016). config/bff hardcodes "true" (default;
# so `kustomize build`/`make deploy` stay valid); the chart templates it from a
# Helm value so a hardened install can disable BYO MCP with
# `--set bff.mcp.enabled=false`. values.yaml ships `true` so with DEFAULT values
# the render is quoted "true" == kustomize → no drift. Quoted directly (no
# `| default true`) for the same reason as the connect switch (Helm's `default`
# treats boolean `false` as empty).
MCP_ENABLED_ENV_KUSTOMIZE = (
    '        - name: MCP_ENABLED\n' '          value: "true"'
)
MCP_ENABLED_ENV_HELM = (
    "        - name: MCP_ENABLED\n"
    "          value: {{ .Values.bff.mcp.enabled | quote }}"
)

# The BFF's BYO-MCP trust policy (ADR 0016). config/bff hardcodes "false"
# (self-serve — the default); the chart templates it from a Helm value so a
# hardened install can require operator approval with
# `--set bff.mcp.requireApproval=true`. values.yaml ships `false` so with DEFAULT
# values the render is quoted "false" == kustomize → no drift.
MCP_REQUIRE_APPROVAL_ENV_KUSTOMIZE = (
    '        - name: MCP_REQUIRE_APPROVAL\n' '          value: "false"'
)
MCP_REQUIRE_APPROVAL_ENV_HELM = (
    "        - name: MCP_REQUIRE_APPROVAL\n"
    "          value: {{ .Values.bff.mcp.requireApproval | quote }}"
)

# CONSOLE_URL (ADR 0040 cross-origin MCP consent): config/bff hardcodes "" (single-origin
# default, so `kustomize build`/`make deploy` stay valid). The chart templates it from
# `bff.consoleURL`; a multi-origin install sets the console origin. values.yaml ships ""
# so the DEFAULT render == kustomize (no drift).
CONSOLE_URL_ENV_KUSTOMIZE = (
    '        - name: CONSOLE_URL\n' '          value: ""'
)
CONSOLE_URL_ENV_HELM = (
    "        - name: CONSOLE_URL\n"
    '          value: {{ .Values.bff.consoleURL | default "" | quote }}'
)

# MCP_CREDENTIAL_NAMESPACE (m25.1b, ADR 0029 §7): config/bff hardcodes "" (unset — the
# legacy per-namespace grant path, so `kustomize build`/`make deploy` stay valid with
# no extra namespace/RBAC). The chart templates it from `bff.mcp.credentialNamespace`;
# setting it renders the dedicated namespace + RBAC (templates/mcp-credentials.yaml) and
# routes grants there. values.yaml ships "" so the DEFAULT render == kustomize (no drift).
# MCP_CREDENTIAL_NAMESPACE (M124/Gate A): config now hardcodes the install namespace so grant Secrets
# live in ONE locked namespace the token-service already has RBAC for (config/token-service/role.yaml) —
# a fresh install routes grants coherently instead of the legacy per-request-namespace path. The chart
# DERIVES the env from bff.mcp.credentialNamespace ELSE the install namespace, but the *value* stays ""
# (values.yaml) so templates/mcp-credentials.yaml's gate stays OFF — no extra Namespace/Role renders,
# no drift + no broad-RBAC regression. Default render (value "", ns=ctxmesh) == the literal.
MCP_CREDENTIAL_NAMESPACE_ENV_KUSTOMIZE = (
    "        - name: MCP_CREDENTIAL_NAMESPACE\n" "          value: ctxmesh"
)
MCP_CREDENTIAL_NAMESPACE_ENV_HELM = (
    "        - name: MCP_CREDENTIAL_NAMESPACE\n"
    "          value: {{ .Values.bff.mcp.credentialNamespace | default .Values.namespace }}"
)

# COST_ROLLUP_ENABLED (M124/Gate A, audit G11e): config/bff hardcodes "1" so the cost-rollup worker
# runs by default (→ cost_rollups → /api/cost + chargeback). Templated from bff.costRollupEnabled;
# default "1" renders == kustomize (no drift). BFF-only literal (do NOT add to the run-worker — same
# binary, two rollup writers). The gate is exactly "1" (cmd/bff/main.go), so it stays a quoted string.
# Model-service + async wiring (M148/m148.5, m52 M141-install). config/bff hardcodes the
# in-cluster gateway for MODEL_GATEWAY_URL (the chart SHIPS that gateway, so there is no
# reason for it to be unset) and "" for the rest, which name services the chart does not
# ship. Templated so an operator can enable RAG depth (M140) and discovery + async (M141)
# from values.yaml instead of patching a Deployment. Defaults render == kustomize (no drift).
MODEL_SERVICE_ENV_KUSTOMIZE = (
    "        - name: MODEL_GATEWAY_URL\n"
    "          value: http://ctxmesh-gateway.ctxmesh.svc:4000"
)
MODEL_SERVICE_ENV_HELM = (
    "        - name: MODEL_GATEWAY_URL\n"
    "          value: {{ .Values.bff.modelGatewayURL | "
    'default (printf "http://ctxmesh-gateway.%s.svc:4000" .Values.namespace) }}'
)

# The optional model-service / async vars: one values key each, all defaulting to "" so the
# default render is byte-identical to kustomize. Listed as (env name, values path).
OPTIONAL_MODEL_ENV = [
    ("INGEST_OCR_URL", "bff.ingestOcrURL"),
    ("KNOWLEDGE_RERANK_URL", "bff.knowledgeRerankURL"),
    ("DISCOVERY_EMBEDDING_ROUTE", "bff.discoveryEmbeddingRoute"),
    ("DISCOVERY_RERANK_URL", "bff.discoveryRerankURL"),
    ("ASYNC_BACKEND", "bff.asyncBackend"),
    ("NATS_URL", "bff.natsURL"),
    ("NATS_CREDENTIALS_FILE", "bff.natsCredentialsFile"),
]

COST_ROLLUP_ENABLED_ENV_KUSTOMIZE = (
    '        - name: COST_ROLLUP_ENABLED\n' '          value: "1"'
)
COST_ROLLUP_ENABLED_ENV_HELM = (
    "        - name: COST_ROLLUP_ENABLED\n"
    "          value: {{ .Values.bff.costRollupEnabled | quote }}"
)

# MCP_OBO_REQUIRED (M124/Gate A, ADR 0095 §2): config/bff hardcodes "false" (no-OBO install
# unaffected). Templated from controllerManager.oboEgress.enabled — when the operator turns ON OBO
# egress, the BFF/worker fail CLOSED at start-up if capability minting is disabled (else per-user OBO
# silently downgrades to the shared org/public credential). Default (enabled=false) renders "false"
# == kustomize (no drift). Stays a quoted string (envTrue parses it).
MCP_OBO_REQUIRED_ENV_KUSTOMIZE = (
    '        - name: MCP_OBO_REQUIRED\n' '          value: "false"'
)
MCP_OBO_REQUIRED_ENV_HELM = (
    "        - name: MCP_OBO_REQUIRED\n"
    "          value: {{ .Values.controllerManager.oboEgress.enabled | quote }}"
)

# TOKEN_SERVICE_TLS_REQUIRED (SEC-5): config/token-service hardcodes "false" (dev degrades
# to HTTP). The chart templates it from tokenService.tls.required; true ⇒ the token-service
# refuses to start without mTLS. values.yaml ships false so the DEFAULT render == kustomize.
TOKEN_SERVICE_TLS_REQUIRED_ENV_KUSTOMIZE = (
    '        - name: TOKEN_SERVICE_TLS_REQUIRED\n' '          value: "false"'
)
TOKEN_SERVICE_TLS_REQUIRED_ENV_HELM = (
    "        - name: TOKEN_SERVICE_TLS_REQUIRED\n"
    "          value: {{ .Values.tokenService.tls.required | quote }}"
)

# The console OIDC/SSO seam (m19.6, ADR 0020). config/bff hardcodes the OFF defaults
# (OIDC_ENABLED "false", empty issuer/client — so `kustomize build`/`make deploy` stay
# valid AND token login is the default); the chart templates them from the auth.oidc
# values so `--set auth.oidc.enabled=true` (with an issuer + client) lights up SSO.
# values.yaml ships enabled=false + the example issuer/client, but the BFF only
# ADVERTISES OIDC when OIDC_ENABLED=true, so the default render == kustomize → no drift.
# The BFF NEVER holds an OIDC secret — the console is a public PKCE client.
OIDC_ENABLED_ENV_KUSTOMIZE = (
    '        - name: OIDC_ENABLED\n' '          value: "false"'
)
OIDC_ENABLED_ENV_HELM = (
    "        - name: OIDC_ENABLED\n"
    "          value: {{ .Values.auth.oidc.enabled | quote }}"
)
OIDC_ISSUER_ENV_KUSTOMIZE = (
    '        - name: OIDC_ISSUER\n' '          value: ""'
)
OIDC_ISSUER_ENV_HELM = (
    "        - name: OIDC_ISSUER\n"
    "          value: {{ if .Values.auth.oidc.enabled }}{{ .Values.auth.oidc.issuer | quote }}{{ else }}\"\"{{ end }}"
)
OIDC_CLIENT_ID_ENV_KUSTOMIZE = (
    '        - name: OIDC_CLIENT_ID\n' '          value: ""'
)
OIDC_CLIENT_ID_ENV_HELM = (
    "        - name: OIDC_CLIENT_ID\n"
    "          value: {{ if .Values.auth.oidc.enabled }}{{ .Values.auth.oidc.client.id | quote }}{{ else }}\"\"{{ end }}"
)

# OPS-4a — the controller-injected sidecar images + the OBO egress-sidecar config (ADR 0030) ->
# Helm values. config/manager hardcodes each to its os.Getenv code default (empty image / off), so
# the DEFAULT render == kustomize (no drift); the chart templates them from controllerManager.* so a
# real install points the injected collector/discovery/egress images at its registry (they
# ImagePullBackOff off dev.local — OPS-1) and enables OBO egress. MCP_CREDENTIAL_NAMESPACE is NOT
# here: the manager shares the BFF's locked namespace via the existing bff.mcp.credentialNamespace
# replace below (one namespace across the BFF + the controller's OBO injection, by construction).
COLLECTOR_IMAGE_ENV_KUSTOMIZE = (
    '        - name: COLLECTOR_IMAGE\n' '          value: ""'
)
COLLECTOR_IMAGE_ENV_HELM = (
    "        - name: COLLECTOR_IMAGE\n"
    '          value: {{ .Values.controllerManager.injectedImages.collector | default "" | quote }}'
)
DISCOVERY_IMAGE_ENV_KUSTOMIZE = (
    '        - name: DISCOVERY_IMAGE\n' '          value: ""'
)
DISCOVERY_IMAGE_ENV_HELM = (
    "        - name: DISCOVERY_IMAGE\n"
    '          value: {{ .Values.controllerManager.injectedImages.discovery | default "" | quote }}'
)
MCP_OBO_EGRESS_ENABLED_ENV_KUSTOMIZE = (
    '        - name: MCP_OBO_EGRESS_ENABLED\n' '          value: "false"'
)
MCP_OBO_EGRESS_ENABLED_ENV_HELM = (
    "        - name: MCP_OBO_EGRESS_ENABLED\n"
    "          value: {{ .Values.controllerManager.oboEgress.enabled | quote }}"
)
EGRESS_SIDECAR_IMAGE_ENV_KUSTOMIZE = (
    '        - name: EGRESS_SIDECAR_IMAGE\n' '          value: ""'
)
EGRESS_SIDECAR_IMAGE_ENV_HELM = (
    "        - name: EGRESS_SIDECAR_IMAGE\n"
    '          value: {{ .Values.controllerManager.oboEgress.sidecarImage | default "" | quote }}'
)
# MCP_CAPABILITY_PUBLIC_KEY is NO LONGER templated (M124/Gate A): config/manager now reads it from the
# bff-capability Secret via valueFrom.secretKeyRef (the keygen hook provisions it). The chart copies that
# secretKeyRef block VERBATIM from kustomize — no value substitution, no drift, no committed key.
MCP_CAPABILITY_AUDIENCE_ENV_KUSTOMIZE = (
    '        - name: MCP_CAPABILITY_AUDIENCE\n' '          value: ""'
)
MCP_CAPABILITY_AUDIENCE_ENV_HELM = (
    "        - name: MCP_CAPABILITY_AUDIENCE\n"
    '          value: {{ .Values.controllerManager.oboEgress.capabilityAudience | default "" | quote }}'
)
# TOKEN_SERVICE_URL (M124/Gate A): config now hardcodes the in-cluster token-service Service DNS (the
# controller + BFF both delegate MCP grant writes / mint KB+OBO tokens against it — ADR 0029). The chart
# DERIVES it from the install namespace + tokenService.tls.required (https under `profile: production`,
# which SEC-5 requires). Default render (tls.required=false, ns=ctxmesh) reproduces the
# kustomize literal EXACTLY (unquoted) → no drift. Un-set env silently disabled KB retrieval (audit G13).
TOKEN_SERVICE_URL_ENV_KUSTOMIZE = (
    "        - name: TOKEN_SERVICE_URL\n"
    "          value: http://ctxmesh-token-service.ctxmesh.svc:8443"
)
TOKEN_SERVICE_URL_ENV_HELM = (
    "        - name: TOKEN_SERVICE_URL\n"
    '          value: {{ .Values.controllerManager.oboEgress.tokenServiceURL | default (printf "%s://ctxmesh-token-service.%s.svc:8443" (ternary "https" "http" .Values.tokenService.tls.required) .Values.namespace) }}'
)

# OPS-2 — the dev-data-plane gate on the manager. config/manager hardcodes "true" (== the kustomize
# dev posture, so the DEFAULT render matches, no drift); the chart templates it from
# devDataPlane.enabled so a `profile: production` render sets it "false" and the controller injects
# NO dev-only object-store / Langfuse feedback creds into agent pods (OPS-2). One source of truth
# with the bundled dev Valkey/MinIO, which are gated on the same value.
DEV_DATA_PLANE_ENV_KUSTOMIZE = (
    '        - name: DEV_DATA_PLANE\n' '          value: "true"'
)
DEV_DATA_PLANE_ENV_HELM = (
    "        - name: DEV_DATA_PLANE\n"
    "          value: {{ .Values.devDataPlane.enabled | quote }}"
)

# ENABLE_TENANT_LABEL_WEBHOOK (M134, ADR 0102) — DEFAULT-ON tenant-label fail-closed webhook. config/manager
# hardcodes "true" (== the kustomize default, so the DEFAULT render matches → no drift); the chart templates
# it from security.tenantLabelWebhook.enabled so an operator can opt OUT (with the values.yaml ack). ternary
# (NOT Sprig `default`) because `default` treats the boolean false as empty and would flip an explicit false
# back to true. The manager self-derives the allowed controller principal (SelfSubjectReview), so there is
# no companion TENANT_WEBHOOK_CONTROLLER_SA to template.
ENABLE_TENANT_LABEL_WEBHOOK_ENV_KUSTOMIZE = (
    '        - name: ENABLE_TENANT_LABEL_WEBHOOK\n' '          value: "true"'
)
ENABLE_TENANT_LABEL_WEBHOOK_ENV_HELM = (
    "        - name: ENABLE_TENANT_LABEL_WEBHOOK\n"
    '          value: {{ ternary "true" "false" .Values.security.tenantLabelWebhook.enabled | quote }}'
)

# Resources whose `control-plane:` label marks them as the bundled DEV data
# plane (in-cluster Valkey/MinIO). Production supplies its own — PRD §23 — so
# these are gated behind .Values.devDataPlane.enabled.
DEV_DATA_PLANE_LABELS = {"statelayer", "objectstore", "postgres"}

# Resources whose `control-plane:` label marks them as the Go BFF (the UI's
# server-side layer) + its least-privilege SA/RBAC. Gated behind
# .Values.ui.enabled (default true) so an operator can install the platform
# headless; with the default it renders, keeping no-drift with kustomize.
BFF_LABELS = {"bff"}

GEN_BANNER = (
    "# ============================================================================\n"
    "# GENERATED by hack/gen-helm-chart.sh from `kustomize build config/default`.\n"
    "# DO NOT EDIT BY HAND. Change config/ then run `make helm-generate`.\n"
    "# No-drift is enforced by `make helm-verify` (chart == kustomize).\n"
    "# ============================================================================\n"
)


def control_plane_label(doc: str) -> str | None:
    """Return the value of the top-level `control-plane:` metadata label, if any."""
    # The first `control-plane:` under metadata.labels identifies the component.
    m = re.search(r"^    control-plane:\s*(\S+)", doc, re.MULTILINE)
    return m.group(1) if m else None


def kind_of(doc: str) -> str:
    m = re.search(r"^kind:\s*(\S+)", doc, re.MULTILINE)
    return m.group(1) if m else ""


def name_of(doc: str) -> str:
    m = re.search(r"^  name:\s*(\S+)", doc, re.MULTILINE)
    return m.group(1) if m else ""


def substitute(doc: str) -> str:
    """Apply the two value substitutions (image, namespace) to a resource body."""
    # Controller image -> Helm value (only the exact kustomize placeholder).
    doc = doc.replace(
        f"image: {IMG_KUSTOMIZE}",
        'image: {{ printf "%s:%s" .Values.controllerManager.image.repository '
        "(.Values.controllerManager.image.tag | default .Chart.AppVersion) }}",
    )
    # Other component images -> Helm values (OPS-4b): bff / statelayer-proxy / token-service were
    # hardcoded `<name>:latest`, un-overridable for a real registry. Each literal appears exactly
    # once in the render (one Deployment each), so an exact-string replace is safe. Empty tag →
    # Chart.AppVersion ("latest") keeps the DEFAULT render == kustomize (no drift).
    for _repo, _val in (
        ("bff:latest", ".Values.bff.image"),
        ("statelayer-proxy:latest", ".Values.statelayerProxy.image"),
        ("token-service:latest", ".Values.tokenService.image"),
    ):
        doc = doc.replace(
            f"image: {_repo}",
            'image: {{ printf "%s:%s" ' + _val + ".repository (" + _val
            + ".tag | default .Chart.AppVersion) }}",
        )
    # Install namespace -> Helm value. Match only the standalone token so we do
    # not touch e.g. a substring inside another name.
    doc = re.sub(
        rf"(\bnamespace:\s*){re.escape(NS_KUSTOMIZE)}\b",
        r"\1{{ .Values.namespace }}",
        doc,
    )
    # State-layer proxy URL on the manager (M53, ADR 0050 §8 phase-3 cutover default):
    # the in-cluster proxy Service FQDN embeds the install namespace, which the generic
    # `namespace:` rule above can't reach (it lives inside an env VALUE string). Template
    # just the namespace segment so a non-default-namespace install still resolves.
    # ORDERING: this must run AFTER the `namespace:`-key re.sub above — that regex
    # anchors on a `namespace:` key so it never touches this env-value FQDN, but a future
    # value-matching namespace rule would need to run after this exact-string replace.
    # The FQDN literal appears exactly once (manager.yaml), so the replace can't over-match.
    doc = doc.replace(
        f"ctxmesh-statelayer-proxy.{NS_KUSTOMIZE}.svc",
        "ctxmesh-statelayer-proxy.{{ .Values.namespace }}.svc",
    )
    # State-layer Valkey backend address STATELAYER_ADDR (OPS-4c). The kustomize literal is the
    # in-cluster Valkey Service (fixed namespace); template it so (a) a non-default-namespace install
    # resolves it, and (b) a BYO-external Valkey (statelayer.externalAddr) repoints the fail-closed
    # proxy off the in-cluster Service — which does NOT exist in a production render without
    # devDataPlane/persistence. Default (externalAddr empty) renders the in-cluster addr in the
    # install namespace == kustomize on the default namespace (no drift). Appears once per backend
    # Deployment; the exact-string replace hits each.
    doc = doc.replace(
        f"ctxmesh-statelayer.{NS_KUSTOMIZE}.svc:6379",
        "{{ .Values.statelayer.externalAddr | "
        'default (printf "ctxmesh-statelayer.%s.svc:6379" .Values.namespace) }}',
    )
    # Control-plane / run-store DSN (M148, ADR 0130). Same treatment as
    # statelayer.externalAddr above and for the same reason: the kustomize literal
    # names the bundled in-cluster Postgres at a FIXED namespace, which (a) does not
    # resolve for a non-default-namespace install and (b) does not EXIST at all in a
    # production render, where devDataPlane.enabled=false. Templating it lets
    # `postgres.externalDsn` repoint the control plane at an operator-managed
    # database while the default renders byte-identical to kustomize (no drift).
    #
    # The literal appears twice (CONTROLPLANE_DSN and RUN_STORE_DSN in the same
    # Secret) and the replace hits both, which is intended — they are the same
    # database and splitting them would let an install point half of itself
    # somewhere else.
    doc = doc.replace(
        f"postgres://ctxmesh:ctxmesh-dev-secret@ctxmesh-postgres.{NS_KUSTOMIZE}.svc:5432/ctxmesh?sslmode=disable",
        "{{ .Values.postgres.externalDsn | "
        'default (printf "postgres://ctxmesh:ctxmesh-dev-secret@ctxmesh-postgres.%s.svc:5432/ctxmesh?sslmode=disable" .Values.namespace) }}',
    )
    # BFF connect-a-provider kill-switch -> Helm value (ADR 0015). Only the BFF
    # deployment carries this exact env block; the default keeps the render at
    # "true" == kustomize (no drift), while `--set …=false` disables it.
    doc = doc.replace(
        PROVIDER_CONNECT_ENV_KUSTOMIZE,
        PROVIDER_CONNECT_ENV_HELM,
    )
    # M148/m148.5 — the model-service + async block.
    doc = doc.replace(MODEL_SERVICE_ENV_KUSTOMIZE, MODEL_SERVICE_ENV_HELM)
    for env_name, val_path in OPTIONAL_MODEL_ENV:
        doc = doc.replace(
            f'        - name: {env_name}\n          value: ""',
            "        - name: %s\n          value: {{ .Values.%s | default \"\" | quote }}"
            % (env_name, val_path),
        )
    # BFF durable-run-store dispatch (ADR 0051): inject the run-store env AFTER the
    # provider-connect env (now in its Helm form) — a BFF-only anchor. Default OFF ⇒
    # renders nothing ⇒ no-drift; enabled ⇒ RUN_STORE_DSN + RUN_WORKER_DISPATCH.
    doc = doc.replace(PROVIDER_CONNECT_ENV_HELM, RUN_STORE_ENV_INJECT)
    # BFF create-from-prompt platform generation-model pin -> Helm value (ADR 0014).
    # Default renders the quoted empty string == kustomize (no drift); an operator's
    # `--set …=<list>` pins the generation models.
    doc = doc.replace(
        PLATFORM_GEN_MODELS_ENV_KUSTOMIZE,
        PLATFORM_GEN_MODELS_ENV_HELM,
    )
    # BFF BYO-MCP kill-switch -> Helm value (ADR 0016). Default renders "true" ==
    # kustomize (no drift); `--set bff.mcp.enabled=false` disables BYO MCP.
    doc = doc.replace(
        MCP_ENABLED_ENV_KUSTOMIZE,
        MCP_ENABLED_ENV_HELM,
    )
    # BFF BYO-MCP trust policy -> Helm value (ADR 0016). Default renders "false" ==
    # kustomize (no drift); `--set bff.mcp.requireApproval=true` marks new tools
    # pending-approval on a hardened install.
    doc = doc.replace(
        MCP_REQUIRE_APPROVAL_ENV_KUSTOMIZE,
        MCP_REQUIRE_APPROVAL_ENV_HELM,
    )
    # BFF cross-origin MCP-consent console origin -> Helm value (ADR 0040). Default
    # renders "" == kustomize (no drift); `--set bff.consoleURL=https://console.<domain>`
    # registers the canonical redirect_uri + cross-origin relay target.
    doc = doc.replace(
        CONSOLE_URL_ENV_KUSTOMIZE,
        CONSOLE_URL_ENV_HELM,
    )
    # BFF locked credential namespace -> Helm value (m25.1b, ADR 0029 §7). Default
    # renders "" == kustomize (no drift); `--set bff.mcp.credentialNamespace=...` routes
    # MCP grants to the dedicated namespace (templates/mcp-credentials.yaml).
    doc = doc.replace(
        MCP_CREDENTIAL_NAMESPACE_ENV_KUSTOMIZE,
        MCP_CREDENTIAL_NAMESPACE_ENV_HELM,
    )
    # token-service fail-closed TLS toggle -> Helm value (SEC-5). Default "false" renders ==
    # kustomize (no drift); tokenService.tls.required=true makes the credential plane refuse
    # plain HTTP, enforced under profile=production by ha-profile-guards.yaml.
    doc = doc.replace(
        TOKEN_SERVICE_TLS_REQUIRED_ENV_KUSTOMIZE,
        TOKEN_SERVICE_TLS_REQUIRED_ENV_HELM,
    )
    # BFF console OIDC/SSO seam -> Helm values (m19.6, ADR 0020). With auth.oidc
    # disabled (the default) all three render == the kustomize OFF literals (no drift);
    # `--set auth.oidc.enabled=true` advertises the issuer + public PKCE client to the SPA.
    doc = doc.replace(OIDC_ENABLED_ENV_KUSTOMIZE, OIDC_ENABLED_ENV_HELM)
    doc = doc.replace(OIDC_ISSUER_ENV_KUSTOMIZE, OIDC_ISSUER_ENV_HELM)
    doc = doc.replace(OIDC_CLIENT_ID_ENV_KUSTOMIZE, OIDC_CLIENT_ID_ENV_HELM)
    # OPS-4a — the controller's injected-image + OBO-egress env -> Helm values. All default
    # empty/false render == the config/manager literals (no drift); a real install points the
    # injected collector/discovery/egress-sidecar images at its registry + enables OBO egress
    # (ADR 0030). Manager-only literals, so these never collide with the BFF/token-service docs.
    doc = doc.replace(COLLECTOR_IMAGE_ENV_KUSTOMIZE, COLLECTOR_IMAGE_ENV_HELM)
    doc = doc.replace(DISCOVERY_IMAGE_ENV_KUSTOMIZE, DISCOVERY_IMAGE_ENV_HELM)
    doc = doc.replace(MCP_OBO_EGRESS_ENABLED_ENV_KUSTOMIZE, MCP_OBO_EGRESS_ENABLED_ENV_HELM)
    doc = doc.replace(EGRESS_SIDECAR_IMAGE_ENV_KUSTOMIZE, EGRESS_SIDECAR_IMAGE_ENV_HELM)
    # MCP_CAPABILITY_PUBLIC_KEY is now a secretKeyRef in config/manager, copied verbatim (no replace).
    doc = doc.replace(MCP_CAPABILITY_AUDIENCE_ENV_KUSTOMIZE, MCP_CAPABILITY_AUDIENCE_ENV_HELM)
    doc = doc.replace(TOKEN_SERVICE_URL_ENV_KUSTOMIZE, TOKEN_SERVICE_URL_ENV_HELM)
    doc = doc.replace(COST_ROLLUP_ENABLED_ENV_KUSTOMIZE, COST_ROLLUP_ENABLED_ENV_HELM)
    doc = doc.replace(MCP_OBO_REQUIRED_ENV_KUSTOMIZE, MCP_OBO_REQUIRED_ENV_HELM)
    # OPS-2 — the dev-data-plane gate -> Helm value. Default "true" renders == kustomize (no drift);
    # profile=production sets devDataPlane.enabled=false so the controller injects no dev creds.
    doc = doc.replace(DEV_DATA_PLANE_ENV_KUSTOMIZE, DEV_DATA_PLANE_ENV_HELM)
    doc = doc.replace(ENABLE_TENANT_LABEL_WEBHOOK_ENV_KUSTOMIZE, ENABLE_TENANT_LABEL_WEBHOOK_ENV_HELM)
    # The Namespace object's own name + RoleBinding/ClusterRoleBinding subject
    # namespaces use `name:`/`namespace:` -> also parameterize the Namespace name.
    return doc


def substitute_namespace_object(doc: str) -> str:
    """For the Namespace resource, its `name:` is the install namespace too."""
    return re.sub(
        rf"(^  name:\s*){re.escape(NS_KUSTOMIZE)}\b",
        r"\1{{ .Values.namespace }}",
        doc,
        flags=re.MULTILINE,
    )


def subject_namespace(doc: str) -> str:
    """RBAC subjects reference the SA namespace via `namespace:` in subjects[]."""
    return re.sub(
        rf"(\bnamespace:\s*){re.escape(NS_KUSTOMIZE)}\b",
        r"\1{{ .Values.namespace }}",
        doc,
    )


def main() -> None:
    out_dir = os.environ["OUT_DIR"]
    raw = sys.stdin.read()
    docs = [d for d in raw.split("\n---\n") if d.strip()]

    crds, control_plane, dev_data_plane, bff = [], [], [], []

    for doc in docs:
        doc = doc.strip("\n")
        kind = kind_of(doc)
        cp = control_plane_label(doc)

        # Apply substitutions to every doc uniformly (image + namespace fields).
        doc = substitute(doc)
        if kind == "Namespace":
            doc = substitute_namespace_object(doc)
        doc = subject_namespace(doc)

        if kind == "CustomResourceDefinition":
            crds.append(doc)
        elif cp in DEV_DATA_PLANE_LABELS:
            dev_data_plane.append(doc)
        elif cp in BFF_LABELS:
            bff.append(doc)
        else:
            control_plane.append(doc)

    # --- crds.yaml -----------------------------------------------------------
    with open(os.path.join(out_dir, "crds.yaml"), "w") as f:
        f.write(GEN_BANNER)
        f.write("{{- if .Values.crds.install }}\n")
        f.write("\n---\n".join(crds))
        f.write("\n{{- end }}\n")

    # --- control-plane.yaml --------------------------------------------------
    with open(os.path.join(out_dir, "control-plane.yaml"), "w") as f:
        f.write(GEN_BANNER)
        # Control-plane HA dials (ADR 0037 m34.5 + M50 hardening): template each control-plane
        # Deployment's replica count. The controller-manager runs --leader-elect, so >1 is a
        # hot-standby cluster (one leader reconciles). The gateway (config-rendered LiteLLM proxy)
        # and token-service (stateless credential-plane reader) are stateless request-servers, so
        # >1 is plain horizontal scale — pairing them with componentPodDisruptionBudgets makes those
        # PDBs USABLE (a minAvailable:1 PDB on a 1-replica Deployment would wedge node drains; M50
        # found the dials were missing). The BFF is intentionally NOT dialled here — its in-process
        # run path assumes a single pod until runWorkerDispatch (ADR 0034), so it stays replicas:1.
        # Default 1 renders `replicas: 1` == kustomize (helm-verify no-drift).
        replica_dials = {
            "controller-manager": "controllerManager.replicas",
            "gateway": "gateway.replicas",
            "token-service": "tokenService.replicas",
            # The state-layer proxy (M51, ADR 0050) is stateless (auth by token, state in
            # Valkey), so >1 is plain HA — REQUIRED before the phase-3 credential-removal
            # cutover so the budget-fail-closed path survives a proxy drain (ADR 0050 §5).
            "statelayer-proxy": "statelayerProxy.replicas",
        }
        cp_docs = []
        for doc in control_plane:
            if kind_of(doc) == "Deployment":
                for marker, valpath in replica_dials.items():
                    if f"control-plane: {marker}" in doc:
                        doc = doc.replace(
                            "  replicas: 1\n",
                            "  replicas: {{ .Values.%s | default 1 }}\n" % valpath,
                        )
                        break
            cp_docs.append(doc)
        f.write("\n---\n".join(cp_docs))
        f.write("\n")

    # --- dev-data-plane.yaml -------------------------------------------------
    # Gated: dev/trial ONLY (PRD §23). Production disables it and points the
    # controller at an externally-managed Valkey/MinIO (values seam).
    with open(os.path.join(out_dir, "dev-data-plane.yaml"), "w") as f:
        f.write(GEN_BANNER)
        f.write(
            "# The bundled in-cluster Valkey (statelayer) + MinIO (objectstore)"
            " are for\n# DEV / TRIAL ONLY (PRD §23). Deterministic dev creds,"
            " never real secrets.\n"
            "# Production sets devDataPlane.enabled=false and brings its own"
            " data plane.\n"
        )
        f.write("{{- if .Values.devDataPlane.enabled }}\n")
        f.write("\n---\n".join(dev_data_plane))
        f.write("\n{{- end }}\n")

    # --- bff.yaml ------------------------------------------------------------
    # The Go BFF (UI server-side layer) + its least-privilege SA/RBAC. Gated by
    # .Values.ui.enabled (default true), so the UI ships with the platform but an
    # operator can install headless. ADR 0011: the BFF SA holds NO agent-CRD
    # access — user ops run on the caller's token, K8s RBAC is the single authz.
    with open(os.path.join(out_dir, "bff.yaml"), "w") as f:
        f.write(GEN_BANNER)
        f.write(
            "# The Go BFF serves the static UI + /api behind M11 auth. Its SA is"
            " least-\n# privilege (ADR 0011): no agent-CRD access — every"
            " user-facing CRD op runs\n# on the CALLER'S token, so the K8s API"
            " server enforces the caller's RBAC.\n"
        )
        f.write("{{- if .Values.ui.enabled }}\n")
        # BFF replicas dial (ADR 0051, M55): now that runWorkerDispatch (m55.2) lets the
        # BFF offload runs to the durable worker, it can scale >1. Default 1 == kustomize
        # (no-drift). The consistency guard (templates/ha-guards.yaml) forbids replicas>1
        # without dispatch — in-process runs would split across pods (some lost).
        bff_docs = []
        for doc in bff:
            if kind_of(doc) == "Deployment" and "control-plane: bff" in doc:
                doc = doc.replace(
                    "  replicas: 1\n",
                    "  replicas: {{ .Values.bff.replicas | default 1 }}\n",
                )
            bff_docs.append(doc)
        f.write("\n---\n".join(bff_docs))
        f.write("\n{{- end }}\n")


if __name__ == "__main__":
    main()
