import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ApprovalsPage } from "@/pages/approvals-page";
import type { ApprovalQueueItem } from "@/lib/api";

// The queue is namespace-scoped and the backend REQUIRES a namespace, so the page reads the global
// namespace selector. Mock it (controllable) — a real namespace for the list tests, "" for the
// select-a-namespace prompt. vi.hoisted so the mock factory can read the current value.
const nsRef = vi.hoisted(() => ({ current: "team-a" }));
vi.mock("@/lib/namespace", () => ({
  useNamespace: () => ({ namespace: nsRef.current }),
}));

// ApprovalsPage (V15, M113) — the namespace-scoped unified approval inbox.
//
// Coverage:
//   • rows render with a link to /runs/{runId}
//   • the request carries ?namespace=<selected ns>
//   • a 403 renders the forbidden state (not a fake empty list)
//   • the empty state renders on []
//   • new M113 columns: namespace, kind badge (plan_approval / approval), waitingSince
//   • M151: the §4.4 queue budget — inline Approve/Deny on a row that states its
//     whole ask, a "Next step" link on one that does not; the §2 colour doctrine
//     (kind tag neutral, hold spent once on the page count); and the four §7
//     degraded states kept visibly distinct, above all "nothing is waiting on
//     you" vs. "you cannot see what is waiting"

function item_(over: Partial<ApprovalQueueItem> = {}): ApprovalQueueItem {
  return {
    runId: "run-abc123",
    agent: "my-agent",
    namespace: "team-a",
    kind: "plan_approval",
    ...over,
  };
}

// installFetch stubs global fetch; the responder receives the parsed query string.
function installFetch(
  respond: (qs: URLSearchParams) => { ok: boolean; status?: number; body?: unknown },
) {
  const captured: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      captured.push(url);
      const qs = new URLSearchParams(url.split("?")[1] ?? "");
      const r = respond(qs);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body ?? [],
        text: async () => JSON.stringify(r.body ?? { error: "err" }),
      } as Response);
    }),
  );
  return captured;
}

function renderPage(initialEntry = "/approvals") {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Routes>
        <Route path="/approvals" element={<ApprovalsPage />} />
        <Route path="/runs/:id" element={<div data-testid="run-detail" />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  nsRef.current = "team-a"; // a real namespace by default; the no-namespace test overrides
});

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Basic rendering ────────────────────────────────────────────────────────────

describe("ApprovalsPage — basic rendering (V15, M113)", () => {
  it('renders "Approvals" title (unified inbox, not "Plan approvals")', async () => {
    installFetch(() => ({ ok: true, body: [] }));
    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByRole("heading", { name: "Approvals" })).toBeInTheDocument();
    // The old "Plan approvals" title must be gone
    expect(screen.queryByRole("heading", { name: "Plan approvals" })).toBeNull();
  });

  it("renders approvals-page and approvals table with row data + run links", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        item_({ runId: "run-abc", agent: "workflow-agent-1" }),
        item_({ runId: "run-def", agent: "workflow-agent-2", message: "Proceed with plan?" }),
      ],
    }));

    renderPage();

    expect(await screen.findByTestId("approvals-page")).toBeInTheDocument();
    // The table is rendered (aria-label)
    expect(screen.getByRole("table", { name: "Approvals" })).toBeInTheDocument();
    // Run IDs appear as links
    const runAbcLink = screen.getByRole("link", { name: "run-abc" });
    expect(runAbcLink).toBeInTheDocument();
    expect(runAbcLink).toHaveAttribute("href", "/runs/run-abc");
    const runDefLink = screen.getByRole("link", { name: "run-def" });
    expect(runDefLink).toHaveAttribute("href", "/runs/run-def");
    // Agent names appear
    expect(screen.getByText("workflow-agent-1")).toBeInTheDocument();
    expect(screen.getByText("workflow-agent-2")).toBeInTheDocument();
    // Message appears
    expect(screen.getByText("Proceed with plan?")).toBeInTheDocument();
  });

  it("shows rootRunId context when present", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        item_({ runId: "run-child", agent: "sub-agent", rootRunId: "run-root" }),
      ],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByText("part of")).toBeInTheDocument();
    // rootRunId links to its own run detail page
    const rootLink = screen.getByRole("link", { name: "run-root" });
    expect(rootLink).toHaveAttribute("href", "/runs/run-root");
  });
});

