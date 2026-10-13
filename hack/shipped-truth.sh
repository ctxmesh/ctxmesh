#!/usr/bin/env bash
# shipped-truth — claims about the shipped source that a string or a test name can hold, one table
# row each, so a rename, a deleted test or a reverted fix fails CI instead of passing unnoticed.
#
# To add a row, append one `row` call to the table below:
#
#   row <ref> <mode> <file> <needle> "<claim>" [n]
#
#   ref     the milestone or card the claim comes from (m125, m52.Q5)
#   file    a path from the repo root; `site:<path>` is relative to the docs-site checkout
#           (CTXMESH_SITE, default ../ctxmesh.github.io). The tree modes take a comma-separated
#           pathspec list instead (`*.go,*.yaml`, `internal/controller`).
#   needle  the text, regex or test name; `-` for the modes that take none (exists, absent, syntax)
#   claim   what must be true, in a short phrase — printed beside the ref when the row fails
#
#   test           the file declares `func NAME(` (Go) or `def NAME(` (Python)
#   has            the fixed string is in the file
#   has-re         the extended regex matches in the file
#   lacks          the fixed string is NOT in the file (the file must exist)
#   exists         the file exists
#   absent         nothing exists at the path
#   syntax         the file parses under `bash -n`
#   count-ge       at least n lines of the file contain the fixed string      (a threshold)
#   count-eq       as many lines contain the fixed string as contain the string in the n slot
#   has-tree       some tracked file under the pathspecs contains the fixed string
#   lacks-tree     no tracked file under the pathspecs contains the fixed string
#   lacks-tree-re  no tracked file under the pathspecs matches the extended regex
#   lacks-name     no tracked file's NAME matches the pathspec, case-insensitively
#   files-ge       at least n tracked files under the pathspecs contain the fixed string
#
# count-eq and the tree modes exist because some claims are not about one file: a persisted identifier
# must survive somewhere in the tree, a retired name must survive nowhere, and a threshold is a count.
# Tree modes search tracked text files only, so untracked output and binaries cannot satisfy or break a row.
#
# It fails closed: an empty table, a table shorter than FLOOR, a missing file, a pathspec that
# selects nothing, an empty needle, a malformed regex, and an absent docs site all fail. The site
# rows are skipped, loudly, only under SHIPPED_TRUTH_ALLOW_PARTIAL=1.
set -euo pipefail
cd "$(dirname "$0")/.."

# The number of rows the table holds. Raise it with the rows; lowering it is a visible edit to a claim.
FLOOR=173

SITE="${CTXMESH_SITE:-../ctxmesh.github.io}"
SITE_OK=0
[ -d "$SITE/src/content/docs" ] && SITE_OK=1

total=0
failed=0
skipped=0

nope() { printf 'FAIL %s: %s\n     %s\n' "$ref" "$claim" "$1" >&2; failed=$((failed + 1)); }

# A bash-3.2-safe split of a comma-separated pathspec list into $specs.
specs=()
split_specs() {
  specs=()
  local IFS=,
  # shellcheck disable=SC2206
  specs=($1)
}

# selects [pathspec...] — true when git tracks at least one file under them. `head`, never
# `grep -q`: an early-exiting reader SIGPIPEs git and pipefail then reports a match as a failure.
selects() { [ -n "$(git ls-files -- "${@:-.}" | head -n 1)" ]; }

# match <F|E> <needle> <grep args...>  ->  0 match, 1 none, 2 error
match() {
  local kind="$1" needle="$2"
  shift 2
  if [ "$kind" = F ]; then grep -qF -e "$needle" "$@"; else grep -qE -e "$needle" "$@"; fi
}

