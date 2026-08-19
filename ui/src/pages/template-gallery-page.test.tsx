import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { TemplateGalleryPage } from "@/pages/template-gallery-page";
import { ToastProvider } from "@/components/kit";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";

// LocationSpy captures the last navigation target so tests can assert routing.
function LocationSpy({ onLocation }: { onLocation: (loc: string, state: unknown) => void }) {
  const loc = useLocation();
  onLocation(loc.pathname + loc.search, loc.state);
  return null;
}

// Fake fetch for the template gallery page. Covers:
//   - GET /api/templates  — the template list (recipes + published agents)
//   - GET /api/catalog    — the MCP catalog tab
//   - GET /api/mcpservers — the caller's owned list (for T10 cross-check)
//   - POST /api/agents/.../fork — the fork action
//   - POST /api/mcp/connect — the connect action

interface TemplateRow {
  kind: string;
  source: "recipe" | "published";
  name: string;
  description?: string;
  spec?: string;
  provenance?: {
    originNamespace?: string;
    originName?: string;
    version?: string;
  } | "builtin";
  visibility?: string;
  alreadyForkedAs?: { namespace: string; name: string };
}

interface CatalogRow {
  name: string;
  namespace: string;
  toolCount: number;
  authType?: string;
  visibility: string;
  description?: string;
  credentialSource?: string;
}

interface OwnedRow {
  name: string;
  namespace: string;
  url?: string;
  toolCount?: number;
  status?: string;
}

const defaultRecipe: TemplateRow = {
  kind: "agent",
  source: "recipe",
  name: "summarizer-recipe",
  description: "Summarizes any document",
  spec: "apiVersion: ctxmesh.ai/v1\nkind: Agent",
  provenance: "builtin",
};

const defaultPublished: TemplateRow = {
  kind: "agent",
  source: "published",
  name: "support-agent",
  description: "A support bot",
  provenance: {
    originNamespace: "prod",
    originName: "support-agent",
    version: "v2",
  },
  visibility: "org",
};

const defaultCatalogEntry: CatalogRow = {
  name: "scalekit-mcp",
  namespace: "platform",
  toolCount: 10,
  authType: "oauth",
  visibility: "org",
};

function makeFetch(opts?: {
  templates?: TemplateRow[];
  forkStatus?: number;
  forkResponse?: object;
  catalogEntries?: CatalogRow[];
  connectStatus?: number;
  connectResponse?: object;
  ownedServers?: OwnedRow[];
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const j = (body: unknown, ok = true, status = 200) =>
        Promise.resolve({
          ok,
          status,
          json: async () => body,
          text: async () => JSON.stringify(body),
        } as Response);

      // Namespace check (CapabilitiesProvider / NamespaceProvider if needed)
      if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (path.startsWith("/api/capabilities"))
        return j({
          namespace: "",
          allowed: { agentdeployments: { create: true, update: true, delete: true } },
        });

      // Template gallery
      if (path === "/api/templates" && method === "GET") {
        return j({ templates: opts?.templates ?? [defaultRecipe, defaultPublished] });
      }

      // MCP catalog
      if (path === "/api/catalog" && method === "GET") {
        return j({ entries: opts?.catalogEntries ?? [defaultCatalogEntry] });
      }

      // T10: caller's owned servers list (used for already-connected cross-check)
      if (path === "/api/mcpservers" && method === "GET") {
        return j({ items: opts?.ownedServers ?? [] });
      }

      // Fork
      if (path.endsWith("/fork") && method === "POST") {
        const status = opts?.forkStatus ?? 201;
        if (status >= 400) return j({ error: "not found" }, false, status);
        return j(
          opts?.forkResponse ?? {
            status: "forked",
            agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
            created: [],
            needsRebinding: [],
            unresolvedRefs: [],
          },
          true,
          status,
        );
      }

      // MCP connect
      if (path === "/api/mcp/connect" && method === "POST") {
        const status = opts?.connectStatus ?? 200;
        if (status >= 400) return j({ error: "fail" }, false, status);
        return j(opts?.connectResponse ?? { name: "scalekit-mcp", namespace: "my-ns" });
      }

      return j({}, false, 404);
    }),
  );
}

