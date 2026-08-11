import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { TemplateGalleryPage } from "@/pages/template-gallery-page";
import { ToastProvider } from "@/components/kit";

// LocationSpy captures the last navigation target so tests can assert routing.
function LocationSpy({ onLocation }: { onLocation: (loc: string) => void }) {
  const loc = useLocation();
  onLocation(loc.pathname + loc.search);
  return null;
}

// Fake fetch for the template gallery page. Covers:
//   - GET /api/templates  — the template list (recipes + published agents)
//   - GET /api/catalog    — the MCP catalog tab
//   - POST /api/agents/.../fork — the fork action

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
}

interface CatalogRow {
  name: string;
  namespace: string;
  toolCount: number;
  authType?: string;
  visibility: string;
  description?: string;
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
        return j({ name: "scalekit-mcp", namespace: "my-ns" });
      }

      return j({}, false, 404);
    }),
  );
}

function renderPage(onLocation?: (loc: string) => void) {
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

  it("shows 409 conflict message when fork returns name collision", async () => {
    makeFetch({ templates: [defaultPublished], forkStatus: 409 });
    renderPage();

    fireEvent.click(await screen.findByTestId(`fork-template-${defaultPublished.name}`));
    await waitFor(() => {
      expect(screen.getByText("Fork failed")).toBeInTheDocument();
    });
    expect(screen.getByText(/already exists.*different origin/i)).toBeInTheDocument();
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