row() {
  ref="$1"; local mode="$2" file="$3" needle="$4"
  claim="$5"; local n="${6:-}"
  total=$((total + 1))

  [ -n "$file" ] || { nope "no file or pathspec given"; return; }
  case "$mode" in
    exists|absent|syntax) ;;   # no needle; write `-`
    *) [ -n "$needle" ] || { nope "empty needle — a row that matches everything proves nothing"; return; } ;;
  esac

  local root="." rel="$file" site=0
  case "$file" in site:*) site=1; root="$SITE"; rel="${file#site:}" ;; esac
  if [ "$site" = 1 ] && [ "$SITE_OK" = 0 ]; then
    if [ -n "${SHIPPED_TRUTH_ALLOW_PARTIAL:-}" ]; then skipped=$((skipped + 1)); return; fi
    nope "the docs site is not checked out at $SITE (set CTXMESH_SITE; SHIPPED_TRUTH_ALLOW_PARTIAL=1 skips site rows loudly)"
    return
  fi
  local path="$root/$rel" rc=0

  case "$mode" in
    test|has|has-re|lacks|count-ge|count-eq|syntax)
      [ -f "$path" ] || { nope "file missing: $file"; return; } ;;
  esac

  case "$mode" in
    test)
      local decl
      case "$rel" in *.py) decl="def $needle(" ;; *) decl="func $needle(" ;; esac
      match F "$decl" "$path" || nope "$file declares no \`$decl\`" ;;
    has)
      match F "$needle" "$path" || nope "$file does not contain: $needle" ;;
    has-re)
      match E "$needle" "$path" || rc=$?
      case "$rc" in 0) ;; 1) nope "$file does not match: $needle" ;; *) nope "malformed regex: $needle" ;; esac ;;
    lacks)
      if match F "$needle" "$path"; then nope "$file still contains: $needle"; fi ;;
    exists)
      [ -f "$path" ] || nope "missing: $file" ;;
    absent)
      [ ! -e "$path" ] || nope "present, and must not be: $file" ;;
    syntax)
      bash -n "$path" 2>/dev/null || nope "$file does not parse under bash -n" ;;
    count-ge)
      case "$n" in ''|*[!0-9]*) nope "count-ge needs a numeric n"; return ;; esac
      local c
      c="$(grep -cF -e "$needle" "$path" || true)"
      [ "${c:-0}" -ge "$n" ] || nope "$file holds ${c:-0} line(s) with '$needle', want at least $n" ;;
    count-eq)
      [ -n "$n" ] || { nope "count-eq needs a second string to count against"; return; }
      local a b
      a="$(grep -cF -e "$needle" "$path" || true)"
      b="$(grep -cF -e "$n" "$path" || true)"
      [ "${b:-0}" -gt 0 ] || { nope "$file holds no line with '$n' — nothing to compare against"; return; }
      [ "${a:-0}" = "${b:-0}" ] || nope "$file: ${a:-0} line(s) with '$needle' but ${b:-0} with '$n'" ;;
    has-tree|lacks-tree|lacks-tree-re|files-ge)
      tree "$mode" "$site" "$root" "$rel" "$needle" "$n" ;;
    lacks-name)
      split_specs "$rel"
      selects || { nope "git lists no tracked files — the name check ran against nothing"; return; }
      if selects "${specs[@]}"; then nope "a tracked file name matches: $rel"; fi ;;
    *)
      nope "unknown mode: $mode" ;;
  esac
}

# tree <mode> <site> <root> <pathspecs> <needle> <n> — the search half of the tree modes.
tree() {
  local mode="$1" site="$2" root="$3" rel="$4" needle="$5" n="$6" hits=0 rc=0 kind=F
  [ "$mode" = lacks-tree-re ] && kind=E
  local flag="-qF"
  [ "$kind" = E ] && flag="-qE"

  if [ "$site" = 1 ]; then
    [ -d "$root/$rel" ] || { nope "no such directory under the docs site: $rel"; return; }
    [ -n "$(find "$root/$rel" -type f | head -1)" ] || { nope "no files under the docs site path $rel"; return; }
    case "$mode" in
      files-ge) hits="$(grep -rlF -e "$needle" "$root/$rel" | wc -l | tr -d ' ')" ;;
      *) grep -r $flag -e "$needle" "$root/$rel" || rc=$? ;;
    esac
  else
    split_specs "$rel"
    selects "${specs[@]}" || { nope "the pathspec selects no tracked file: $rel — the row would be vacuous"; return; }
    case "$mode" in
      files-ge) hits="$(git grep -lIF -e "$needle" -- "${specs[@]}" | wc -l | tr -d ' ')" ;;
      *) git grep -I $flag -e "$needle" -- "${specs[@]}" || rc=$? ;;
    esac
  fi

  case "$mode" in
    has-tree)
      case "$rc" in 0) ;; 1) nope "no file under $rel contains: $needle" ;; *) nope "search failed (exit $rc) for: $needle" ;; esac ;;
    lacks-tree|lacks-tree-re)
      case "$rc" in 1) ;; 0) nope "a file under $rel still contains: $needle" ;; *) nope "search failed (exit $rc) for: $needle" ;; esac ;;
    files-ge)
      case "$n" in ''|*[!0-9]*) nope "files-ge needs a numeric n"; return ;; esac
      [ "$hits" -ge "$n" ] || nope "$hits file(s) under $rel contain '$needle', want at least $n" ;;
  esac
}


# ── the table ─────────────────────────────────────────────────────────────────

