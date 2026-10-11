import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AgentBindingsPanel } from "@/components/agent-bindings-panel";
import type { AgentDependency, AgentDetailResponse } from "@/lib/api";
import fixtures from "@/test/contract-fixtures.json";

// The Bindings panel (m184.15): every reference the agent makes, as the BFF resolved it. The base
// detail comes from the contract fixtures so the shape is the one the BFF actually sends.

const base = fixtures.AgentDetailResponse as unknown as AgentDetailResponse;

function renderPanel(dependencies: AgentDependency[] | undefined) {
  const detail = { ...base, dependencies } as AgentDetailResponse;
  return render(
    <MemoryRouter>
      <AgentBindingsPanel detail={detail} />
    </MemoryRouter>,
  );
}

const EVERY_KIND: AgentDependency[] = [
  {
    kind: "modelRoute",
    name: "chat",
    namespace: "prod",
    state: "notReady",
    reason: "NotYetServed",
    message: "ModelRoute chat: route rendered; the gateway has not started serving it yet",
  },
  {
    kind: "secretBinding",
    name: "openai-key",
    namespace: "prod",
    via: "chat",
    state: "ready",
    reason: "Resolved",
    message: "the secret resolves",
  },
  {
    kind: "guardrailPolicy",
    name: "pii",
    namespace: "prod",
    state: "missing",
    reason: "GuardrailPolicyNotFound",
    message: 'There is no GuardrailPolicy named "pii" in prod.',
  },
  {
    kind: "toolBinding",
    name: "crm-refund",
    namespace: "prod",
    detail: "create_refund",
    via: "acme-crm",
    state: "notReady",
    reason: "RegistryNotFound",
    message: "ToolRegistry acme-crm not found",
  },
  { kind: "knowledgeBase", name: "handbook", namespace: "prod", detail: "auto-inject", state: "ready", reason: "Ready" },
  { kind: "sessionMemory", name: "session", detail: "per user", state: "declared" },
  { kind: "longTermMemory", name: "long-term", detail: "embeds with embed", state: "declared" },
  { kind: "approvalRule", name: "create_refund", state: "declared" },
  { kind: "approver", name: "finance", detail: "Group", state: "declared" },
  {
    kind: "feedbackSource",
    name: "human",
    detail: "thumbs, accuracy",
    state: "declared",
    message: "Enforce: a score whose name no source declares is rejected.",
  },
  {
    kind: "knowledgeBase",
    name: "secret-docs",
    namespace: "legal",
    detail: "tool-only",
    state: "unknown",
    reason: "Forbidden",
    message: "Your account can't read this knowledge base, so its state is not shown.",
  },
];

