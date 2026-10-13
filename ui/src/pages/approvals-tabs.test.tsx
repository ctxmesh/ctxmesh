import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

import { ToastProvider } from "@/components/kit";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ApprovalsPage } from "@/pages/approvals-page";

// Approvals holds both queues where a person must decide: runs (the default tab) and MCP
// servers awaiting an operator (?tab=mcp). The queues' own behaviour is covered by
// approvals-page.test.tsx and mcp-approvals-queue.test.tsx; this is the page around them.

function installFetch(registries: { update: boolean }) {
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
          allowed: { agentregistries: { update: registries.update }, workflows: { list: true } },
          flows: {},
        });
      }
      if (path === "/api/approvals") return ok([]);
      if (path === "/api/mcp/approvals") {
        const pending = [{ namespace: "team-a", name: "acme-mcp", toolCount: 2 }];
        return ok({ items: pending, servers: pending });
      }
      return ok({});
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
              <Route path="/approvals" element={<ApprovalsPage />} />
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

describe("the Approvals page's tabs", () => {
  it("opens on the run queue, with the MCP queue one tab away for an operator", async () => {
    const urls = installFetch({ update: true });
    renderAt("/approvals");
    expect(await screen.findByTestId("approvals-page")).toBeInTheDocument();
    await waitFor(() => expect(urls).toContain("/api/approvals?namespace=team-a"));
    expect(screen.getByRole("heading", { level: 1, name: "Approvals" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "Runs" })).toHaveAttribute("aria-selected", "true");
    expect(await screen.findByRole("tab", { name: "MCP servers" })).toHaveAttribute("aria-selected", "false");
    expect(urls.some((u) => u.startsWith("/api/mcp/approvals"))).toBe(false);
  });

  it("switches to the MCP queue in place, and the URL names the tab", async () => {
    const urls = installFetch({ update: true });
    renderAt("/approvals");
    fireEvent.click(await screen.findByRole("tab", { name: "MCP servers" }));
    expect(await screen.findByTestId("mcp-approvals")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent("/approvals?tab=mcp");
    expect(await screen.findByTestId("mcp-approve-team-a-acme-mcp")).toBeInTheDocument();
    expect(urls).toContain("/api/mcp/approvals?namespace=team-a");
    // One header for the page: the queue's own title is gone, its count and refresh are not.
    expect(screen.getAllByRole("heading", { level: 1 }).map((h) => h.textContent)).toEqual(["Approvals"]);
    expect(screen.getByTestId("mcp-approvals-refresh")).toBeInTheDocument();
    expect(screen.getByTestId("mcp-approvals-waiting-count")).toHaveTextContent("1 awaiting a person");

    fireEvent.click(screen.getByRole("tab", { name: "Runs" }));
    expect(await screen.findByTestId("approvals-page")).toBeInTheDocument();
    expect(screen.getByTestId("where")).toHaveTextContent(/^\/approvals$/);
  });

  it("shows no MCP tab to a caller who cannot decide one", async () => {
    installFetch({ update: false });
    renderAt("/approvals");
    expect(await screen.findByTestId("approvals-page")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("tab", { name: "MCP servers" })).toBeNull());
  });

  it("still opens the MCP queue from a link for that caller, read-only", async () => {
    installFetch({ update: false });
    renderAt("/approvals?tab=mcp");
    expect(await screen.findByTestId("mcp-review-team-a-acme-mcp")).toBeInTheDocument();
    expect(screen.queryByTestId("mcp-approve-team-a-acme-mcp")).toBeNull();
    expect(screen.getByRole("tab", { name: "MCP servers" })).toHaveAttribute("aria-selected", "true");
  });

  it("reads an unknown tab as the run queue", async () => {
    installFetch({ update: true });
    renderAt("/approvals?tab=nonsense");
    expect(await screen.findByTestId("approvals-page")).toBeInTheDocument();
    expect(screen.queryByTestId("mcp-approvals")).toBeNull();
  });
});