# Named-test pins. `go test` passes when a test is deleted or renamed, so a claim resting on one test
# needs its name held here.
row m87 test internal/controller/agentdeployment_controller_test.go TestReconcile_IdentitySAConflict_ReadyFalse "a foreign-owned identity SA yields Ready=False IdentitySAConflict (C11)"
row m87 test internal/controller/podtoken_hardening_test.go TestMountServiceAccountToken_HardenedByDefault "identity SAs set automountServiceAccountToken=false by default (C10)"
row m89 has internal/controller/tenant_controller_test.go "func TestTenant_NetworkIsolation" "networkIsolation secure-by-default has envtest coverage (C4, ADR 0073)"
row m89 syntax hack/migrate-network-isolation.sh - "the networkIsolation backfill migration parses (m89.4)"

# Gate B — the durable worker is failure-safe (ADR 0097).
row m125 test internal/bff/run_worker_test.go TestStartRunWorkers_WaitJoinsOnDrain "F-1: a rolling-deploy drain joins every worker (WaitGroup)"
row m125 test internal/bff/run_worker_test.go TestStartHeartbeat_TransientKeepsRenewing "F-2: a transient heartbeat error keeps renewing the lease"
row m125 test internal/bff/workflow_executor_test.go TestWorkflowFence_PeerReclaimedRunNotClobbered "F-3: a fenced workflow executor never clobbers a peer-reclaimed run"
row m125 test internal/bff/run_worker_test.go TestReclaimUnderBacklog "F-4: lease reclaim still runs under a queued backlog"
row m125 test internal/bff/run_worker_test.go TestPoisonRun_DeadLetteredNotReReclaimed "F-5: a poison run is dead-lettered, not reclaimed forever"
row m125 test internal/dbpool/dbpool_test.go TestApply_BoundsThePool "F-8: the DB pool limits are applied"

# Gate C — the interposed runtime (ADRs 0098/0099).
row m126 test cmd/launcher/gateway_stream_test.go TestServeStreamingVerbatim_NonGuardrailedStreamsAndBooks "F1: every interposed class streams and books real spend"
row m126 test cmd/launcher/gateway_stream_test.go TestServeStreamingVerbatim_StallEmitsErrorFrame "F5: a stalled stream emits an error frame"
row m126 test internal/egress/proxy_test.go TestEgressToolTimeout_TypedNotHang "F3: a hung tool times out with a typed 504"
row m126 test internal/bff/run_worker_test.go TestRunExecTimeout_Configurable "F4: the run deadline is configurable, not a hard 90s"
row m126 test internal/bff/run_worker_test.go TestStartHeartbeat_SelfFencesAfterLeaseOutage "F4: a worker self-fences after a lease outage"
row m126 test internal/statelayer/quota_test.go TestQuotaAgentAndConvSpend_ReadBack "F2: per-agent and per-conversation spend endpoints read back"
row m126 test internal/gateway/budget/budget_test.go TestEnforcer_BackendEnforcesAndFailsClosed "F2: budget enforces across replicas and fails closed"

# Gate E — ops maturity and platform PKI (ADR 0102).
row m128 has internal/bff/metrics.go ctxmesh_run_duration_seconds "A1: the run-duration SLI is exported"
row m128 has internal/bff/metrics.go ctxmesh_run_outcomes_total "A1: the run-outcome SLI is exported"
row m128 has internal/bff/metrics.go ctxmesh_run_worker_active "A1: the active-worker SLI is exported"
row m128 has internal/bff/metrics.go ctxmesh_run_queue_depth "A1: the queue-depth SLI is exported"
row m128 has deploy/helm/ctxmesh/templates/prometheus-runpipeline.yaml AgentEngineRunWorkerPoolDead "A1: the dead-worker-pool PrometheusRule ships"
row m128 count-eq deploy/helm/ctxmesh/templates/crds.yaml "helm.sh/resource-policy: keep" "A2: every CRD carries resource-policy: keep, so helm uninstall cannot cascade-delete CRs" "kind: CustomResourceDefinition"
row m128 test internal/pki/certrotator_test.go TestRotatorFor_MapsConfig "B: the in-process cert-controller is wired from config"
row m128 test internal/pki/certrotator_test.go TestDNSName_ShortServiceForm_NoClusterLocal "B: the serving-cert SAN uses the short service form"
row m128 test internal/webhook/tenant_label_test.go TestTenantLabelValidator "B: the tenant-label handler denies set, change and remove"
row m128 test internal/webhook/tenant_vwc_test.go TestBuildTenantLabelVWC_Guards "B: the manager-owned VWC carries its guards"
row m128 test internal/credplane/mtls_test.go TestE1ServerAuth "B: E-1 server-authenticated egress TLS round-trips"
row m128 has deploy/helm/ctxmesh/templates/ha-profile-guards.yaml "in-process cert-controller (no cert-manager)" "C: the Gate-A guard message says Gate E delivered the cert infra"

