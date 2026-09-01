import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ProvidersPage } from "@/pages/providers-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";
// The list mock is built FROM the m18.4 contract fixture (real BFF DTO shape), so a
// shape drift is caught by the Go golden test, not by a stale hand-authored mock.
import fixtures from "@/test/contract-fixtures.json";

const OPERATOR = { secretbindings: { create: true, update: true, delete: true } };
const VIEWER = { secretbindings: { create: false, update: false, delete: false } };

const providerList = fixtures.ProviderListResponse; // { providers:[...], items:[...] }

type Setup = {
  caps?: Record<string, Record<string, boolean>>;
  providersStatus?: number;
  providers?: unknown[];
  rotateStatus?: number;
  disconnectStatus?: number;
};

function installFetch(opts: Setup = {}) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (body: unknown, ok = true, status = ok ? 200 : 500) =>
        Promise.resolve({
          ok,
          status,
          json: async () => body,
          text: async () => JSON.stringify(body),
        } as Response);

      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: opts.caps ?? OPERATOR });

      if (url === "/api/providers" && method === "GET") {
        const status = opts.providersStatus ?? 200;
        if (status >= 400) return j({ error: "unavailable" }, false, status);
        return j(
          opts.providers
            ? { providers: opts.providers, items: opts.providers }
            : providerList,
        );
      }
      if (/\/api\/providers\/[^/]+\/rotate$/.test(url) && method === "POST") {
        const status = opts.rotateStatus ?? 200;
        return j(status < 400 ? providerList.providers[0] : { error: "invalid key" }, status < 400, status);
      }
      if (/\/api\/providers\/[^/]+$/.test(url) && method === "DELETE") {
        const status = opts.disconnectStatus ?? 204;
        return j({}, status < 400, status);
      }
      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderPage(caps?: Record<string, Record<string, boolean>>, setup: Setup = {}) {
  installFetch({ ...setup, caps: caps ?? setup.caps });
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <ProvidersPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  sessionStorage.clear();
});

describe("ProvidersPage", () => {
  it("lists connected providers from the real DTO shape", async () => {
    renderPage(OPERATOR);
    expect(await screen.findByText("Anthropic")).toBeInTheDocument();
    // The fixture's provider has 2 models — rendered as a count, never key
    // material. M151 §4.8 puts a count in the numeric register (mono, tabular,
    // right-aligned), so the cell reads "2" under a "Models" head rather than
    // the prose "2 models"; the assertion is scoped to the row's own cell so it
    // cannot be satisfied by the closing line elsewhere on the page.
    const models = screen.getByTestId("provider-models-anthropic");
    expect(models).toHaveTextContent(/^2$/);
    // …and the count is DISCOVERABLE (M100 UI99-refs): hovering reveals the actual model names.
    expect(models).toHaveAttribute("title", expect.stringContaining("claude"));
    expect(screen.getByTestId("row-actions-anthropic")).toBeInTheDocument();
    // m54.6: the per-row action reads "Create agent" (parity with the connect
    // flow's "Create agent with this"), not the ambiguous "Use".
    expect(screen.getByTestId("use-anthropic")).toHaveTextContent("Create agent");
  });

  it("Rotate opens a dialog and POSTs the new key to /rotate", async () => {
    const calls = installFetch({ caps: OPERATOR });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <ProvidersPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByTestId("rotate-anthropic"));
    const input = await screen.findByTestId("rotate-key-input");
    fireEvent.change(input, { target: { value: "sk-new-rotated-key" } });
    fireEvent.click(screen.getByTestId("rotate-confirm"));

    await waitFor(() => {
      const post = calls.find(
        (c) => c.url === "/api/providers/anthropic/rotate" && c.method === "POST",
      );
      expect(post).toBeDefined();
      expect(post!.body).toContain("sk-new-rotated-key");
    });
  });

  it("Disconnect goes through the typed-name confirm and DELETEs", async () => {
    const calls = installFetch({ caps: OPERATOR });
    render(
      <MemoryRouter>
        <ToastProvider>
          <NamespaceProvider>
            <CapabilitiesProvider>
              <ProvidersPage />
            </CapabilitiesProvider>
          </NamespaceProvider>
        </ToastProvider>
      </MemoryRouter>,
    );

    fireEvent.click(await screen.findByTestId("disconnect-anthropic"));
    const dialog = screen.getByRole("alertdialog");
    // Typed-name gate: type the provider name to enable the destructive confirm.
    fireEvent.change(within(dialog).getByLabelText(/to confirm/i), {
      target: { value: "anthropic" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: /disconnect/i }));

    await waitFor(() => {
      const del = calls.find(
        (c) => /\/api\/providers\/anthropic/.test(c.url) && c.method === "DELETE",
      );
      expect(del).toBeDefined();
    });
  });

  it("a viewer sees the list but NO rotate/disconnect actions", async () => {
    renderPage(VIEWER);
    await screen.findByText("Anthropic");
    expect(screen.queryByTestId("rotate-anthropic")).not.toBeInTheDocument();
    expect(screen.queryByTestId("disconnect-anthropic")).not.toBeInTheDocument();
    expect(screen.queryByTestId("connect-provider-button")).not.toBeInTheDocument();
  });

  it("points a not-ready connection at its model route, where the reason is", async () => {
    // `ready` is the backing ModelRoute's Ready condition and this list cannot
    // see WHY it is false, so the next step must not guess at a cause — it
    // sends the reader to the route that carries the condition message.
    renderPage(OPERATOR, {
      providers: [
        {
          name: "broken",
          namespace: "default",
          provider: "openai",
          displayName: "Broken key",
          models: [],
          secretName: "broken",
          ready: false,
        },
      ],
    });
    const next = await screen.findByTestId("provider-next-broken");
    expect(next).toHaveTextContent("Open its route");
    expect(next).toHaveAttribute("href", "/routes/default/broken");
  });

  it("shows the empty state when no providers are connected", async () => {
    renderPage(OPERATOR, { providers: [] });
    expect(await screen.findByText(/No providers connected/)).toBeInTheDocument();
  });

  it("shows the disabled state when connect is kill-switched (404)", async () => {
    renderPage(OPERATOR, { providersStatus: 404 });
    expect(await screen.findByTestId("providers-disabled")).toBeInTheDocument();
  });
});