function renderPage(onLocation?: (loc: string, state?: unknown) => void) {
  return render(
    <MemoryRouter>
      <ToastProvider>
        {onLocation && (
          <Routes>
            <Route path="*" element={<LocationSpy onLocation={onLocation} />} />
          </Routes>
        )}
        <TemplateGalleryPage />
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

// ── Taxonomy contract (m76.1) ─────────────────────────────────────────────────
describe("TemplateGalleryPage — taxonomy (m76.1)", () => {
  it("Gallery MCP tab is labeled 'Shared servers', not 'MCP Servers'", () => {
    makeFetch();
    renderPage();
    const mcpTab = screen.getByTestId("gallery-tab-mcp");
    expect(mcpTab).toHaveTextContent("Shared servers");
    expect(mcpTab).not.toHaveTextContent("MCP Servers");
  });
});

// ── Template tab ─────────────────────────────────────────────────────────────
describe("TemplateGalleryPage — templates tab", () => {
  it("renders the template gallery header and two tabs", async () => {
    makeFetch();
    renderPage();
    expect(screen.getByText("Gallery")).toBeInTheDocument();
    expect(screen.getByTestId("gallery-tab-templates")).toBeInTheDocument();
    expect(screen.getByTestId("gallery-tab-mcp")).toBeInTheDocument();
  });

  it("renders recipe and published agent template cards", async () => {
    makeFetch();
    renderPage();

    // Recipe card
    expect(
      await screen.findByTestId(`template-card-${defaultRecipe.name}`),
    ).toBeInTheDocument();
    expect(screen.getByTestId(`template-name-${defaultRecipe.name}`)).toHaveTextContent(
      defaultRecipe.name,
    );
    expect(screen.getByTestId(`template-source-${defaultRecipe.name}`)).toHaveTextContent(
      "built-in",
    );

    // Published agent card
    expect(screen.getByTestId(`template-card-${defaultPublished.name}`)).toBeInTheDocument();
    expect(screen.getByTestId(`template-source-${defaultPublished.name}`)).toHaveTextContent(
      "published",
    );
  });

  it("shows the origin provenance for a published template", async () => {
    makeFetch();
    renderPage();
    const originEl = await screen.findByTestId(`template-origin-${defaultPublished.name}`);
    expect(originEl).toHaveTextContent("prod/support-agent");
  });

  // U16: a template the caller already forked is pre-marked with a badge that LINKS to their fork,
  // and the Fork button is demoted (but stays enabled — re-forking under a new name is allowed).
  it("pre-marks an already-forked template with a link to the fork (U16)", async () => {
    makeFetch({
      templates: [
        {
          ...defaultPublished,
          alreadyForkedAs: { namespace: "team-a", name: "my-support-copy" },
        },
      ],
    });
    renderPage();

    const badge = await screen.findByTestId(
      `template-already-forked-${defaultPublished.name}`,
    );
    expect(badge).toHaveTextContent(/Already forked/i);
    // The badge links to the caller's fork, not the origin.
    expect(badge).toHaveAttribute(
      "href",
      "/agents/team-a/my-support-copy",
    );
    // Fork stays available (re-fork under a new name is legit).
    expect(
      screen.getByTestId(`fork-template-${defaultPublished.name}`),
    ).toBeEnabled();
  });

  it("does NOT mark an un-forked template", async () => {
    makeFetch(); // defaultPublished has no alreadyForkedAs
    renderPage();
    await screen.findByTestId(`template-card-${defaultPublished.name}`);
    expect(
      screen.queryByTestId(`template-already-forked-${defaultPublished.name}`),
    ).toBeNull();
  });

  it("recipe Install navigates to /agents/new?recipe=<name> (not ?spec=)", async () => {
    makeFetch();
    let lastLocation = "";
    renderPage((loc) => { lastLocation = loc; });

    const forkBtn = await screen.findByTestId(`fork-template-${defaultRecipe.name}`);
    expect(forkBtn).toHaveTextContent("Install");
    fireEvent.click(forkBtn);

    // Navigates with ?recipe=<name>, not a fragile ?spec= blob.
    await waitFor(() => {
      expect(lastLocation).toContain("/agents/new");
      expect(lastLocation).toContain(`recipe=${encodeURIComponent(defaultRecipe.name)}`);
      // Must NOT carry the raw spec in the URL (that path is buggy and was replaced).
      expect(lastLocation).not.toContain("spec=");
    });
    // No error toast.
    expect(screen.queryByText("Fork failed")).toBeNull();
  });

  it("published agent fork navigates to the FORK's namespace/name, not the origin's", async () => {
    // The fork response carries agent.namespace="my-ns" (the caller's ns), NOT "prod" (the origin).
    makeFetch({
      templates: [defaultPublished],
      forkStatus: 201,
      forkResponse: {
        status: "forked",
        agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
        created: [],
        needsRebinding: [],
        unresolvedRefs: [],
      },
    });
    let lastLocation = "";
    renderPage((loc) => { lastLocation = loc; });

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    await waitFor(() => {
      // Must navigate to the FORK's coordinates (my-ns/support-agent), not the origin's (prod/…).
      expect(lastLocation).toBe("/agents/my-ns/support-agent");
    });
  });

  it("published agent fork calls the fork API and shows success toast", async () => {
    const calls: { url: string; method: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const path = url.split("?")[0];
        const method = init?.method ?? "GET";
        calls.push({ url: path, method });
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({
            ok,
            status,
            json: async () => body,
            text: async () => JSON.stringify(body),
          } as Response);
        if (path === "/api/templates") return j({ templates: [defaultPublished] });
        if (path === "/api/catalog") return j({ entries: [] });
        if (path === "/api/mcpservers") return j({ items: [] });
        if (path.endsWith("/fork") && method === "POST")
          return j({
            status: "forked",
            agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
            created: [],
            needsRebinding: [],
            unresolvedRefs: [],
          }, true, 201);
        return j({}, false, 404);
      }),
    );

    renderPage();

    const forkBtn = await screen.findByTestId(`fork-template-${defaultPublished.name}`);
    expect(forkBtn).toHaveTextContent("Fork");
    fireEvent.click(forkBtn);

    await waitFor(() => {
      expect(calls.some((c) => c.url.endsWith("/fork") && c.method === "POST")).toBe(true);
    });
    await waitFor(() => {
      expect(screen.getByText("Forked")).toBeInTheDocument();
    });
  });

  it("shows needs-attention toast when fork returns dangling refs", async () => {
    makeFetch({
      templates: [defaultPublished],
      forkStatus: 201,
      forkResponse: {
        status: "forked",
        agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
        created: [],
        needsRebinding: ["model-route"],
        unresolvedRefs: ["tools/my-tool"],
      },
    });

    renderPage();

    const forkBtn = await screen.findByTestId(`fork-template-${defaultPublished.name}`);
    fireEvent.click(forkBtn);

    await waitFor(() => {
      expect(screen.getByText(/Forked — needs attention/)).toBeInTheDocument();
    });
    expect(screen.getByText(/model-route/)).toBeInTheDocument();
  });

  it("shows an honest 404 error when the fork API returns not-found", async () => {
    makeFetch({ templates: [defaultPublished], forkStatus: 404 });
    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));
    await waitFor(() => {
      expect(screen.getByText("Fork failed")).toBeInTheDocument();
    });
    expect(screen.getByText(/no longer discoverable/)).toBeInTheDocument();
  });

  it("shows rename-on-fork dialog on 409 name collision (U11 — no dead-end error)", async () => {
    // U11: a 409 collision no longer shows a dead-end "Fork failed" toast — it opens
    // the rename dialog so the user can retry with a different name.
    makeFetch({ templates: [defaultPublished], forkStatus: 409 });
    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));
    await waitFor(() => {
      expect(screen.getByTestId("rename-fork-dialog")).toBeInTheDocument();
    });
    // No dead-end "Fork failed" error toast.
    expect(screen.queryByText("Fork failed")).toBeNull();
  });

  it("renders empty state when no templates are returned", async () => {
    makeFetch({ templates: [] });
    renderPage();
    expect(await screen.findByText("No templates yet")).toBeInTheDocument();
  });
});