# Gate F — the model-driven supervisor finishes with a composed answer (ADR 0103).
row m129 has sdk/python/src/ctxmesh/managed.py budget_exhausted_composed "F2: the managed loop's forced-composition finish_reason exists"
row m129 test sdk/python/tests/test_managed.py test_managed_loop_max_steps_forces_composition "F2: the managed loop forces a composition at max_steps instead of raising"

# Governance and quality (ADRs 0111-0113, 0152).
row m139 test internal/bff/guardrail_event_handler_test.go TestGuardrailEvent_ValidCapabilityWritesAuditRow "G11c: guardrail.block writes an audit row in the caller's namespace"
row m139 test internal/bff/tool_policy_approvers_test.go TestApproverMatches "ADR 0111: the approver matcher"
row m139 test internal/bff/tool_policy_approvers_test.go TestResumeApproval_NonApproverRefused "ADR 0152: a non-approver cannot approve"
row m139 test internal/bff/tool_policy_approvers_test.go TestResumeApproval_GroupApproverAllowed "ADR 0152: a Group approver can approve"
row m139 test internal/bff/tool_policy_approvers_test.go TestResumeApproval_EmptyApproversIsRBACOnly "ADR 0152: empty approvers means RBAC only"
row m139 test internal/controller/tool_policy_resolve_test.go TestResolveToolPolicy_InlineApproval "ADR 0152: an inline require-approval rule resolves"
row m139 test internal/controller/tool_policy_resolve_test.go TestResolveToolPolicy_InlineAllowOverrideWinsOverDefault "ADR 0152: one author — an inline allow beats the default"
row m139 test internal/controller/tool_policy_controller_test.go TestToolPolicy_InlineApproval_DeliveredToSidecar "ADR 0152: require-approval reaches the sidecar (envtest)"
row m139 test internal/controller/tool_policy_controller_test.go TestToolPolicy_ApproverShapeValidatedAtAdmission "ADR 0152: the approver shape is validated at admission (envtest)"
row m139 test internal/controller/canary_autoprogress_test.go TestDecideAutoProgress "ADR 0113: the canary auto-progression state machine"
row m139 test internal/controller/canary_autoprogress_test.go TestNextCanaryStep "ADR 0113: canary next-step selection"
row m139 test internal/controller/canary_controller_test.go TestCanary_AutoProgress "ADR 0113: canary auto-progress (envtest)"
row m139 has internal/controller/canary_controller_test.go "func TestCanary_Split" "ADR 0113: the canary traffic split has envtest coverage"
row m139 test internal/controller/agentdeployment_feedback_test.go TestAgentDeployment_FeedbackValidatedAtAdmission "ADR 0152: a malformed feedback declaration is refused at admission"
row m139 test internal/controller/agentdeployment_feedback_test.go TestAgentDeployment_FeedbackValidatedInEveryServedVersion "ADR 0152: the feedback rules hold in every served version"
row m139 test internal/bff/feedback_spec_test.go TestSubmitFeedback_DeclaredName_Relayed "ADR 0112: a declared feedback name is relayed"
row m139 test internal/bff/feedback_spec_test.go TestSubmitFeedback_UndeclaredName_EnforceRejected "ADR 0112: an undeclared feedback name is rejected in Enforce"
row m139 test internal/bff/feedback_spec_test.go TestFeedbackRead_AttributesSources "ADR 0112: feedback reads attribute their sources"
row m139 test internal/guardrail/streamability_test.go TestAnalyzeOutputStreamability "K10: one shared streamability analyzer (ADR 0086)"
row m139 test cmd/launcher/streamability_drift_test.go TestStreamabilityNoDrift "K10: enforce and report agree on streamability"
row m139 test internal/controller/guardrailpolicy_streaming_test.go TestGuardrailStreamingStatus "K10: status.streaming is reported"

