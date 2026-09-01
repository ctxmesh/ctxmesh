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
    // The validated policy shows a "Ready" badge (unified lexicon, M99 E1).
    expect(screen.getByText("Ready")).toBeInTheDocument();
    // The invalid policy shows its reason, humanized (M144.1 status vocabulary).
    expect(screen.getByText("Invalid pattern")).toBeInTheDocument();
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
    expect(await screen.findByText("You don't have permission to view guardrail policies")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/you do not have permission to/)).toBeNull();
    expect(screen.queryByText("No guardrail policies")).toBeNull();
  });

  it("empty → a teaching empty state", async () => {
    installFetch(() => ({ ok: true, body: { items: [] } }));
    renderPage();
    await waitFor(() => expect(screen.getByText("No guardrail policies")).toBeInTheDocument());
  });

  it("no referencing agents is a stated 'not applied', never the unknown dash", async () => {
    installFetch(() => ({
      ok: true,
      // streamingMode set so the streaming column renders a badge — the only "—"
      // this page can produce is an UNRECONCILED value, and this row has none.
      body: { items: [policy({ name: "orphan-policy", referencingAgents: [], streamingMode: "Buffered" })] },
    }));
    renderPage();
    await screen.findByText("orphan-policy");
    // M151 §7.1: zero and unknown never share a glyph. An empty referencingAgents
    // array is a REAL answer ("declared but never exercised", §2.5) and renders
    // the `open` Tag — the dash is reserved for a value the backend never sent.
    expect(screen.getByText("not applied")).toBeInTheDocument();
    expect(screen.queryByText("—")).toBeNull();
  });

  it("an unreconciled streaming mode IS the unknown dash (and is not a zero)", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [policy({ name: "fresh-policy", referencingAgents: ["a-agent"] })] },
    }));
    renderPage();
    await screen.findByText("fresh-policy");
    // No status.streaming yet ⇒ the §7.1 unknown glyph, with its reason on hover.
    const dash = screen.getByText("—");
    expect(dash).toBeInTheDocument();
    expect(dash).toHaveAttribute("title", expect.stringContaining("unknown, not off"));
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

  it("K10 renders the effective streaming mode (M139, ADR 0086)", async () => {
    installFetch(() => ({
      ok: true,
      body: {
        items: [
          policy({ name: "streams", streamingMode: "Streaming", streamingWindow: 23 }),
          policy({ name: "buffered", streamingMode: "Buffered", streamingReason: "not stream-safe" }),
          policy({ name: "unreconciled" }), // no streamingMode ⇒ a dash
        ],
      },
    }));
    renderPage();
    await screen.findByText("streams");

    const streaming = screen.getByTestId("streaming-streams");
    expect(streaming).toHaveTextContent("Streaming");
    expect(streaming).toHaveTextContent("W=23");
    expect(screen.getByTestId("streaming-buffered")).toHaveTextContent("Buffered");
    // A policy not yet reconciled shows no streaming badge.
    expect(screen.queryByTestId("streaming-unreconciled")).toBeNull();
  });
});

// ── M151: archetype A1 — blocking-first order, Next step, honest degraded states ──
describe("GuardrailPoliciesPage — A1 archetype (M151)", () => {
  function threePolicies() {
    return {
      ok: true,
      body: {
        items: [
          // Alphabetically FIRST and perfectly healthy — it must sort LAST.
          policy({ name: "aaa-healthy", referencingAgents: ["a-agent"], streamingMode: "Streaming" }),
          policy({ name: "mmm-unattached", referencingAgents: [], streamingMode: "Streaming" }),
          // Alphabetically LAST and broken — it must sort FIRST.
          policy({ name: "zzz-broken", validated: false, reason: "InvalidPattern: pattern #3 failed to compile", referencingAgents: ["b-agent"], streamingMode: "Streaming" }),
        ],
      },
    };
  }

  it("sorts by what is blocking, not alphabetically", async () => {
    installFetch(threePolicies);
    renderPage();
    await screen.findByText("zzz-broken");

    const names = screen
      .getAllByRole("row")
      .slice(1) // drop the header row
      .map((r) => r.querySelector("td")?.textContent ?? "");
    expect(names[0]).toContain("zzz-broken"); // won't apply — needs a person now
    expect(names[1]).toContain("mmm-unattached"); // valid but guarding nothing
    expect(names[2]).toContain("aaa-healthy"); // "Nothing needed" sinks to the bottom
  });

  it("gives each row a verb-first Next step, and the inert one for a healthy row", async () => {
    installFetch(threePolicies);
    renderPage();
    await screen.findByText("zzz-broken");

    expect(screen.getByTestId("next-step-zzz-broken")).toHaveTextContent("Fix the policy");
    expect(screen.getByTestId("next-step-mmm-unattached")).toHaveTextContent("Attach it to an agent");
    // Nothing needed is NOT a link: inert text, not focusable, no destination.
    const inert = screen.getByTestId("next-step-aaa-healthy");
    expect(inert).toHaveTextContent("Nothing needed");
    expect(inert.tagName).toBe("SPAN");
    // Every label stays inside the §4.4 copy budget.
    for (const id of ["next-step-zzz-broken", "next-step-mmm-unattached"]) {
      const label = screen.getByTestId(id).textContent!.replace(" →", "");
      expect(label.length).toBeLessThanOrEqual(22);
    }
  });

  it("the crit Next step opens the drawer with the FULL controller reason", async () => {
    installFetch(threePolicies);
    renderPage();
    await screen.findByText("zzz-broken");

    fireEvent.click(screen.getByTestId("next-step-zzz-broken"));
    expect(await screen.findByTestId("guardrail-drawer")).toBeInTheDocument();
    // The State cell can only abbreviate the reason; the drawer carries it whole.
    expect(
      screen.getByText("InvalidPattern: pattern #3 failed to compile"),
    ).toBeInTheDocument();
  });

  it("a chip that matched nothing is NOT the first-run empty state", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [policy({ name: "aaa-healthy", referencingAgents: ["a-agent"] })] },
    }));
    renderPage();
    await screen.findByText("aaa-healthy");

    fireEvent.click(screen.getByRole("radio", { name: /Needs attention/ }));
    expect(await screen.findByText("Nothing here needs you")).toBeInTheDocument();
    // The teaching copy must NOT appear — policies exist, this view excluded them.
    expect(screen.queryByText("No guardrail policies")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show everything" }));
    expect(await screen.findByText("aaa-healthy")).toBeInTheDocument();
  });

  it("states the honest ratio in the closing note, from the loaded counts", async () => {
    installFetch(threePolicies);
    renderPage();
    await screen.findByText("zzz-broken");
    expect(
      screen.getByText(
        "2 of the 3 policies need a person: 1 won't apply until someone fixes it, 1 guards nothing yet. The other one is in force.",
      ),
    ).toBeInTheDocument();
  });

  it("a cluster that reports no streaming mode at all gets a QuietNote, not a column of unexplained dashes", async () => {
    installFetch(() => ({
      ok: true,
      body: { items: [policy({ name: "old-controller", referencingAgents: ["a-agent"] })] },
    }));
    renderPage();
    await screen.findByText("old-controller");
    const note = screen.getByRole("note");
    expect(note).toHaveTextContent("Effective streaming mode isn't reported here.");
    // It is a note, never an error — nothing broke.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not render the QuietNote when the controller does report the mode", async () => {
    installFetch(threePolicies);
    renderPage();
    await screen.findByText("zzz-broken");
    expect(screen.queryByRole("note")).toBeNull();
  });
});
