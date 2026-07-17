import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { McpServersPage } from "@/pages/mcp-servers-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// A recording fetch mock for the MCP servers page: it answers /api/capabilities
// (delete allowed by default), /api/mcpservers (the list — re-fetched after a delete),
// the per-server references (delete-impact), and the DELETE. Tests assert the delete
// flow: button → dialog + impact → typed-name confirm → DELETE → reload.
interface Captured {
  url: string;
  method: string;
}

function recordingFetch(opts?: {
  servers?: McpRow[];
  references?: { kind: string; name: string; agentRef: string }[];
  caps?: Record<string, Record<string, boolean>>;
  deleteStatus?: number;
}) {
  const calls: Captured[] = [];
  // The list flips to empty after a successful delete so the reload is observable.
  let deleted = false;
  const servers = opts?.servers ?? [defaultRow];
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

      if (path.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (path.startsWith("/api/capabilities")) {
        return j({
          namespace: "",
          allowed: opts?.caps ?? {
            agentregistries: { create: true, update: true, delete: true },
          },
        });
      }
      if (path === "/api/mcp/org-credential" && method === "POST") {
        return j({ status: "org-credential-set", server: "scalekit-mcp-server", namespace: "prod" });
      }
      if (path.endsWith("/references")) {
        return j({ references: opts?.references ?? [], bindingCount: (opts?.references ?? []).length });
      }
      if (path === "/api/mcpservers" && method === "GET") {
        return j({ items: deleted ? [] : servers });
      }
      if (path.startsWith("/api/mcpservers/") && method === "DELETE") {
        const status = opts?.deleteStatus ?? 200;
        if (status >= 400) return j({ error: "forbidden" }, false, status);
        deleted = true;
        return j({ deleted: ["ToolRegistry", "Secret"], orphanedBindings: [] });
      }
      return j({}, false, 404);
    }),
  );
  return calls;
}

interface McpRow {
  name: string;
  namespace: string;
  url: string;
  toolCount: number;
  status: string;
  authType?: string;
  scope?: string;
}
const defaultRow: McpRow = {
  name: "scalekit-mcp-server",
  namespace: "prod",
  url: "https://mcp.scalekit.com/",
  toolCount: 35,
  status: "approved",
  authType: "oauth",
  scope: "personal",
};

function renderPage() {
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <McpServersPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("McpServersPage delete (m26.4)", () => {
  it("deletes a server after a typed-name confirm, previewing the dependent bindings, then reloads", async () => {
    const calls = recordingFetch({
      references: [{ kind: "MCPToolBinding", name: "sk-agent-list-orgs", agentRef: "sk-agent" }],
    });
    renderPage();

    // The server row + its delete affordance render (canDelete from caps).
    const delBtn = await screen.findByTestId("delete-mcp-scalekit-mcp-server");
    fireEvent.click(delBtn);

    // The dialog previews the delete-impact (the dependent binding) loaded from references.
    expect(await screen.findByTestId("mcp-ref-sk-agent-list-orgs")).toBeInTheDocument();
    expect(calls.some((c) => c.url.endsWith("/references"))).toBe(true);

    // The Delete button is gated on typing the server name.
    const confirmInput = screen.getByPlaceholderText("scalekit-mcp-server");
    fireEvent.change(confirmInput, { target: { value: "scalekit-mcp-server" } });

    fireEvent.click(screen.getByRole("button", { name: /Delete server/ }));

    // It issued the DELETE and then re-listed (the row is gone).
    await waitFor(() => {
      expect(
        calls.some((c) => c.method === "DELETE" && c.url === "/api/mcpservers/prod/scalekit-mcp-server"),
      ).toBe(true);
    });
    await waitFor(() => {
      expect(screen.queryByTestId("mcp-server-scalekit-mcp-server")).toBeNull();
    });
    expect(screen.getByText(/No MCP servers yet/)).toBeInTheDocument();
  });

  it("shows the scope badge and promotes a server to org scope with a shared credential, then reloads", async () => {
    const calls = recordingFetch();
    renderPage();

    // The current scope is surfaced as a badge.
    expect(await screen.findByTestId("scope-scalekit-mcp-server")).toHaveTextContent("personal");

    // The org-credential (share) action opens a dialog with a credential input.
    fireEvent.click(screen.getByTestId("org-cred-scalekit-mcp-server"));
    const input = await screen.findByTestId("org-cred-input");
    expect(input).toHaveAttribute("type", "password"); // the secret is never a plain field
    fireEvent.change(input, { target: { value: "org-shared-token" } });

    fireEvent.click(screen.getByTestId("org-cred-submit"));

    await waitFor(() => {
      expect(calls.some((c) => c.method === "POST" && c.url === "/api/mcp/org-credential")).toBe(true);
    });
    // A reload follows (a second list GET after the initial one).
    await waitFor(() => {
      expect(calls.filter((c) => c.url === "/api/mcpservers" && c.method === "GET").length).toBeGreaterThanOrEqual(2);
    });
  });

  it("hides the delete affordance when the caller cannot delete registries", async () => {
    recordingFetch({ caps: { agentregistries: { create: true, delete: false } } });
    renderPage();

    // The row renders, but with no delete button.
    await screen.findByTestId("mcp-server-scalekit-mcp-server");
    expect(screen.queryByTestId("delete-mcp-scalekit-mcp-server")).toBeNull();
  });
});