# The rename to ctxmesh carried nothing it should not have (ADR 0127). The retired name is spelled in
# two halves so that this file does not match the very search it runs.
row m147 lacks-tree-re . "agent""ry|Agent""ry|AGENT""RY" "the retired product name survives in no tracked file"
row m147 lacks-name ":(icase)*agent""ry*" - "the retired product name survives in no tracked file name"
row m147 has-tree "*.go,*.yaml" "agents.ctxmesh.ai" "the CRD API group did not move (stored objects would orphan)"
row m147 has-tree "*.go,*.yaml" "7ab0b236.ctxmesh.ai" "the leader-election lease id did not move"
row m147 has-tree "*.go" "agents.ctxmesh.ai/finalizer" "the finalizer suffix did not move (existing objects would never release)"
row m147 lacks-tree "*.go,*.yaml" "ctxmesh.ctxmesh.ai" "a blind replace did not mangle a domain-qualified identifier"
row m147 has-re config/default/kustomization.yaml '^namePrefix:[[:space:]]+ctxmesh-[[:space:]]*$' "namePrefix is ctxmesh-, in lockstep with the PKI rotator constants"
row m147 has cmd/main.go '"ctxmesh-webhook-server-cert"' "the PKI rotator's cert Secret name matches namePrefix"
row m147 has cmd/main.go '"ctxmesh-webhook-service"' "the PKI rotator's webhook Service name matches namePrefix"
row m147 has cmd/main.go '"ctxmesh-ca"' "the PKI rotator's CA name matches namePrefix"
row m147 exists .gitleaks.toml - "the history scan is repeatable: its config ships"
row m147 exists LICENSE - "the licence the source headers assert is shipped"
row m147 has LICENSE "Apache License" "LICENSE is Apache-2.0, which the headers assert"
row m147 exists NOTICE - "a public repo ships a NOTICE"
row m147 exists SECURITY.md - "a public repo ships a security policy"
row m147 exists CONTRIBUTING.md - "a public repo ships contributing guidance"
row m147 exists CODEOWNERS - "a public repo ships CODEOWNERS"
row m147 has-re sdk/typescript/package.json '^  "license": *"Apache-2\.0"' "the TypeScript SDK declares Apache-2.0"
row m147 has sdk/python/pyproject.toml "Apache-2.0" "the Python SDK declares Apache-2.0"
row m147 absent CLAUDE.md - "no brain artifact in the public repo (golden rule)"
row m147 absent AGENTS.md - "no tool-generated AI guide in the public repo (golden rule)"
row m147 absent .claude - "no .claude directory in the public repo (golden rule)"

# Skills (ADR 0137): a version is content-addressed, an alias is pinned at deploy time, nothing
# floats implicitly.
row m161 test internal/controlplane/skill/conformance_test.go TestStoreConformance "versions are immutable and content-addressed on both source paths"
row m161 test internal/controlplane/skill/validate_test.go TestDigestIsContentIdentity "same bytes are the same version"
row m161 test internal/controlplane/skill/validate_test.go TestABranchIsNotAVersion "a branch ref is refused as a version"
row m161 test internal/controlplane/skill/ref_test.go TestResolutionPinsAnAliasToADigest "an alias resolves once and the digest is what is recorded"
row m161 test internal/controlplane/skill/ref_test.go TestABareNameIsRefused "a bare skill name is refused (no implicit floating ref)"
row m161 test internal/controlplane/skill/ref_test.go TestResolutionIsAllOrNothing "a partial resolution cannot escape"
row m161 test internal/controller/skill_revision_test.go TestAnAgentWithoutSkillsKeepsItsRevisionName "an agent without skills keeps a byte-identical revision name"
row m161 test internal/controller/skill_revision_test.go TestAnAliasMoveRollsARevision "a resolved-digest change rolls a revision"
row m161 test internal/controller/skill_revision_test.go TestSkillOrderIsPartOfTheIdentity "skill order is part of the revision identity"
row m161 test internal/controller/skill_gate_test.go TestEditingSkillRefsReachesTheDeployGate "a skill edit reaches the eval deploy gate"
row m161 test internal/controller/skill_gate_test.go TestAnAliasMoveDoesNotChangeTheSpecHash "an alias move is not a spec change"
row m161 test internal/controller/skill_inject_test.go TestOnlyRefsReachThePod "only refs and descriptions ride in env; a body is a mounted file"
row m161 test internal/controller/skill_inject_test.go TestDescriptionsRideInEnv "descriptions ride in env, so listing skills is free"
row m161 test internal/controller/skill_inject_test.go TestSkillDirIsSetOnlyWhenBodiesAreMounted "the skill dir is set only when bodies are mounted"
row m161 test internal/bff/skills_test.go TestABranchRefIs422 "the BFF refuses a branch ref with 422"
row m161 test internal/bff/skills_test.go TestVersionAddIsIdempotent "adding a version is idempotent"
# Implementation markers:
row m161 has internal/controlplane/migrations/0026_skills.sql "digest      text        NOT NULL" "the schema keys skill versions by digest"
row m161 has api/v1beta1/agentdeployment_types.go "ResolvedSkills []string" "status carries resolvedSkills, so a snapshot never holds a movable alias"
row m161 has internal/controller/agentdeployment_controller.go "deploy.Status.ResolvedSkills = resolvedSkills" "the controller records what the skill refs resolved to"
row m161 has cmd/launcher/skills.go 'mux.HandleFunc("GET /skills"' "the launcher serves the skill list the SDKs call"
row m161 has cmd/launcher/skills.go "is not attached to this agent" "/skills/load checks the injected roster"
row m161 has cmd/launcher/skills.go "filepath.Base" "the skill mount path is defended independently of the roster check"
row m161 exists sdk/python/src/ctxmesh/skills.py - "the Python SDK has a skills surface"
row m161 exists sdk/typescript/src/skills.ts - "the TypeScript SDK has a skills surface"
row m161 has internal/controlplane/skill/validate.go MaxBodyBytes "a skill body is size-capped (v1 ships no unbounded payload)"
row m161 has-re site:src/content/docs/guides/skills.md "not executables|instructions and data" "the public skills guide states that a skill carries no executables"

