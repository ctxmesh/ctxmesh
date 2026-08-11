import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { McpCatalogPage } from "@/pages/mcp-catalog-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// Fake BFF for McpCatalogPage tests (m73.7). Matches the fake-BFF pattern
// from mcp-servers-page.test.tsx: vi.stubGlobal fetch, answer /api/catalog,
// /api/mcp/connect, and the shell endpoints (/api/capabilities, /api/namespaces).

const defaultEntry = {
  name: "acme-mcp",
  namespace: "shared",
  url: "https://mcp.acme.dev/sse",
  description: "Acme tools",
  toolCount: 5,
  authType: "oauth",
  visibility: "org",
  credentialSource: "shared",
};

function fakeFetch(opts?: {
  entries?: typeof defaultEntry[];
  connectStatus?: number;
  connectBody?: unknown;
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

      if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (path.startsWith("/api/capabilities"))
        return j({
          namespace: "",
          allowed: { agentregistries: { create: true } },
        });
      if (path === "/api/catalog" && method === "GET")
        return j({ entries: opts?.entries ?? [defaultEntry] });
      if (path === "/api/mcp/connect" && method === "POST") {
        const status = opts?.connectStatus ?? 200;
        if (status >= 400) return j({ error: "not found" }, false, status);
        return j(opts?.connectBody ?? { name: "acme-mcp", namespace: "default" });
      }
      return j({}, false, 404);
    }),
  );
}

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <McpCatalogPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("McpCatalogPage (m73.7)", () => {
  it("renders catalog entries with visibility badge and tool count", async () => {
    fakeFetch();
    renderPage();

    expect(await screen.findByTestId("catalog-entry-acme-mcp")).toBeInTheDocument();
    expect(screen.getByTestId("catalog-entry-name-acme-mcp")).toHaveTextContent("acme-mcp");
    // visibility badge text
    expect(screen.getByText("org")).toBeInTheDocument();
    // tool count badge
    expect(screen.getByText("5 tools")).toBeInTheDocument();
  });

  it("shows empty state when catalog is empty", async () => {
    fakeFetch({ entries: [] });
    renderPage();

    expect(await screen.findByText(/No discoverable servers yet/)).toBeInTheDocument();
  });

  it("calls connectMcpServer and shows success toast on Connect click", async () => {
    fakeFetch({ connectBody: { name: "acme-mcp", namespace: "default" } });
    renderPage();

    const btn = await screen.findByTestId("connect-entry-acme-mcp");
    fireEvent.click(btn);

    await waitFor(() => {
      expect(screen.getByText(/Connected/)).toBeInTheDocument();
    });
  });

  it("shows already-connected note when status is already-connected", async () => {
    fakeFetch({ connectBody: { status: "already-connected" } });
    renderPage();

    fireEvent.click(await screen.findByTestId("connect-entry-acme-mcp"));

    await waitFor(() => {
      expect(screen.getByText(/Already connected/)).toBeInTheDocument();
    });
  });

  it("shows error toast on connect failure (404)", async () => {
    fakeFetch({ connectStatus: 404 });
    renderPage();

    fireEvent.click(await screen.findByTestId("connect-entry-acme-mcp"));

    await waitFor(() => {
      expect(screen.getByText(/Connect failed/)).toBeInTheDocument();
    });
  });

  it("renders description and namespace for each entry", async () => {
    fakeFetch();
    renderPage();

    await screen.findByTestId("catalog-entry-acme-mcp");
    expect(screen.getByText("Acme tools")).toBeInTheDocument();
    // namespace "shared" appears in the namespace line
    expect(screen.getByText(/namespace:/)).toBeInTheDocument();
  });

  it("byo-oauth connect toast mentions connecting your own account (P1-2)", async () => {
    const byoEntry = { ...defaultEntry, credentialSource: "byo-oauth" };
    fakeFetch({ entries: [byoEntry], connectBody: { name: "acme-mcp", namespace: "default" } });
    renderPage();

    fireEvent.click(await screen.findByTestId("connect-entry-acme-mcp"));

    await waitFor(() => {
      expect(screen.getByText(/You'll connect your own account/)).toBeInTheDocument();
      expect(screen.getByText(/publisher's credentials are never shared/)).toBeInTheDocument();
    });
  });

  it("non-byo-oauth connect toast uses shorter message (P1-2)", async () => {
    // defaultEntry has credentialSource: "shared" — should use the short toast
    fakeFetch({ connectBody: { name: "acme-mcp", namespace: "default" } });
    renderPage();

    fireEvent.click(await screen.findByTestId("connect-entry-acme-mcp"));

    await waitFor(() => {
      expect(screen.getByText(/is now available in your MCP servers/)).toBeInTheDocument();
    });
  });
});
