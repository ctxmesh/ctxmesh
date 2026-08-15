import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { GuardrailPoliciesPage } from "@/pages/guardrail-policies-page";
import type { GuardrailPolicySummary } from "@/lib/api";

// GuardrailPoliciesPage (m66.10) — the GuardrailPolicy content-governance policies (read-only).

function policy(over: Partial<GuardrailPolicySummary> = {}): GuardrailPolicySummary {
  return {
    name: "pii-and-jailbreak",
    namespace: "default",
    piiEnabled: true,
    denylistCount: 2,
    judgeEnabled: true,
    failMode: "closed",
    userRateLimited: true,
    validated: true,
    policyHash: "sha256-abc",
    referencingAgents: ["echo-agent", "chat-agent"],
    ...over,
  };
}

function installFetch(respond: () => { ok: boolean; status?: number; body?: unknown }) {
  vi.stubGlobal(
    "fetch",
    vi.fn(() => {
      const r = respond();
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? { items: [] },
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={["/guardrails"]}>
      <Routes>
        <Route path="/guardrails" element={<GuardrailPoliciesPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

describe("GuardrailPoliciesPage (m66.10)", () => {
  it("renders policies with detectors summary, failMode, agent count, and status badge", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          policy({ name: "pii-and-jailbreak", validated: true, referencingAgents: ["echo-agent", "chat-agent"] }),
          policy({ name: "lenient", piiEnabled: false, judgeEnabled: false, denylistCount: 0, userRateLimited: false, failMode: "open", validated: false, reason: "InvalidPattern", referencingAgents: [] }),
        ],
      },
    }));

    renderPage();

    expect(await screen.findByTestId("guardrail-policies-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Guardrail policies" })).toBeInTheDocument();
    expect(screen.getByText("pii-and-jailbreak")).toBeInTheDocument();
    // The validated policy shows a "valid" badge.
    expect(screen.getByText("valid")).toBeInTheDocument();
    // The invalid policy shows its reason.
    expect(screen.getByText("InvalidPattern")).toBeInTheDocument();
    // Fail mode badges — closed is salient (success variant), open is warning (riskier posture).
    const closedBadge = screen.getByText("closed");
    expect(closedBadge).toBeInTheDocument();
    expect(closedBadge.className).toMatch(/bg-success/);
    const openBadge = screen.getByText("open");
    expect(openBadge).toBeInTheDocument();
    expect(openBadge.className).toMatch(/bg-warning/);
    // Referencing agents count.
    expect(screen.getByText("2 agents")).toBeInTheDocument();
    // Detectors summary for the PII+judge+rate policy — the column shows
    // "PII, 2 deny rules, judge, rate-limited" in the table cell (not the description paragraph).
    expect(screen.getAllByText(/PII/).length).toBeGreaterThan(0);
  });

  it("filters policies by name", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [policy({ name: "pii-strict" }), policy({ name: "lenient", failMode: "open", validated: false, reason: "InvalidPattern" })] },
    }));
    renderPage();
    await screen.findByText("pii-strict");

    fireEvent.change(screen.getByLabelText("Filter list"), { target: { value: "len" } });
    await waitFor(() => expect(screen.queryByText("pii-strict")).not.toBeInTheDocument());
    expect(screen.getByText("lenient")).toBeInTheDocument();
  });

  it("403 surfaces a forbidden state (never a fake empty list)", async () => {
    installFetch(() => ({ ok: false, status: 403, body: { error: "you do not have permission to list guardrailpolicies" } }));
    renderPage();
    await waitFor(() =>
      expect(screen.getByText(/you do not have permission to list guardrailpolicies/)).toBeInTheDocument(),
    );
    expect(screen.queryByText("No guardrail policies")).toBeNull();
  });

  it("empty → a teaching empty state", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("No guardrail policies")).toBeInTheDocument());
  });

  it("no referencing agents shows a dash", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [policy({ name: "orphan-policy", referencingAgents: [] })] },
    }));
    renderPage();
    await screen.findByText("orphan-policy");
    // The agents column shows "—" when referencingAgents is empty.
    expect(screen.getByText("—")).toBeInTheDocument();
  });
});

// ── K7 m76.6: GuardrailPolicy console P3 polish ──────────────────────────────
describe("GuardrailPoliciesPage — K7 polish (m76.6)", () => {
  it("K7(c) shows the single agent's name (not '1 agent') when exactly one agent references the policy", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          policy({ name: "solo-policy", referencingAgents: ["billing-agent"] }),
        ],
      },
    }));
    renderPage();
    await screen.findByText("solo-policy");
    // Shows the agent name directly — more actionable than "1 agent".
    expect(screen.getByText("billing-agent")).toBeInTheDocument();
    // Must NOT show the generic "1 agent" text.
    expect(screen.queryByText("1 agent")).toBeNull();
  });

  it("K7(c) shows count for multiple agents", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          policy({ name: "multi-policy", referencingAgents: ["agent-a", "agent-b", "agent-c"] }),
        ],
      },
    }));
    renderPage();
    await screen.findByText("multi-policy");
    expect(screen.getByText("3 agents")).toBeInTheDocument();
  });
});