describe("AgentBindingsPanel", () => {
  it("lists every reference kind, each with its resolved state", () => {
    renderPanel(EVERY_KIND);
    const panel = screen.getByTestId("agent-bindings-panel");
    expect(panel).toHaveTextContent("11 references");

    for (const d of EVERY_KIND) {
      expect(screen.getByTestId(`dep-${d.kind}-${d.name}`)).toBeInTheDocument();
    }
    expect(screen.getByTestId("dep-state-secretBinding-openai-key")).toHaveTextContent("ready");
    expect(screen.getByTestId("dep-state-knowledgeBase-handbook")).toHaveTextContent("ready");
    expect(screen.getByTestId("dep-state-sessionMemory-session")).toHaveTextContent("declared");
    expect(screen.getByTestId("dep-state-approver-finance")).toHaveTextContent("declared");
    expect(screen.getByTestId("dep-state-feedbackSource-human")).toHaveTextContent("declared");
    // A reference the caller cannot read claims nothing — neither ready nor missing.
    const hidden = screen.getByTestId("dep-state-knowledgeBase-secret-docs");
    expect(hidden).toHaveTextContent("unknown");
    expect(hidden).not.toHaveClass("text-destructive");
  });

  it("shows a missing guardrail policy as missing, in crit, with the reason and a way to fix it", () => {
    renderPanel(EVERY_KIND);
    const tag = screen.getByTestId("dep-state-guardrailPolicy-pii");
    expect(tag).toHaveTextContent("missing");
    expect(tag).toHaveClass("text-destructive");
    const row = screen.getByTestId("dep-guardrailPolicy-pii");
    expect(row).toHaveTextContent("GuardrailPolicyNotFound");
    expect(row).toHaveTextContent('There is no GuardrailPolicy named "pii" in prod.');
    // A missing reference has no page of its own to open, so its name is not a link.
    expect(within(row).getByText("pii").closest("a")).toBeNull();
    expect(screen.getByTestId("dep-fix-guardrailPolicy-pii")).toHaveAttribute("href", "/guardrails");
  });

  it("shows an unready model route as loading (converging, not failed), with the condition's words", () => {
    renderPanel(EVERY_KIND);
    const tag = screen.getByTestId("dep-state-modelRoute-chat");
    expect(tag).toHaveTextContent("model loading");
    expect(tag).not.toHaveClass("text-destructive");
    const row = screen.getByTestId("dep-modelRoute-chat");
    expect(row).toHaveTextContent("NotYetServed");
    expect(row).toHaveTextContent("has not started serving");
    expect(within(row).getByRole("link", { name: "chat" })).toHaveAttribute("href", "/routes/prod/chat");
  });

  it("shows a not-ready tool binding in crit with its reason, and counts what blocks the agent", () => {
    renderPanel(EVERY_KIND);
    const tag = screen.getByTestId("dep-state-toolBinding-crm-refund");
    expect(tag).toHaveTextContent("not ready");
    expect(tag).toHaveClass("text-destructive");
    expect(screen.getByTestId("dep-toolBinding-crm-refund")).toHaveTextContent("RegistryNotFound");
    // The missing guardrail and the unbound tool block it; the loading route converges on its own.
    expect(screen.getByTestId("bindings-attention")).toHaveTextContent("2");
  });

  it("links each found reference to its own page, without restating a ready row's status", () => {
    renderPanel(EVERY_KIND);
    expect(screen.getByTestId("dep-secretBinding-openai-key")).not.toHaveTextContent("the secret resolves");
    expect(
      within(screen.getByTestId("dep-secretBinding-openai-key")).getByRole("link", { name: "openai-key" }),
    ).toHaveAttribute("href", "/secrets/prod/openai-key");
    expect(screen.getByTestId("dep-secretBinding-openai-key")).toHaveTextContent("used by route chat");
    expect(
      within(screen.getByTestId("dep-knowledgeBase-handbook")).getByRole("link", { name: "handbook" }),
    ).toHaveAttribute("href", "/knowledgebases/prod/handbook");
  });

  it("says plainly when a kind is absent, and what approvals mean together", () => {
    renderPanel([{ kind: "approvalRule", name: "create_refund", state: "declared" }]);
    expect(screen.getByTestId("bindings-group-model")).toHaveTextContent("No model route");
    expect(screen.getByTestId("bindings-group-guardrails")).toHaveTextContent("No guardrail policy.");
    expect(screen.getByTestId("approvals-anyone-note")).toBeInTheDocument();
    expect(screen.queryByTestId("bindings-attention")).toBeNull();
  });

  it("shows a knowledge base nothing will ingest as needing one, with the way to start it", () => {
    renderPanel([
      {
        kind: "knowledgeBase",
        name: "handbook",
        namespace: "prod",
        detail: "tool-only",
        state: "notReady",
        reason: "NeedsIngest",
        message: "Nothing has been ingested yet, and no refresh schedule will start an ingestion.",
      },
    ]);
    const tag = screen.getByTestId("dep-state-knowledgeBase-handbook");
    expect(tag).toHaveTextContent("needs ingest");
    expect(tag).toHaveClass("text-destructive");
    expect(screen.getByTestId("dep-knowledgeBase-handbook")).toHaveTextContent("no refresh schedule");
    expect(screen.getByTestId("dep-fix-knowledgeBase-handbook")).toHaveAttribute(
      "href",
      "/knowledgebases/prod/handbook",
    );
    expect(screen.getByTestId("bindings-attention")).toHaveTextContent("1");
  });

  it("keeps a pending knowledge base its schedule will ingest as converging", () => {
    renderPanel([
      {
        kind: "knowledgeBase",
        name: "handbook",
        namespace: "prod",
        state: "notReady",
        reason: "Pending",
        message: "Nothing has been ingested yet. Its refresh schedule starts the first ingestion.",
      },
    ]);
    const tag = screen.getByTestId("dep-state-knowledgeBase-handbook");
    expect(tag).toHaveTextContent("not ready yet");
    expect(tag).not.toHaveClass("text-destructive");
    expect(screen.queryByTestId("dep-fix-knowledgeBase-handbook")).toBeNull();
    expect(screen.queryByTestId("bindings-attention")).toBeNull();
  });

  it("shows a knowledge base being re-ingested as ready, and says it is re-ingesting", () => {
    renderPanel([
      {
        kind: "knowledgeBase",
        name: "handbook",
        namespace: "prod",
        state: "ready",
        reason: "Ingesting",
        message: "Re-ingesting now. Queries use the last completed ingestion until it finishes.",
      },
    ]);
    expect(screen.getByTestId("dep-state-knowledgeBase-handbook")).toHaveTextContent("ready");
    expect(screen.getByTestId("dep-knowledgeBase-handbook")).toHaveTextContent("Re-ingesting now");
  });

  it("shows a route the controller has not checked since the agent changed as checking", () => {
    renderPanel([
      {
        kind: "modelRoute",
        name: "chat",
        namespace: "prod",
        state: "unknown",
        reason: "Checking",
        message: "The controller has not yet checked the route the agent's current spec names.",
      },
    ]);
    const tag = screen.getByTestId("dep-state-modelRoute-chat");
    expect(tag).toHaveTextContent("checking");
    expect(tag).not.toHaveClass("text-destructive");
    expect(screen.getByTestId("dep-modelRoute-chat")).toHaveTextContent("has not yet checked");
    expect(screen.queryByTestId("bindings-attention")).toBeNull();
  });

  it("lists a route read from a ConfigMap as declared, says where it comes from, and links nowhere", () => {
    renderPanel([
      {
        kind: "modelRoute",
        name: "MODEL_ROUTE",
        namespace: "prod",
        detail: "from ConfigMap llm, key route",
        state: "declared",
        reason: "ValueFrom",
        message: "MODEL_ROUTE is read from ConfigMap llm, key route when the agent starts.",
      },
    ]);
    const row = screen.getByTestId("dep-modelRoute-MODEL_ROUTE");
    expect(screen.getByTestId("dep-state-modelRoute-MODEL_ROUTE")).toHaveTextContent("declared");
    expect(row).toHaveTextContent("from ConfigMap llm, key route");
    expect(row).toHaveTextContent("when the agent starts");
    expect(within(row).queryByRole("link")).toBeNull();
    expect(screen.getByTestId("bindings-group-model")).not.toHaveTextContent("No model route");
  });

  it("claims neither that every reference is listed, nor that no route means no model", () => {
    renderPanel([]);
    const panel = screen.getByTestId("agent-bindings-panel");
    expect(panel).not.toHaveTextContent(/\bEvery\b/);
    const model = screen.getByTestId("bindings-group-model");
    expect(model).toHaveTextContent("No model route");
    expect(model).not.toHaveTextContent("no model to call");
  });

  it("does not guess when the server sent no references", () => {
    renderPanel(undefined);
    expect(screen.getByTestId("agent-bindings-panel")).toHaveTextContent("aren't resolved on this server");
    expect(screen.queryByTestId("bindings-group-model")).toBeNull();
  });
});
