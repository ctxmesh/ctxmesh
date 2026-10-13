import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { ToastProvider } from "@/components/kit";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { McpServersPage } from "@/pages/mcp-servers-page";

// MCP servers holds the servers (the default tab) and the tool catalog they feed
// (?tab=tools). Each tab's own behaviour is covered by mcp-servers-page.test.tsx and
// tool-catalog.test.tsx; this is the page around them.

const SERVER = {
  name: "search-mcp",
  namespace: "team-a",
  url: "https://search.example/mcp",
  status: "approved",
  toolCount: 1,
};
const TOOL = { name: "code-search", description: "Search code", registry: "search-mcp", source: "user-added", namespace: "team-a" };

function installFetch() {
  const urls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      urls.push(url);
      const ok = (body: unknown) =>
        Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => "" } as Response);
      const path = url.split("?")[0];
      if (path === "/api/namespaces") return ok({ namespaces: [{ name: "team-a" }] });
      if (path === "/api/capabilities") {
        return ok({
          namespace: "team-a",
          allowed: { agentregistries: { create: true, update: true, delete: true }, mcptoolbindings: { create: true } },
          flows: {},
        });
      }
      if (path === "/api/mcpservers") return ok({ items: [SERVER], servers: [SERVER] });
      if (path === "/api/tools") return ok({ tools: [TOOL] });
      return ok({ items: [] });
    }),
  );
  return urls;
}

function Where() {
  const { pathname, search } = useLocation();
  return <div data-testid="where">{pathname + search}</div>;
}

function renderAt(path: string) {
  localStorage.setItem("ctxmesh.namespace", "team-a");
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="/tools/mcp-servers" element={<McpServersPage />} />
            </Routes>
            <Where />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  localStorage.clear();
});

describe("the MCP servers page's tabs", () => {
  it("opens on the servers, with Add MCP server, and does not read the catalog", async () => {
    const urls = installFetch();
    renderAt("/tools/mcp-servers");
    expect(await screen.findByTestId("mcp-server-search-mcp")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Servers" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("button", { name: "Add MCP server" })).toBeInTheDocument();
    expect(urls.some((u) => u.startsWith("/api/tools"))).toBe(false);
  });

  it("shows the tool catalog on its tab, under the same one header", async () => {
    const urls = installFetch();
    renderAt("/tools/mcp-servers");
    fireEvent.click(await screen.findByRole("tab", { name: "Tools" }));
    expect(await screen.findByTestId("catalog-tool-code-search")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/tools/mcp-servers?tab=tools");
    expect(urls).toContain("/api/tools?namespace=team-a");
    expect(screen.getAllByRole("heading", { level: 1 }).map((h) => h.textContent)).toEqual(["MCP servers"]);
    // Binding a tool still opens from its card.
    fireEvent.click(screen.getByTestId("catalog-bind-code-search"));
    expect(await screen.findByTestId("bind-tool-wizard")).toBeInTheDocument();
  });

  it("takes a server's drawer straight to the tools it feeds", async () => {
    installFetch();
    renderAt("/tools/mcp-servers");
    fireEvent.click(await screen.findByTestId("mcp-server-search-mcp"));
    const drawer = await screen.findByTestId("mcp-server-drawer");
    expect(drawer).toBeInTheDocument();
    const browse = screen.getByRole("link", { name: /Browse its tools/ });
    expect(browse).toHaveAttribute("href", "/tools/mcp-servers?tab=tools");
    fireEvent.click(browse);
    expect(await screen.findByTestId("tool-catalog")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByTestId("mcp-server-drawer")).toBeNull());
  });

  it("opens the catalog from a link to its tab", async () => {
    installFetch();
    renderAt("/tools/mcp-servers?tab=tools");
    const catalog = await screen.findByTestId("tool-catalog");
    expect(await within(catalog).findByTestId("catalog-tool-code-search")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Tools" })).toHaveAttribute("aria-selected", "true");
  });
});