# load() returns an attached skill's body: verified against its digest, capped and never truncated,
# and only for skills attached to the agent.
row m184.39c test internal/controller/skill_bodies_integration_test.go TestReconcile_SkillBodyIsMountedAndFollowsTheVersion "an attached skill's body is mounted at SKILL_DIR and follows its version"
row m184.39c test internal/controller/skill_bodies_integration_test.go TestReconcile_SkillDigestMismatchMountsNothing "bytes that do not hash to the pinned digest are never mounted"
row m184.39c test internal/controller/skill_bodies_test.go TestSkillBodiesAreCapped "skill bodies are capped, and one that does not fit is left out whole"
row m184.39c test internal/controller/skill_bodies_test.go TestOnlyAttachedBodiesAreMounted "only attached skills' bodies are mounted"
row m184.39c test internal/controller/skill_bodies_test.go TestAForeignObjectKeyIsNeverRead "an upload key outside its namespace is never read"
row m184.39c test cmd/launcher/skills_test.go TestLoadReturnsTheMountedBodyOfAnAttachedSkill "load() returns the mounted body, and an unattached skill is 403"
row m184.39c has cmd/main.go "SkillGit: prompt.NewHTTPResolver" "the controller fetches git-sourced skill bodies for real, not from a fixture"
row m184.39c lacks internal/controller/agentdeployment_controller.go "injectSkills(containers, resolvedSkills, skillDescriptions, false)" "the controller does not hardcode skill bodies as unmounted"

# Containment: the agent container is hardened, the launcher plane is pod-local, egress control is
# unconditional.
row m162 test internal/controller/hardening_test.go TestTheAgentContainerIsHardenedByDefault "the agent container carries a hardened securityContext by default"
row m162 test internal/controller/hardening_test.go TestTheProfileOmitsWhatWouldBreakRealImages "the hardening profile omits what would break real images"
row m162 test internal/controller/hardening_test.go TestUnconfinedIsByteIdenticalToThePreviousShape "an unconfined agent keeps the previous pod shape"
row m162 has api/v1beta1/agentdeployment_types.go "Unconfined bool" "a per-agent declared exception exists (no cluster-wide switch is the only escape)"
row m162 has internal/controller/agentdeployment_controller.go "func hardeningFold" "the hardening is folded into the revision digest, so it reaches running pods"
row m162 has internal/controller/agentdeployment_controller_test.go "hardeningFold(combinedBindingDigest" "the tests share the production fold instead of reimplementing it"
row m162 has cmd/launcher/memory.go "func loopbackAddr" "a loopback binding helper exists"
row m162 has-re cmd/launcher/memory.go 'Addr:.*loopbackAddr\(' "the memory listener binds loopback"
row m162 has-re cmd/launcher/delegate.go 'Addr:.*loopbackAddr\(' "the delegate listener binds loopback"
row m162 has-re cmd/launcher/feedback.go 'Addr:.*loopbackAddr\(' "the feedback listener binds loopback"
row m162 has-re cmd/launcher/gateway.go 'Addr:.*loopbackAddr\(' "the gateway listener binds loopback"
row m162 has-re cmd/launcher/main.go 'Addr: *loopbackAddr\(cfg\.(A2A|AMP)\.Port\)' "the agent-plane (A2A/AMP) listener binds loopback"
row m162 has cmd/launcher/main.go 'Addr:    fmt.Sprintf(":%d", cfg.ProxyPort)' "the agent's serving port stays open to the kubelet probes"
row m162 has cmd/launcher/memory_test.go "func TestBuildMemoryHTTPServer" "the launcher listener binding has tests"
row m162 has internal/controller/egress_redirect_test.go "func TestEgressRedirect" "the egress redirect rules have tests"
row m162 has Dockerfile.egress-sidecar "USER 64535:64535" "the sidecar runs as the dedicated uid the redirect rules exempt"
row m162 lacks internal/controller/egress_redirect.go "egressSidecarUID = 65532" "the exemption is not back on the distroless nonroot uid any agent image may use"

