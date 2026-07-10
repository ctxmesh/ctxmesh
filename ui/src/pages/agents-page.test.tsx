import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { AgentsPage } from "@/pages/agents-page";
import { NamespaceProvider, useNamespace } from "@/lib/namespace";

// The agents list is now the FIRST DataTable consumer (ui-foundation §4/§6): it
// reads the list contract's `items` + `nextCursor` (NOT the legacy `agents`
// key), pages by the opaque cursor, and filters the loaded window with `q`. A
// recording fetch mock captures every /api/agents URL (so we can assert the
// query params — cursor / q / namespace) and answers with scripted pages.
interface Captured {
  url: string;
}

// installFetch routes /api/namespaces (the picker) and /api/agents (the list).
// `agentsResponder` receives the parsed query so a test can key its page off the
// cursor / q / namespace and exercise real pagination + scoping.
function installFetch(opts: {
  namespaces?: { name: string }[];
  agents: (qs: URLSearchParams) => { ok: boolean; status?: number; body: unknown };
}) {
  const calls: Captured[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      calls.push({ url });
      if (url.startsWith("/api/namespaces")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ namespaces: opts.namespaces ?? [] }),
        } as Response);
      }
      const qs = new URLSearchParams(url.split("?")[1] ?? "");
      const r = opts.agents(qs);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body,
      } as Response);
    }),
  );
  return calls;
}

function agent(name: string, namespace = "prod") {
  return { name, namespace, image: `${name}:1`, phase: "Ready", ready: true };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("AgentsPage (DataTable + list contract)", () => {
  it("renders the teaching empty-state when the BFF returns no items", async () => {
    installFetch({ agents: () => ({ ok: true, body: { agents: [], items: [], nextCursor: "" } }) });
    render(<AgentsPage />);
    expect(await screen.findByText("No agents yet")).toBeInTheDocument();
  });

  it("renders a row from `items` (not the legacy `agents` key)", async () => {
    installFetch({
      // `agents` is intentionally EMPTY: the DataTable must read `items`.
      agents: () => ({ ok: true, body: { agents: [], items: [agent("echo")], nextCursor: "" } }),
    });
    render(<AgentsPage />);
    expect(await screen.findByText("echo")).toBeInTheDocument();
    expect(screen.getByText("prod")).toBeInTheDocument();
    expect(screen.getByText("echo:1")).toBeInTheDocument();
  });

  it("paginates by the opaque cursor (Next/Prev walk the page stack)", async () => {
    const calls = installFetch({
      agents: (qs) => {
        const cursor = qs.get("cursor") ?? "";
        if (cursor === "") {
          return { ok: true, body: { agents: [], items: [agent("echo-0")], nextCursor: "c1" } };
        }
        return { ok: true, body: { agents: [], items: [agent("echo-1")], nextCursor: "" } };
      },
    });

    render(<AgentsPage />);
    expect(await screen.findByText("echo-0")).toBeInTheDocument();

    const next = screen.getByRole("button", { name: /Next page/ });
    const prev = screen.getByRole("button", { name: /Previous page/ });
    expect(next).toBeEnabled();
    expect(prev).toBeDisabled();

    fireEvent.click(next);
    expect(await screen.findByText("echo-1")).toBeInTheDocument();
    // The 2nd page was fetched with ?cursor=c1 (the opaque continue token).
    expect(calls.some((c) => c.url.includes("cursor=c1"))).toBe(true);
    // On the last page Next is dead (nextCursor ""), Prev is live.
    expect(screen.getByRole("button", { name: /Next page/ })).toBeDisabled();
    expect(screen.getByRole("button", { name: /Previous page/ })).toBeEnabled();

    // Prev walks back to page 0 (echo-0).
    fireEvent.click(screen.getByRole("button", { name: /Previous page/ }));
    expect(await screen.findByText("echo-0")).toBeInTheDocument();
  });

  it("keeps Next live on an empty FILTERED window when more pages exist (cursor-vs-q)", async () => {
    // The server returns an EMPTY window for this filter but a non-empty
    // nextCursor — the matches may be on a later page. Next must stay live and
    // the honest 'no matches in this page' state renders (NOT a dead end).
    installFetch({
      agents: (qs) => {
        const q = qs.get("q");
        if (q) return { ok: true, body: { agents: [], items: [], nextCursor: "c2" } };
        return { ok: true, body: { agents: [], items: [agent("echo-0")], nextCursor: "c2" } };
      },
    });

    render(<AgentsPage />);
    expect(await screen.findByText("echo-0")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Filter list"), {
      target: { value: "zzz" },
    });

    expect(await screen.findByText("No matches in this page")).toBeInTheDocument();
    // hasNext keys off nextCursor, not the (zero) row count → Next stays live.
    expect(screen.getByRole("button", { name: /Next page/ })).toBeEnabled();
  });

  it("labels the filter as a windowed FILTER, never a global search", async () => {
    installFetch({ agents: () => ({ ok: true, body: { agents: [], items: [agent("echo")], nextCursor: "" } }) });
    render(<AgentsPage />);
    await screen.findByText("echo");
    expect(
      screen.getByPlaceholderText(/Filter agents on this page/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/filter is windowed to the loaded page/i),
    ).toBeInTheDocument();
  });

  it("renders the forbidden variant on a 403, not a fake empty list", async () => {
    installFetch({
      agents: () => ({
        ok: false,
        status: 403,
        body: { error: "forbidden: cannot list agentdeployments" },
      }),
    });
    render(<AgentsPage />);
    expect(
      await screen.findByText(/forbidden: cannot list agentdeployments/),
    ).toBeInTheDocument();
    // A forbidden error offers no misleading "Retry" (RBAC won't change on retry).
    expect(screen.queryByRole("button", { name: /Retry/ })).toBeNull();
  });

  it("scopes the list to the shell's selected namespace", async () => {
    const calls = installFetch({
      namespaces: [{ name: "team-a" }],
      agents: (qs) => {
        const ns = qs.get("namespace");
        return {
          ok: true,
          body: {
            agents: [],
            items: [agent(ns === "team-a" ? "scoped" : "all-ns", ns ?? "prod")],
            nextCursor: "",
          },
        };
      },
    });

    render(
      <NamespaceProvider>
        <NsSwitcher />
        <AgentsPage />
      </NamespaceProvider>,
    );

    expect(await screen.findByText("all-ns")).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("ns-switch"), {
      target: { value: "team-a" },
    });

    expect(await screen.findByText("scoped")).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("namespace=team-a"))).toBe(true);
  });
});

// NsSwitcher stands in for the shell's namespace picker, bound to the SAME
// NamespaceProvider the page consumes — so selecting a namespace re-scopes the
// page's list exactly as the real shell does.
function NsSwitcher() {
  const { namespace, setNamespace, list } = useNamespace();
  const options = list.kind === "ready" ? list.namespaces.map((n) => n.name) : [];
  return (
    <select
      aria-label="ns-switch"
      value={namespace}
      onChange={(e) => setNamespace(e.target.value)}
    >
      <option value="">All namespaces</option>
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </select>
  );
}