// ── M113: namespace column ─────────────────────────────────────────────────────

describe("ApprovalsPage — namespace column (M113)", () => {
  it("renders the namespace column value from item.namespace", async () => {
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-ns-test", namespace: "production" })],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByText("production")).toBeInTheDocument();
  });
});

// ── M113: kind badge ──────────────────────────────────────────────────────────

describe("ApprovalsPage — kind badge (M113)", () => {
  it('shows "Plan gate" badge for plan_approval kind', async () => {
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-plan", kind: "plan_approval" })],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByText("Plan gate")).toBeInTheDocument();
  });

  it('shows "Step approval" badge for approval kind', async () => {
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-step", kind: "approval" })],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByText("Step approval")).toBeInTheDocument();
  });

  it("renders both badge kinds when the queue has mixed items", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        item_({ runId: "run-plan", kind: "plan_approval" }),
        item_({ runId: "run-step", kind: "approval" }),
      ],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    expect(screen.getByText("Plan gate")).toBeInTheDocument();
    expect(screen.getByText("Step approval")).toBeInTheDocument();
  });
});

// ── M113: waiting since column ─────────────────────────────────────────────────

describe("ApprovalsPage — waitingSince column (M113)", () => {
  it('renders "—" when waitingSince is absent', async () => {
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-no-wait" })], // no waitingSince
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    // The "—" dash for the waiting-since cell
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
  });

  it("renders a relative time string for a waitingSince timestamp", async () => {
    // Use a timestamp 2 hours in the past
    const twoHoursAgo = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-wait", waitingSince: twoHoursAgo })],
    }));

    renderPage();

    await screen.findByTestId("approvals-page");
    // formatRelativeTime returns "2h ago" for ~2 hours. Scoped to the TABLE:
    // the closing line now says the same thing about the oldest row, and an
    // unscoped query would match both.
    const table = screen.getByRole("table", { name: "Approvals" });
    expect(within(table).getByText(/\dh ago/)).toBeInTheDocument();
  });
});

// ── Namespace scoping ──────────────────────────────────────────────────────────

describe("ApprovalsPage — namespace param (V5, M112)", () => {
  it("sends ?namespace=<selected> when a namespace is chosen", async () => {
    const captured = installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    await waitFor(() => expect(captured.length).toBeGreaterThan(0));
    expect(captured[0]).toContain("/api/approvals");
    expect(captured[0]).toContain("namespace=team-a");
  });

  it("aggregates the queue across all visible namespaces when none is selected (M144.6)", async () => {
    nsRef.current = ""; // the default "all namespaces" scope
    const captured = installFetch((qs) =>
      qs.has("namespace")
        ? { ok: true, body: [] }
        : { ok: true, body: { namespaces: [{ name: "team-a" }, { name: "team-b" }] } },
    );

    renderPage();

    // It does NOT dead-end; it lists namespaces then fetches approvals for each.
    await waitFor(() => expect(captured.some((u) => u.includes("/api/namespaces"))).toBe(true));
    await waitFor(() => expect(captured.some((u) => u.includes("namespace=team-a"))).toBe(true));
    expect(captured.some((u) => u.includes("namespace=team-b"))).toBe(true);
    expect(screen.queryByText("Select a namespace")).not.toBeInTheDocument();
  });
});

// ── V16 (M115): manual refresh ─────────────────────────────────────────────────