// ── MCP catalog tab ───────────────────────────────────────────────────────────
describe("TemplateGalleryPage — MCP catalog tab", () => {
  it("switches to the MCP tab and renders catalog entries", async () => {
    makeFetch();
    renderPage();

    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    expect(
      await screen.findByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`),
    ).toBeInTheDocument();
    expect(screen.getByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`)).toBeInTheDocument();
  });

  it("MCP connect calls the connect API and shows success toast", async () => {
    const calls: { url: string; method: string }[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const path = url.split("?")[0];
        const method = init?.method ?? "GET";
        calls.push({ url: path, method });
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({
            ok,
            status,
            json: async () => body,
            text: async () => JSON.stringify(body),
          } as Response);
        if (path === "/api/templates") return j({ templates: [] });
        if (path === "/api/catalog") return j({ entries: [defaultCatalogEntry] });
        if (path === "/api/mcpservers") return j({ items: [] });
        if (path === "/api/mcp/connect" && method === "POST")
          return j({ name: "scalekit-mcp", namespace: "my-ns" });
        return j({}, false, 404);
      }),
    );

    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    const connectBtn = await screen.findByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`);
    fireEvent.click(connectBtn);

    await waitFor(() => {
      expect(calls.some((c) => c.url === "/api/mcp/connect" && c.method === "POST")).toBe(true);
    });
    await waitFor(() => {
      expect(screen.getByText("Connected")).toBeInTheDocument();
    });
  });

  it("shows empty state when MCP catalog has no entries", async () => {
    makeFetch({ catalogEntries: [] });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));
    expect(await screen.findByText("No discoverable servers yet")).toBeInTheDocument();
  });
});

// ── T8: CredentialSourceBadge in the catalog tab ─────────────────────────────
describe("TemplateGalleryPage MCP catalog — CredentialSourceBadge (m76.2 T8)", () => {
  it("renders 'You connect your account' for byo-oauth catalog entries", async () => {
    makeFetch({
      catalogEntries: [{ ...defaultCatalogEntry, credentialSource: "byo-oauth" }],
    });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    const badge = await screen.findByTestId(`cred-source-${defaultCatalogEntry.name}`);
    expect(badge).toHaveTextContent("You connect your account");
  });

  it("renders 'Uses a shared credential' for shared catalog entries", async () => {
    makeFetch({
      catalogEntries: [{ ...defaultCatalogEntry, credentialSource: "shared" }],
    });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    const badge = await screen.findByTestId(`cred-source-${defaultCatalogEntry.name}`);
    expect(badge).toHaveTextContent("Uses a shared credential");
  });

  it("hides the badge when credentialSource is absent", async () => {
    makeFetch({
      catalogEntries: [{ ...defaultCatalogEntry, credentialSource: undefined }],
    });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    await screen.findByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`);
    expect(screen.queryByTestId(`cred-source-${defaultCatalogEntry.name}`)).toBeNull();
  });
});