# AMP: the call surface gets its own name; a rename must never silently disable the guard (ADR 0138).
row m164 has cmd/launcher/amp.go "func envelopeHeaderWithSource" "the guard reads the envelope under both names"
row m164 test cmd/launcher/amp_test.go TestEnvelopeHeaderAcceptsEitherName "an envelope under either name is understood"
row m164 test cmd/launcher/amp_test.go TestGuardEnforcesUnderEitherHeaderName "the guard enforces identically under both names"
row m164 test cmd/launcher/amp_test.go TestGuardDeniesConflictingEnvelopes "two envelopes that disagree fail closed"
row m164 test cmd/launcher/amp_test.go TestOutboundStampsBothEnvelopeNames "a hop carries both names with identical content"
row m164 test cmd/launcher/amp_test.go TestBothOutboundPathsAreServed "/amp and /a2a are both served"
row m164 test cmd/launcher/amp_test.go TestEnvelopeSourceIsRecorded "the guard reports which envelope name a caller used"
row m164 has cmd/launcher/amp.go "Header.Set(ampEnvelopeHeader" "a synchronous hop stamps the AMP envelope"
row m164 has cmd/launcher/async.go "Header.Set(ampEnvelopeHeader" "an asynchronous hop stamps the AMP envelope"
row m164 has cmd/launcher/amp.go "Header.Set(legacyEnvelopeHeader" "a synchronous hop still stamps the legacy envelope (un-upgraded peers keep the guard)"
row m164 has cmd/launcher/async.go "Header.Set(legacyEnvelopeHeader" "an asynchronous hop still stamps the legacy envelope"
row m164 has-tree internal/controller A2A_MAX_DEPTH "the controller still stamps A2A_MAX_DEPTH (running pods keep their guard limits)"
row m164 has cmd/launcher/amp.go A2A_MAX_DEPTH "the launcher still reads A2A_MAX_DEPTH"
row m164 has internal/asyncbus/jetstream.go CTXMESH_A2A "the JetStream stream name was not renamed (in-flight durable hops would strand)"
row m164 has internal/asyncbus/jetstream.go ctxmesh.a2a "the JetStream subject prefix was not renamed"
row m164 has cmd/launcher/async.go "a2a:seen:" "the dedup keyspace prefix was not renamed"
row m164 has config/statelayer/deployment.yaml "~a2a:seen:*" "the Valkey ACL still matches the dedup prefix"
row m164 lacks-tree-re "*.go" '"a2a\.[a-z_]' "no telemetry still emits an a2a.* name (a half-renamed namespace is worse than either)"
row m164 has docs/upgrading.md "amp.call" "the upgrade notes carry the telemetry mapping"
row m164 has docs/upgrading.md "silently return nothing" "the upgrade notes warn that saved queries go quiet rather than erroring"
row m164 has cmd/launcher/amp.go "amp.envelope.source" "the envelope source is recorded on the span"
row m164 files-ge site:src/content/docs "Agent2Agent" "at least three public pages distinguish AMP from Google's A2A" 3

# Ship the beta: a stranger can install from the public docs, the stores are tested against a real
# database, a tag produces a Release a person can see. (The zero-reachable-CVEs claim is the
# `security` job's govulncheck; the required-checks claim is repository state, not source.)
row m165 has .github/workflows/ci.yml CONTROLPLANE_TEST_DSN "CI sets CONTROLPLANE_TEST_DSN, so the store suites run against Postgres, not their twins"
row m165 has .github/workflows/ci.yml pgvector/pgvector "CI's Postgres service is pgvector (migrations 0003/0005 CREATE EXTENSION vector)"
row m165 has .github/workflows/release.yml "gh release create" "a tag produces a GitHub Release page"
row m165 has .github/workflows/release.yml "has no '## \${tag}' section" "a missing CHANGELOG section fails the release instead of publishing an empty one"
row m165 has-re CHANGELOG.md '^## v0\.1\.0-beta\.1' "the CHANGELOG keeps a section for the first beta tag"
row m165 lacks-tree site:src/content/docs "charts.ctxmesh.ai" "the docs do not tell users to add a helm repo at a domain that does not resolve"
row m165 has site:src/content/docs/getting-started/installation.md "oci://ghcr.io/ctxmesh/charts/ctxmesh" "installation.md carries the real OCI chart coordinates"
row m165 has site:src/content/docs/getting-started/installation.md "Knative Eventing" "installation.md lists Knative Eventing"
row m165 exists site:src/content/docs/reference/compatibility.md - "a compatibility page is published"
row m165 has site:src/content/docs/reference/compatibility.md "no independent semver signal" "the compatibility page says what matched versioning does not promise"