describe("ApprovalsPage — manual refresh (V16, M115)", () => {
  it("the Refresh button refetches the queue", async () => {
    const captured = installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    await waitFor(() => expect(captured.length).toBe(1));
    const btn = await screen.findByTestId("approvals-refresh");
    fireEvent.click(btn);
    await waitFor(() => expect(captured.length).toBe(2));
    // The refetch still carries the selected namespace.
    expect(captured[1]).toContain("namespace=team-a");
  });

  it("the Refresh button works in the all-namespaces view (M144.6 — no longer a dead-end)", async () => {
    nsRef.current = "";
    const captured = installFetch((qs) =>
      qs.has("namespace")
        ? { ok: true, body: [] }
        : { ok: true, body: { namespaces: [{ name: "team-a" }] } },
    );

    renderPage();

    await waitFor(() => expect(captured.some((u) => u.includes("namespace=team-a"))).toBe(true));
    expect(screen.getByTestId("approvals-refresh")).not.toBeDisabled();
  });

  // The manual refresh is SILENT: it must NOT blank the table with a skeleton (the close-gate UX finding).
  // While the refresh is in flight the existing rows stay visible and the button shows a "Refreshing…" spinner.
  it("manual refresh keeps rows visible (no skeleton flash) and spins the button", async () => {
    let call = 0;
    let resolveRefresh: (() => void) | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(() => {
        call += 1;
        const rowsResponse = {
          ok: true,
          status: 200,
          json: async () => [item_({ runId: "run-keep" })],
          text: async () => "[]",
        } as Response;
        if (call === 1) return Promise.resolve(rowsResponse); // initial load: one row
        // The refresh fetch stays pending until we resolve it, so the "refreshing" state is observable.
        return new Promise<Response>((res) => {
          resolveRefresh = () => res(rowsResponse);
        });
      }),
    );

    renderPage();
    await screen.findByRole("link", { name: "run-keep" });

    fireEvent.click(screen.getByTestId("approvals-refresh"));

    // Mid-refresh: the button spins AND the existing row is still on screen (no skeleton took its place).
    await waitFor(() => expect(screen.getByText("Refreshing…")).toBeInTheDocument());
    expect(screen.getByRole("link", { name: "run-keep" })).toBeInTheDocument();

    resolveRefresh?.();
  });
});

// ── Empty state ────────────────────────────────────────────────────────────────

describe("ApprovalsPage — empty state (V5, M112)", () => {
  it("renders the good-news empty state when items is [] — NOT a bare 'no results'", async () => {
    installFetch(() => ({ ok: true, body: [] }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText("Nothing is waiting on you.")).toBeInTheDocument(),
    );
  });
});

// ── Degrade discipline: 403 forbidden / 500 error ─────────────────────────────

describe("ApprovalsPage — 403 / 500 states (V5, M112)", () => {
  it("403 renders the forbidden state — never a fake empty list", async () => {
    installFetch(() => ({
      ok: false,
      status: 403,
      body: { error: "you don't have permission to list workflows in this namespace" },
    }));

    renderPage();

    // The DataTable forbidden variant surfaces an access-denied message.
    // The exact text comes from DataTable's forbidden prop — check for it.
    await waitFor(() => {
      // DataTable with forbidden=true shows a message containing the resource name.
      // We assert the forbidden state is rendered (NOT an empty list, NOT a Retry button).
      expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
    });
    // No Retry button on 403 (forbidden is terminal).
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("500 surfaces a visible, retryable error", async () => {
    installFetch(() => ({
      ok: false,
      status: 500,
      body: { error: "failed to read the approval queue" },
    }));

    renderPage();

    await waitFor(() =>
      expect(screen.getByText(/failed to read the approval queue/)).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /Retry/ })).toBeInTheDocument();
    expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
  });
});

// ── M151: the queue archetype (§4.4 budget, §2 colour doctrine, §7 states) ────