// ── T10: already-connected disabled state ─────────────────────────────────────
describe("TemplateGalleryPage MCP catalog — already-connected (m76.2 T10)", () => {
  it("shows 'Connected ✓' disabled button for an entry already owned in caller's namespace", async () => {
    // The catalog entry and the owned server have the same ns/name.
    makeFetch({
      catalogEntries: [defaultCatalogEntry],
      ownedServers: [
        {
          name: defaultCatalogEntry.name,
          namespace: defaultCatalogEntry.namespace,
          url: "https://mcp.example.com",
          toolCount: 10,
          status: "approved",
        },
      ],
    });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    await screen.findByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`);

    // The Connect button is disabled.
    const connectBtn = screen.getByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`);
    expect(connectBtn).toBeDisabled();
    expect(connectBtn).toHaveTextContent("Connected");

    // The "Connected ✓" badge also renders.
    expect(screen.getByTestId(`mcp-catalog-connected-${defaultCatalogEntry.name}`)).toBeInTheDocument();
  });

  it("shows an active Connect button for entries NOT in the caller's namespace", async () => {
    // Owned server has a different name — no match.
    makeFetch({
      catalogEntries: [defaultCatalogEntry],
      ownedServers: [
        {
          name: "other-mcp",
          namespace: "platform",
          url: "https://other.example.com",
          toolCount: 5,
          status: "approved",
        },
      ],
    });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    await screen.findByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`);
    const connectBtn = screen.getByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`);
    expect(connectBtn).not.toBeDisabled();
    expect(connectBtn).toHaveTextContent("Connect");
  });
});

