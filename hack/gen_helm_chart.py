#!/usr/bin/env python3
"""Generate agent-engine Helm chart templates from `kustomize build config/default`.

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
  2. the install namespace `agent-engine-system`-> {{ .Values.namespace }}

Both default (in values.yaml) to the exact kustomize strings, so with defaults
`helm template` == `kustomize build config/default`. `make helm-verify` proves it.
"""
from __future__ import annotations

import os
import re
import sys

NS_KUSTOMIZE = "agent-engine-system"
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
MCP_CREDENTIAL_NAMESPACE_ENV_KUSTOMIZE = (
    '        - name: MCP_CREDENTIAL_NAMESPACE\n' '          value: ""'
)
MCP_CREDENTIAL_NAMESPACE_ENV_HELM = (
    "        - name: MCP_CREDENTIAL_NAMESPACE\n"
    "          value: {{ .Values.bff.mcp.credentialNamespace | quote }}"
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

# Resources whose `control-plane:` label marks them as the bundled DEV data
# plane (in-cluster Valkey/MinIO). Production supplies its own — PRD §23 — so
# these are gated behind .Values.devDataPlane.enabled.
DEV_DATA_PLANE_LABELS = {"statelayer", "objectstore"}

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
    # Install namespace -> Helm value. Match only the standalone token so we do
    # not touch e.g. a substring inside another name.
    doc = re.sub(
        rf"(\bnamespace:\s*){re.escape(NS_KUSTOMIZE)}\b",
        r"\1{{ .Values.namespace }}",
        doc,
    )
    # BFF connect-a-provider kill-switch -> Helm value (ADR 0015). Only the BFF
    # deployment carries this exact env block; the default keeps the render at
    # "true" == kustomize (no drift), while `--set …=false` disables it.
    doc = doc.replace(
        PROVIDER_CONNECT_ENV_KUSTOMIZE,
        PROVIDER_CONNECT_ENV_HELM,
    )
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
    # BFF console OIDC/SSO seam -> Helm values (m19.6, ADR 0020). With auth.oidc
    # disabled (the default) all three render == the kustomize OFF literals (no drift);
    # `--set auth.oidc.enabled=true` advertises the issuer + public PKCE client to the SPA.
    doc = doc.replace(OIDC_ENABLED_ENV_KUSTOMIZE, OIDC_ENABLED_ENV_HELM)
    doc = doc.replace(OIDC_ISSUER_ENV_KUSTOMIZE, OIDC_ISSUER_ENV_HELM)
    doc = doc.replace(OIDC_CLIENT_ID_ENV_KUSTOMIZE, OIDC_CLIENT_ID_ENV_HELM)
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
        f.write("\n---\n".join(bff))
        f.write("\n{{- end }}\n")


if __name__ == "__main__":
    main()