// installDecisionFetch stubs the queue GET plus the POST /api/runs/{id}/resume the
// inline decision controls fire, recording every call so the wire body can be asserted.
function installDecisionFetch(
  rows: ApprovalQueueItem[],
  resume: { ok: boolean; status?: number; body?: unknown } = { ok: true },
) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      if (url.includes("/resume")) {
        return Promise.resolve({
          ok: resume.ok,
          status: resume.status ?? (resume.ok ? 200 : 403),
          json: async () => resume.body ?? { id: "x", status: "running" },
          text: async () => JSON.stringify(resume.body ?? { error: "denied" }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => rows,
        text: async () => JSON.stringify(rows),
      } as Response);
    }),
  );
  return calls;
}

describe("ApprovalsPage — inline decide (§4.4 queue budget)", () => {
  it("a step approval that states its whole ask is decidable in the row", async () => {
    installDecisionFetch([
      item_({ runId: "run-step", kind: "approval", message: "Approve create_refund?" }),
    ]);
    renderPage();

    expect(await screen.findByTestId("approvals-approve-run-step")).toBeInTheDocument();
    expect(screen.getByTestId("approvals-deny-run-step")).toBeInTheDocument();
  });

  it("Approve posts decision=approve and the row leaves the queue", async () => {
    const calls = installDecisionFetch([
      item_({ runId: "run-step", kind: "approval", message: "Approve create_refund?" }),
    ]);
    renderPage();

    fireEvent.click(await screen.findByTestId("approvals-approve-run-step"));

    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url === "/api/runs/run-step/resume" &&
            c.method === "POST" &&
            c.body.includes('"decision":"approve"'),
        ),
      ).toBe(true),
    );
    // The run is genuinely no longer awaiting a person, so it leaves the view.
    await waitFor(() =>
      expect(screen.queryByTestId("approvals-approve-run-step")).toBeNull(),
    );
  });

  it("Deny goes through a confirmation before it resumes the run", async () => {
    const calls = installDecisionFetch([
      item_({ runId: "run-step", kind: "approval", message: "Approve create_refund?" }),
    ]);
    renderPage();

    fireEvent.click(await screen.findByTestId("approvals-deny-run-step"));
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toBeInTheDocument();
    // Nothing has been sent yet — the confirmation is the gate.
    expect(calls.some((c) => c.url.includes("/resume"))).toBe(false);

    fireEvent.change(screen.getByTestId("approvals-deny-reason"), {
      target: { value: "duplicate charge" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /^deny$/i }));

    await waitFor(() =>
      expect(
        calls.some(
          (c) =>
            c.url === "/api/runs/run-step/resume" &&
            c.body.includes('"decision":"deny"') &&
            c.body.includes("duplicate charge"),
        ),
      ).toBe(true),
    );
  });

  it("a plan gate is NOT decidable from the row — it links to the plan", async () => {
    installDecisionFetch([
      item_({ runId: "run-plan", kind: "plan_approval", message: "Plan: 6 steps." }),
    ]);
    renderPage();

    const next = await screen.findByTestId("approvals-review-run-plan");
    expect(next).toHaveTextContent("Read the plan");
    expect(next).toHaveAttribute("href", "/runs/run-plan");
    expect(screen.queryByTestId("approvals-approve-run-plan")).toBeNull();
  });

  it("a step approval with no recorded ask is not decidable either", async () => {
    installDecisionFetch([item_({ runId: "run-blank", kind: "approval" })]);
    renderPage();

    const next = await screen.findByTestId("approvals-review-run-blank");
    expect(next).toHaveTextContent("Open the ask");
    expect(screen.queryByTestId("approvals-approve-run-blank")).toBeNull();
  });

  it("a rejected decision is surfaced, and says the run is still waiting", async () => {
    installDecisionFetch(
      [item_({ runId: "run-step", kind: "approval", message: "Approve create_refund?" })],
      { ok: false, status: 403, body: { error: "not a designated approver" } },
    );
    renderPage();

    fireEvent.click(await screen.findByTestId("approvals-approve-run-step"));

    const alert = await screen.findByTestId("approvals-action-error");
    expect(alert).toHaveTextContent(/still waiting/);
    // The row must NOT disappear — nothing was decided.
    expect(screen.getByTestId("approvals-approve-run-step")).toBeInTheDocument();
  });
});