// ── T11: post-connect navigate with highlight state ───────────────────────────
describe("TemplateGalleryPage MCP catalog — post-connect highlight (m76.2 T11)", () => {
  it("navigates to /tools/mcp-servers with location.state.highlight after connect", async () => {
    makeFetch({
      catalogEntries: [defaultCatalogEntry],
      connectResponse: { name: "scalekit-mcp", namespace: "my-ns" },
    });
    let lastPath = "";
    let lastState: unknown = null;
    renderPage((loc, state) => {
      lastPath = loc;
      lastState = state;
    });
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    const connectBtn = await screen.findByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`);
    fireEvent.click(connectBtn);

    await waitFor(() => {
      expect(lastPath).toContain("/tools/mcp-servers");
    });
    expect((lastState as { highlight?: string })?.highlight).toBe("my-ns/scalekit-mcp");
  });
});

// ── T12: catalog search filter ────────────────────────────────────────────────
describe("TemplateGalleryPage MCP catalog — search filter (m76.2 T12)", () => {
  it("filters catalog entries by name when search input is typed", async () => {
    makeFetch({
      catalogEntries: [
        defaultCatalogEntry,
        { name: "other-mcp", namespace: "staging", toolCount: 3, visibility: "team" },
      ],
    });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    await screen.findByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`);
    await screen.findByTestId("mcp-catalog-entry-other-mcp");

    // Type in search box.
    const searchInput = screen.getByTestId("mcp-catalog-search");
    fireEvent.change(searchInput, { target: { value: "other" } });

    // Only "other-mcp" remains.
    expect(screen.queryByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`)).toBeNull();
    expect(screen.getByTestId("mcp-catalog-entry-other-mcp")).toBeInTheDocument();
  });

  it("shows a no-results message when search matches nothing", async () => {
    makeFetch({ catalogEntries: [defaultCatalogEntry] });
    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    await screen.findByTestId(`mcp-catalog-entry-${defaultCatalogEntry.name}`);
    const searchInput = screen.getByTestId("mcp-catalog-search");
    fireEvent.change(searchInput, { target: { value: "zzznomatch" } });

    expect(await screen.findByTestId("mcp-catalog-no-results")).toBeInTheDocument();
  });

  it("shows 'Connecting…' label on the button while connect is in flight", async () => {
    // Use a never-resolving connect to keep the in-flight state visible.
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const path = url.split("?")[0];
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);

        if (path === "/api/templates") return j({ templates: [] });
        if (path === "/api/catalog") return j({ entries: [defaultCatalogEntry] });
        if (path === "/api/mcpservers") return j({ items: [] });
        // Never resolve the connect — keeps the button in "Connecting…" state.
        if (path === "/api/mcp/connect" && method === "POST") return new Promise(() => {});
        return j({}, false, 404);
      }),
    );

    renderPage();
    fireEvent.click(screen.getByTestId("gallery-tab-mcp"));

    const connectBtn = await screen.findByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`);
    fireEvent.click(connectBtn);

    // The button should show "Connecting…" and be disabled while the request is pending.
    await waitFor(() => {
      expect(screen.getByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`)).toHaveTextContent("Connecting…");
    });
    expect(screen.getByTestId(`connect-mcp-tab-${defaultCatalogEntry.name}`)).toBeDisabled();
  });
});

// ── Helpers for capability-aware rendering ────────────────────────────────────
// renderPageWithCaps wraps the page in CapabilitiesProvider + NamespaceProvider
// so that capability-gated UI (U10 etc.) is testable with real RBAC data.
function renderPageWithCaps(onLocation?: (loc: string, state?: unknown) => void) {
  return render(
    <MemoryRouter>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <ToastProvider>
            {onLocation && (
              <Routes>
                <Route path="*" element={<LocationSpy onLocation={onLocation} />} />
              </Routes>
            )}
            <TemplateGalleryPage />
          </ToastProvider>
        </CapabilitiesProvider>
      </NamespaceProvider>
    </MemoryRouter>,
  );
}

// ── U9: resolvedRefs celebrated in fork success toast ────────────────────────
describe("TemplateGalleryPage (m76.3 U9) — resolvedRefs auto-connect toast", () => {
  it("shows 'N tools connected automatically' toast when resolvedRefs is non-empty (U9)", async () => {
    makeFetch({
      templates: [defaultPublished],
      forkStatus: 201,
      forkResponse: {
        status: "forked",
        agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
        created: [],
        needsRebinding: [],
        unresolvedRefs: [],
        resolvedRefs: ["search", "summarize"],
      },
    });

    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    await waitFor(() => {
      expect(screen.getByText("Forked")).toBeInTheDocument();
    });
    // U9: celebrate the auto-connected tools.
    expect(screen.getByText(/2 tools connected automatically/)).toBeInTheDocument();
  });

  it("mentions resolved tools in needs-attention toast when there are still dangling refs (U9)", async () => {
    makeFetch({
      templates: [defaultPublished],
      forkStatus: 201,
      forkResponse: {
        status: "forked",
        agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
        created: [],
        needsRebinding: ["model route: gpt4-prod"],
        unresolvedRefs: [],
        resolvedRefs: ["search"],
      },
    });

    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    await waitFor(() => {
      expect(screen.getByText(/Forked — needs attention/)).toBeInTheDocument();
    });
    // U9: the resolved tools note appears alongside the dangling refs.
    expect(screen.getByText(/1 tool connected automatically/)).toBeInTheDocument();
  });
});

// ── U10: display-gate Fork button on create capability ────────────────────────
describe("TemplateGalleryPage (m76.3 U10) — Fork button capability gate", () => {
  it("Fork button is disabled for a viewer without agent-create rights (U10)", async () => {
    // Stub capabilities to deny create.
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const path = url.split("?")[0];
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);

        if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (path.startsWith("/api/capabilities"))
          return j({
            namespace: "",
            // Viewer: create is false.
            allowed: { agentdeployments: { create: false, update: false, delete: false } },
          });
        if (path === "/api/templates" && method === "GET")
          return j({ templates: [defaultPublished] });
        if (path === "/api/catalog") return j({ entries: [] });
        if (path === "/api/mcpservers") return j({ items: [] });
        return j({}, false, 404);
      }),
    );

    renderPageWithCaps();

    const forkBtn = await screen.findByTestId(`fork-template-${defaultPublished.name}`);
    // U10: button is disabled (can't fork without create rights).
    expect(forkBtn).toBeDisabled();
    // U10: specific tooltip / title.
    expect(forkBtn).toHaveAttribute("title", "You need agent-create rights to fork");
  });

  it("Fork button is enabled for a user with agent-create rights (U10)", async () => {
    makeFetch({ templates: [defaultPublished] });
    renderPage();

    const forkBtn = await screen.findByTestId(`fork-template-${defaultPublished.name}`);
    expect(forkBtn).not.toBeDisabled();
  });
});

// ── U11: rename-on-fork + already-forked link ──────────────────────────────────
describe("TemplateGalleryPage (m76.3 U11) — rename-on-fork + already-forked link", () => {
  it("shows rename dialog on 409 collision (U11)", async () => {
    makeFetch({ templates: [defaultPublished], forkStatus: 409 });
    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    // U11: rename dialog should appear instead of a dead-end error toast.
    await waitFor(() => {
      expect(screen.getByTestId("rename-fork-dialog")).toBeInTheDocument();
    });
    expect(screen.queryByText("Fork failed")).toBeNull();
  });

  it("retries the fork with the new name from the rename dialog (U11)", async () => {
    const calls: { url: string; method: string; body: string }[] = [];
    let callCount = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const path = url.split("?")[0];
        const method = init?.method ?? "GET";
        const body = typeof init?.body === "string" ? init.body : "";
        calls.push({ url: path, method, body });
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);

        if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (path.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true } } });
        if (path === "/api/templates" && method === "GET")
          return j({ templates: [defaultPublished] });
        if (path === "/api/catalog") return j({ entries: [] });
        if (path === "/api/mcpservers") return j({ items: [] });
        if (path.endsWith("/fork") && method === "POST") {
          callCount++;
          if (callCount === 1) {
            // First fork → 409 collision.
            return j({ error: "collision" }, false, 409);
          }
          // Second fork (with rename) → success.
          return j({
            status: "forked",
            agent: { name: "support-agent-copy", namespace: "my-ns", image: "", phase: "Ready", ready: true },
            created: [],
            needsRebinding: [],
            unresolvedRefs: [],
          }, true, 201);
        }
        return j({}, false, 404);
      }),
    );

    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    // Wait for rename dialog.
    await waitFor(() => expect(screen.getByTestId("rename-fork-dialog")).toBeInTheDocument());

    // Clear the default and type a new name.
    const input = screen.getByTestId("rename-fork-input");
    fireEvent.change(input, { target: { value: "my-custom-fork" } });
    fireEvent.click(screen.getByTestId("rename-fork-confirm"));

    // The second fork request must carry the new name.
    await waitFor(() => {
      const forkCalls = calls.filter((c) => c.url.endsWith("/fork") && c.method === "POST");
      expect(forkCalls.length).toBe(2);
      expect(JSON.parse(forkCalls[1].body).name).toBe("my-custom-fork");
    });
  });

  it("links to the existing fork on already-forked (U11)", async () => {
    makeFetch({
      templates: [defaultPublished],
      forkStatus: 200,
      forkResponse: {
        status: "already-forked",
        agent: { name: "support-agent", namespace: "my-ns", image: "", phase: "Ready", ready: true },
        created: [],
        needsRebinding: [],
        unresolvedRefs: [],
      },
    });
    let lastLocation = "";
    renderPage((loc) => { lastLocation = loc; });

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    // U11: navigate to the existing fork.
    await waitFor(() => {
      expect(lastLocation).toBe("/agents/my-ns/support-agent");
    });
    // U11: toast says "Opening it now" not a dead-end message.
    expect(screen.getByText(/Already forked/)).toBeInTheDocument();
    expect(screen.getByText(/Opening it now/)).toBeInTheDocument();
  });
});

// ── U12: per-entry spinner, key fix, verb+icon differentiation ─────────────────
describe("TemplateGalleryPage (m76.3 U12) — per-entry spinner, key, verb/icon", () => {
  it("shows 'Install' verb for recipe and 'Fork' for published (U12)", async () => {
    makeFetch({ templates: [defaultRecipe, defaultPublished] });
    renderPage();

    // Recipe button says "Install".
    const installBtn = await screen.findByTestId(`fork-template-${defaultRecipe.name}`);
    expect(installBtn).toHaveTextContent("Install");

    // Published button says "Fork".
    const forkBtn = screen.getByTestId(`fork-template-${defaultPublished.name}`);
    expect(forkBtn).toHaveTextContent("Fork");
  });

  it("shows per-entry Forking… spinner while the entry's fork is in flight (U12)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL, init?: RequestInit) => {
        const url = typeof input === "string" ? input : input.toString();
        const path = url.split("?")[0];
        const method = init?.method ?? "GET";
        const j = (body: unknown, ok = true, status = 200) =>
          Promise.resolve({ ok, status, json: async () => body, text: async () => JSON.stringify(body) } as Response);

        if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
        if (path.startsWith("/api/capabilities"))
          return j({ namespace: "", allowed: { agentdeployments: { create: true, update: true, delete: true } } });
        if (path === "/api/templates") return j({ templates: [defaultPublished] });
        if (path === "/api/catalog") return j({ entries: [] });
        if (path === "/api/mcpservers") return j({ items: [] });
        // Never resolve the fork — keeps it in "Forking…" state.
        if (path.endsWith("/fork") && method === "POST") return new Promise(() => {});
        return j({}, false, 404);
      }),
    );

    renderPage();
    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));

    await waitFor(() => {
      expect(screen.getByTestId(`fork-template-${defaultPublished.name}`)).toHaveTextContent("Forking…");
    });
    expect(screen.getByTestId(`fork-template-${defaultPublished.name}`)).toBeDisabled();
  });
});

// ── P1-3: ?tab= deep-link opens the correct tab ───────────────────────────────

function renderPageAt(path: string, onLocation?: (loc: string, state?: unknown) => void) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        {onLocation && (
          <Routes>
            <Route path="*" element={<LocationSpy onLocation={onLocation} />} />
          </Routes>
        )}
        <TemplateGalleryPage />
      </ToastProvider>
    </MemoryRouter>,
  );
}

describe("TemplateGalleryPage — P1-3 ?tab= deep-link (m76 close)", () => {
  it("?tab=mcp opens the Shared-servers (MCP) tab", () => {
    makeFetch();
    renderPageAt("/gallery?tab=mcp");

    // The MCP tab should be active (aria-selected=true).
    expect(screen.getByTestId("gallery-tab-mcp")).toHaveAttribute("aria-selected", "true");
    expect(screen.getByTestId("gallery-tab-templates")).toHaveAttribute("aria-selected", "false");
  });

  it("no ?tab= param defaults to the Agent Templates tab", () => {
    makeFetch();
    renderPageAt("/gallery");

    expect(screen.getByTestId("gallery-tab-templates")).toHaveAttribute("aria-selected", "true");
    expect(screen.getByTestId("gallery-tab-mcp")).toHaveAttribute("aria-selected", "false");
  });

  it("?tab=templates opens the Agent Templates tab explicitly", () => {
    makeFetch();
    renderPageAt("/gallery?tab=templates");

    expect(screen.getByTestId("gallery-tab-templates")).toHaveAttribute("aria-selected", "true");
    expect(screen.getByTestId("gallery-tab-mcp")).toHaveAttribute("aria-selected", "false");
  });
});
