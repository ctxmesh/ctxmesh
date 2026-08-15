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
  publishStatus?: number;
  orgCredStatus?: number;
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
        const status = opts?.orgCredStatus ?? 200;
        if (status >= 400) return j({ error: "forbidden" }, false, status);
        return j({ status: "org-credential-set", server: "scalekit-mcp-server", namespace: "prod" });
      }
      if (path === "/api/mcp/publish" && method === "POST") {
        const status = opts?.publishStatus ?? 200;
        if (status >= 400) return j({ error: "forbidden" }, false, status);
        return j({ name: defaultRow.name, namespace: defaultRow.namespace, visibility: "org" });
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
  visibility?: string;
  credentialSource?: string;
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

  it("hides the delete affordance when the caller cannot delete registries", async () => {
    recordingFetch({ caps: { agentregistries: { create: true, delete: false } } });
    renderPage();

    // The row renders, but with no delete button.
    await screen.findByTestId("mcp-server-scalekit-mcp-server");
    expect(screen.queryByTestId("delete-mcp-scalekit-mcp-server")).toBeNull();
  });
});

// ── T5: Unified Share dialog ─────────────────────────────────────────────────
describe("McpServersPage unified Share dialog (m76.2 T5)", () => {
  const rowWithBadges: McpRow = {
    ...defaultRow,
    visibility: "team",
    credentialSource: "byo-oauth",
  };

  it("ONE Share button opens a unified dialog with mode choice", async () => {
    recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    // Only ONE share button (not two separate org-cred + publish buttons).
    await screen.findByTestId("mcp-server-scalekit-mcp-server");
    expect(screen.queryByTestId(`org-cred-${defaultRow.name}`)).toBeNull();
    expect(screen.queryByTestId(`publish-mcp-${defaultRow.name}`)).toBeNull();

    const shareBtn = screen.getByTestId(`share-mcp-${defaultRow.name}`);
    fireEvent.click(shareBtn);

    // The dialog opens with both mode options.
    expect(await screen.findByTestId("share-dialog")).toBeInTheDocument();
    expect(screen.getByTestId("share-mode-byo")).toBeInTheDocument();
    expect(screen.getByTestId("share-mode-shared-cred")).toBeInTheDocument();
  });

  it("BYO mode (default) shows visibility picker, NOT credential input", async () => {
    recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));

    // BYO is default mode — visibility picker visible, cred input hidden.
    expect(await screen.findByTestId("share-byo-section")).toBeInTheDocument();
    expect(screen.queryByTestId("org-cred-input")).toBeNull();
  });

  it("switching to shared-cred mode shows the credential input and caution", async () => {
    recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-dialog");

    // Switch to shared-cred mode.
    fireEvent.click(screen.getByTestId("share-mode-shared-cred-radio"));

    expect(await screen.findByTestId("share-shared-cred-section")).toBeInTheDocument();
    expect(screen.getByTestId("org-cred-input")).toBeInTheDocument();
    expect(screen.getByTestId("org-cred-caution")).toBeInTheDocument();
    // BYO section hidden.
    expect(screen.queryByTestId("share-byo-section")).toBeNull();
  });

  it("BYO mode submits publish and closes dialog on success", async () => {
    const calls = recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-dialog");

    // BYO is default — submit with team visibility.
    fireEvent.click(screen.getByTestId("share-submit"));

    await waitFor(() => {
      expect(calls.some((c) => c.method === "POST" && c.url === "/api/mcp/publish")).toBe(true);
    });
    await waitFor(() => {
      expect(screen.getByText(/Visibility updated/)).toBeInTheDocument();
    });
    // Dialog closes on success.
    await waitFor(() => {
      expect(screen.queryByTestId("share-dialog")).toBeNull();
    });
  });

  it("shared-cred mode submits org-credential and reloads on success", async () => {
    const calls = recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-dialog");
    fireEvent.click(screen.getByTestId("share-mode-shared-cred-radio"));

    const input = await screen.findByTestId("org-cred-input");
    expect(input).toHaveAttribute("type", "password");
    fireEvent.change(input, { target: { value: "org-shared-token" } });

    fireEvent.click(screen.getByTestId("share-submit"));

    await waitFor(() => {
      expect(calls.some((c) => c.method === "POST" && c.url === "/api/mcp/org-credential")).toBe(true);
    });
    await waitFor(() => {
      expect(screen.getByText(/Org credential set/)).toBeInTheDocument();
    });
    // A reload follows (a second list GET after the initial one).
    await waitFor(() => {
      expect(calls.filter((c) => c.url === "/api/mcpservers" && c.method === "GET").length).toBeGreaterThanOrEqual(2);
    });
  });

  it("public publish requires confirm checkbox before submit is enabled", async () => {
    recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-byo-section");

    // Select public tier.
    fireEvent.click(screen.getByTestId("publish-option-public").querySelector("input")!);

    expect(await screen.findByTestId("publish-public-warning")).toBeInTheDocument();
    const submitBtn = screen.getByTestId("share-submit");
    expect(submitBtn).toBeDisabled();

    fireEvent.click(screen.getByTestId("publish-public-confirm"));
    expect(submitBtn).not.toBeDisabled();
  });

  it("shared-cred warning renders in BYO mode for a server with credentialSource=shared", async () => {
    const sharedCredRow: McpRow = { ...defaultRow, visibility: "team", credentialSource: "shared" };
    recordingFetch({ servers: [sharedCredRow] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    // BYO section is default.
    expect(await screen.findByTestId("publish-shared-cred-warning")).toBeInTheDocument();
    expect(screen.getByTestId("publish-shared-cred-warning")).toHaveTextContent(
      /widening visibility also widens access to that credential/,
    );
  });

  it("shows current visibility at the top of the Share dialog", async () => {
    recordingFetch({ servers: [rowWithBadges] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    const visEl = await screen.findByTestId("share-current-visibility");
    expect(visEl).toHaveTextContent("Currently: team");
  });
});

// ── T7: dialogs keep-open on failure ────────────────────────────────────────
describe("McpServersPage dialogs stay open on failure (m76.2 T7)", () => {
  it("publish 403 → dialog stays open with inline error (not a toast-and-close)", async () => {
    recordingFetch({ servers: [{ ...defaultRow, visibility: "team" }], publishStatus: 403 });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-dialog");
    fireEvent.click(screen.getByTestId("share-submit"));

    // Inline error shown.
    expect(await screen.findByTestId("share-inline-error")).toBeInTheDocument();
    // Dialog is still open.
    expect(screen.getByTestId("share-dialog")).toBeInTheDocument();
  });

  it("org-credential 403 → dialog stays open with inline error", async () => {
    recordingFetch({ servers: [{ ...defaultRow, visibility: "team" }], orgCredStatus: 403 });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-dialog");
    fireEvent.click(screen.getByTestId("share-mode-shared-cred-radio"));

    const input = await screen.findByTestId("org-cred-input");
    fireEvent.change(input, { target: { value: "bad-token" } });
    fireEvent.click(screen.getByTestId("share-submit"));

    // Inline error shown, dialog still open.
    expect(await screen.findByTestId("share-inline-error")).toBeInTheDocument();
    expect(screen.getByTestId("share-dialog")).toBeInTheDocument();
  });
});

// ── T8: CredentialSourceBadge — human labels ─────────────────────────────────
describe("McpServersPage CredentialSourceBadge (m76.2 T8)", () => {
  it("renders 'You connect your account' for byo-oauth", async () => {
    recordingFetch({ servers: [{ ...defaultRow, credentialSource: "byo-oauth" }] });
    renderPage();

    const badge = await screen.findByTestId(`cred-source-${defaultRow.name}`);
    expect(badge).toHaveTextContent("You connect your account");
  });

  it("renders 'Uses a shared credential' for shared", async () => {
    recordingFetch({ servers: [{ ...defaultRow, credentialSource: "shared" }] });
    renderPage();

    const badge = await screen.findByTestId(`cred-source-${defaultRow.name}`);
    expect(badge).toHaveTextContent("Uses a shared credential");
  });

  it("hides the badge when credentialSource is none", async () => {
    recordingFetch({ servers: [{ ...defaultRow, credentialSource: "none" }] });
    renderPage();

    await screen.findByTestId(`mcp-server-${defaultRow.name}`);
    expect(screen.queryByTestId(`cred-source-${defaultRow.name}`)).toBeNull();
  });

  it("hides the badge when credentialSource is absent", async () => {
    const { credentialSource: _removed, ...rowWithout } = { ...defaultRow, credentialSource: undefined };
    recordingFetch({ servers: [rowWithout] });
    renderPage();

    await screen.findByTestId(`mcp-server-${defaultRow.name}`);
    expect(screen.queryByTestId(`cred-source-${defaultRow.name}`)).toBeNull();
  });
});

// ── Badges ────────────────────────────────────────────────────────────────────
describe("McpServersPage visibility + scope badges (m73.7)", () => {
  it("shows visibility and scope badges on the server row", async () => {
    recordingFetch({ servers: [{ ...defaultRow, visibility: "team", credentialSource: "byo-oauth" }] });
    renderPage();

    expect(await screen.findByTestId(`visibility-${defaultRow.name}`)).toHaveTextContent("team");
    expect(screen.getByTestId(`scope-${defaultRow.name}`)).toHaveTextContent("personal");
  });
});

// ── T11: post-connect row highlight ──────────────────────────────────────────
describe("McpServersPage post-connect row highlight (m76.2 T11)", () => {
  it("highlights the row matching location.state.highlight on mount, then fades", async () => {
    recordingFetch();
    render(
      <MemoryRouter
        initialEntries={[{ pathname: "/tools/mcp-servers", state: { highlight: "prod/scalekit-mcp-server" } }]}
        initialIndex={0}
      >
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <McpServersPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    // The row should carry the highlight ring class.
    const row = await screen.findByTestId("mcp-server-scalekit-mcp-server");
    expect(row.className).toMatch(/ring-2/);
  });
});

// ── m76.1: empty state cross-link to Gallery ─────────────────────────────────
// (pins the affordance so m76.2 doesn't re-add it and duplicate the element)
describe("McpServersPage empty state Gallery cross-link (m76.1)", () => {
  it("shows a 'discover shared servers' link to /gallery?tab=mcp in the empty state", async () => {
    recordingFetch({ servers: [] });
    renderPage();

    const link = await screen.findByTestId("gallery-discover-link");
    expect(link).toBeInTheDocument();
    expect(link).toHaveAttribute("href", "/gallery?tab=mcp");
    expect(link).toHaveTextContent("discover shared servers");
  });
});

// ── P1-1: MCP share dialog submit button reads "Share" (not "Publish as …") ──

describe("McpServersPage — P1-1 share verb coherence (submit button)", () => {
  it("BYO submit button reads 'Share' when not busy", async () => {
    recordingFetch({ servers: [{ ...defaultRow, visibility: "team", credentialSource: "byo-oauth" }] });
    renderPage();

    fireEvent.click(await screen.findByTestId(`share-mcp-${defaultRow.name}`));
    await screen.findByTestId("share-dialog");

    // In BYO mode (default), the submit button must say "Share" not "Publish as team".
    expect(screen.getByTestId("share-submit")).toHaveTextContent("Share");
  });
});