describe("ApprovalsPage — colour doctrine (§2)", () => {
  it("the kind tag is the neutral register, never the brand/progressing chip", async () => {
    installFetch(() => ({ ok: true, body: [item_({ runId: "run-plan", kind: "plan_approval" })] }));
    renderPage();

    const tag = await screen.findByText("Plan gate");
    // `muted` = bg-surface-2 / text-muted-foreground. The old `default` variant
    // is now aliased onto `progressing` (bg-accent), which would say "the
    // machine is working on it" on a queue that is blocked on a person.
    expect(tag.className).toContain("bg-surface-2");
    expect(tag.className).not.toContain("bg-accent");
  });

  it("the hold hue is spent once, on the page's own count", async () => {
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-a" }), item_({ runId: "run-b" })],
    }));
    renderPage();

    const chip = await screen.findByTestId("approvals-waiting-count");
    expect(chip).toHaveTextContent("2 awaiting a person");
    expect(chip.className).toContain("bg-hold-surface");
  });
});

describe("ApprovalsPage — honest degraded states (§7)", () => {
  it("501 renders the absent-backend state, not an empty queue", async () => {
    installFetch(() => ({ ok: false, status: 501, body: { error: "not implemented" } }));
    renderPage();

    expect(await screen.findByText(/part of this install/i)).toBeInTheDocument();
    // Distinct from good news, and distinct from a broken request.
    expect(screen.queryByText("Nothing is waiting on you.")).toBeNull();
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("a workspace that refuses is COUNTED, never swallowed into an empty inbox", async () => {
    nsRef.current = "";
    installFetch((qs) => {
      const ns = qs.get("namespace");
      if (ns === null) {
        return { ok: true, body: { namespaces: [{ name: "team-a" }, { name: "team-b" }] } };
      }
      if (ns === "team-b") return { ok: false, status: 403, body: { error: "denied" } };
      return { ok: true, body: [] };
    });
    renderPage();

    // team-a is genuinely empty, so the good news stands — but the page says out
    // loud that one workspace was not readable, so "nothing waiting" is never
    // confused with "you cannot see what is waiting".
    expect(await screen.findByText(/did not return their\s+queue/)).toBeInTheDocument();
    expect(screen.getByText("Nothing is waiting on you.")).toBeInTheDocument();
  });

  it("every workspace refusing is the forbidden state, not an empty inbox", async () => {
    nsRef.current = "";
    installFetch((qs) =>
      qs.has("namespace")
        ? { ok: false, status: 403, body: { error: "denied" } }
        : { ok: true, body: { namespaces: [{ name: "team-a" }, { name: "team-b" }] } },
    );
    renderPage();

    await waitFor(() =>
      expect(screen.queryByText("Nothing is waiting on you.")).toBeNull(),
    );
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });
});

describe("ApprovalsPage — the closing line (§5.18)", () => {
  it("counts the rows in hand and stays grammatical at one", async () => {
    installFetch(() => ({
      ok: true,
      body: [item_({ runId: "run-only", kind: "plan_approval" })],
    }));
    renderPage();

    expect(
      await screen.findByText(/One decision is waiting on you: a plan gate\./),
    ).toBeInTheDocument();
  });

  it("names the mix when both kinds are present", async () => {
    installFetch(() => ({
      ok: true,
      body: [
        item_({ runId: "run-plan", kind: "plan_approval" }),
        item_({ runId: "run-step-1", kind: "approval", message: "a" }),
        item_({ runId: "run-step-2", kind: "approval", message: "b" }),
      ],
    }));
    renderPage();

    expect(
      await screen.findByText(/3 decisions are waiting on you — 1 plan gate and 2 step approvals\./),
    ).toBeInTheDocument();
  });
});