# Per-version scoring: every agent pod carries the version it serves, and the contract bump that put it
# there rolls existing agents rather than being dropped under an unchanged revision name.
row m184.23 has internal/controller/agentdeployment_controller.go 'Name: envAgentVersion, Value: agentVersionName(deploy.Name, hash)' "the controller sets AGENT_VERSION on every agent pod"
row m184.23 test internal/controller/agent_version_integration_test.go TestAnAgentRevisionedBeforeAgentVersionRollsOnce "an agent revisioned before AGENT_VERSION rolls to a new revision name"
row m184.23 test internal/controller/agent_version_integration_test.go TestTheDetectorFindsTheVersionsThePodsCarry "the regression detector finds the versions the pods carry"

# Version history is in the control-plane store: the kind is gone, a store outage never blocks a deploy
# or loses a requested rollback, an upgrade keeps its history, and reading it is authorized as the caller.
row m184.37 absent api/v1beta1/agentversion_types.go - "the AgentVersion kind is retired"
row m184.37 absent config/crd/bases/agents.ctxmesh.ai_agentversions.yaml - "no AgentVersion CRD is generated"
row m184.37 test internal/controlplane/agentversion/conformance_test.go TestStoreConformance "the version store passes one contract on memory and Postgres"
row m184.37 test internal/controller/agent_version_store_test.go TestVersion_RecordFailsOpen "a version that cannot be recorded does not block the deploy"
row m184.37 test internal/controller/agent_version_store_test.go TestRollback_HumanWaitsForTheStore "a rollback requested during a store outage waits, and the agent still rolls"
row m184.37 test internal/controller/agentversion_import_test.go TestImportAgentVersions "an upgrade imports existing AgentVersion objects once, and rolls back to them"
row m184.37 test internal/bff/agent_version_diff_test.go TestAgentVersionDiff_WithoutTheGrantIs403 "the version diff is authorized as the caller"
row m184.37 has config/rbac/ctxmesh_viewer_role.yaml '  - agentversions' "the viewer persona keeps the grant the version-history SSAR checks"
row m184.37 test internal/controller/agentversion_import_memory_test.go TestImportAgentVersions_StreamsWithinAMemoryBound "the upgrade import streams instead of holding every object"
row m184.37 test internal/controller/agent_version_store_test.go TestVersion_PruneKeepsTheServingVersionBehindBlockedCandidates "pruning never removes the version that serves"
row m184.37 test internal/controller/agent_version_store_test.go TestConditionMessagesKeepStoreDetailsOut "no condition message carries the store's connection details"

# The identity variables are the platform's: an author cannot tag traces, scores or spend as another
# agent, version or tenant by setting them in spec.env.
row m184 test internal/controller/identity_env_integration_test.go TestAnAgentCannotNameAnotherAgentsIdentity "spec.env cannot set an agent's namespace, name, version or tenant"
row m184 test internal/controller/identity_env_integration_test.go TestAnAgentDeployedWithItsOwnNamespaceRollsOnceToTheRealOne "an agent that set its own identity rolls to the platform's values"

# ── verdict ───────────────────────────────────────────────────────────────────
if [ "$total" -eq 0 ]; then
  echo "RESULT: FAIL (the table is empty — a gate that checks nothing)"
  exit 1
fi
if [ "$total" -lt "$FLOOR" ]; then
  echo "RESULT: FAIL ($total row(s), floor is $FLOOR — a claim was removed from the table)"
  exit 1
fi
if [ "$failed" -gt 0 ]; then
  echo "RESULT: FAIL ($failed of $total shipped-truth row(s) do not hold — see above)"
  exit 1
fi
if [ "$skipped" -gt 0 ]; then
  echo "SKIP: $skipped docs-site row(s) were not checked — the site is not at $SITE" >&2
  echo "RESULT: PASS ($((total - skipped)) of $total shipped-truth rows hold; $skipped docs-site row(s) SKIPPED — the site is not checked out)"
else
  echo "RESULT: PASS (all $total shipped-truth rows hold)"
fi
