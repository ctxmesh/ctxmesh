import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { PlaygroundPage } from "@/pages/playground-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { MCP_OAUTH_MESSAGE } from "@/lib/oauth-popup";

// A recording fetch mock: it captures every request (url, method, body) and
// answers /api/invoke, /api/traces/{id}, /api/expand, /api/agents. The tests
// assert the RIGHT calls were made and that a completed run offers a "View full
// trace" Link → /traces/:id (no embedded Langfuse iframe — m17.13) — define →
// run → native trace link, and export → apply, all with mocked fetch (tier0
// determinism: no BFF, no cluster, no Langfuse).
interface Captured {
  url: string;
  method: string;
  body: string;
}

function recordingFetch(opts: {
  invoke?: (body: string) => { ok: boolean; status?: number; json: unknown };
  expand?: (body: string) => { ok: boolean; status?: number; text: string };
  create?: (body: string) => { ok: boolean; status?: number; json: unknown };
  traceUrl?: string;
}) {
  const calls: Captured[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url: path, method, body });

      if (path === "/api/invoke") {
        const r = opts.invoke
          ? opts.invoke(body)
          : { ok: true, json: { traceId: "trace-xyz", response: '{"answer":"MOCK_OK"}' } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 502),
          json: async () => r.json,
          text: async () => JSON.stringify(r.json),
        } as Response);
      }
      if (path.startsWith("/api/traces/")) {
        const traceId = path.slice("/api/traces/".length);
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({
            traceId,
            url: opts.traceUrl ?? "https://lf.test/trace/trace-xyz",
          }),
        } as Response);
      }
      if (path === "/api/expand") {
        const r = opts.expand
          ? opts.expand(body)
          : { ok: true, text: "kind: AgentDeployment\n" };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 400),
          text: async () => r.text,
          json: async () => ({ error: r.text }),
        } as Response);
      }
      if (path === "/api/agents") {
        const r = opts.create
          ? opts.create(body)
          : { ok: true, json: { created: [] } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 201 : 400),
          json: async () => r.json,
          text: async () => JSON.stringify(r.json),
        } as Response);
      }
      if (path === "/api/mcp/oauth/grant") {
        // Inline consent begin (ADR 0031): 202 + an authorization URL to pop open.
        return Promise.resolve({
          ok: true,
          status: 202,
          json: async () => ({ authorizationURL: "https://as.example/authorize?x=1", state: "st" }),
          text: async () => "",
        } as Response);
      }
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
  return calls;
}

function fill(label: RegExp | string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

// PlaygroundPage uses <Link> (react-router-dom); wrap in MemoryRouter so tests
// don't need the full app router (the pattern mirrors agents-page.test.tsx).
function renderPage() {
  return render(
    <MemoryRouter>
      <PlaygroundPage />
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("PlaygroundPage", () => {
  it("defines and runs an agent, then offers a 'View full trace' Link → /traces/:id (no Langfuse iframe)", async () => {
    const calls = recordingFetch({
      invoke: () => ({
        ok: true,
        json: { traceId: "trace-xyz", response: '{"answer":"MOCK_OK"}' },
      }),
    });

    renderPage();
    fill("Agent name", "echo-agent");
    fill("Namespace", "prod");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fill("Input (JSON)", '{"prompt":"hi"}');

    fireEvent.click(screen.getByRole("button", { name: /Run agent/ }));

    // The run posted the right /api/invoke body (agent + namespace + input).
    const invokeCall = calls.find((c) => c.url === "/api/invoke");
    expect(invokeCall).toBeDefined();
    expect(invokeCall?.method).toBe("POST");
    const payload = JSON.parse(invokeCall!.body) as {
      agent: string;
      namespace: string;
      input: unknown;
    };
    expect(payload.agent).toBe("echo-agent");
    expect(payload.namespace).toBe("prod");
    expect(payload.input).toEqual({ prompt: "hi" });

    // The run result shows the traceId + the agent response.
    expect(await screen.findByText(/Traced run complete/)).toBeInTheDocument();
    expect(screen.getByTestId("trace-id")).toHaveTextContent("trace-xyz");
    expect((screen.getByLabelText("Agent response") as HTMLTextAreaElement).value).toContain(
      "MOCK_OK",
    );

    // A completed run shows a "View full trace" link to the native /traces/:id (m17.13).
    // There is NO Langfuse iframe anywhere in the playground (m17.13 promise: kill the
    // second Langfuse login).
    const traceLink = screen.getByTestId("view-full-trace");
    expect(traceLink).toHaveAttribute("href", "/traces/trace-xyz");
    expect(document.querySelector("iframe")).toBeNull();
  });

  it("consent_required → inline Connect begins the grant from {server,ns}, pops OAuth, and re-runs on connect (m26.2)", async () => {
    const popup = { closed: false } as Window;
    const openSpy = vi.fn(() => popup);
    vi.stubGlobal("open", openSpy);

    const calls = recordingFetch({
      invoke: () => ({
        ok: true,
        json: {
          traceId: "trace-xyz",
          response: "please connect your account",
          consentRequired: ["scalekit-mcp-server"],
        },
      }),
    });

    renderPage();
    fill("Agent name", "sk-agent");
    fill("Namespace", "prod");
    fill("Image", "ghcr.io/ctxmesh/sk:v1");
    fill("Input (JSON)", '{"prompt":"list orgs"}');
    fireEvent.click(screen.getByRole("button", { name: /Run agent/ }));

    // The consent banner surfaces an inline Connect button for the named server —
    // NOT a dead-end link (the m26.2 fix).
    const connectBtn = await screen.findByTestId("connect-scalekit-mcp-server");
    expect(connectBtn).toHaveTextContent("Connect scalekit-mcp-server");
    expect(document.querySelector('a[href="/tools/mcp-servers"]')).toBeNull();

    fireEvent.click(connectBtn);

    // It begins the grant from just {server, ns} (config recovered server-side) and
    // opens the authorization URL in a popup — the SPA supplies NO OAuth config.
    await waitFor(() => {
      expect(calls.find((c) => c.url === "/api/mcp/oauth/grant")).toBeDefined();
    });
    const grantCall = calls.find((c) => c.url === "/api/mcp/oauth/grant")!;
    const grantBody = JSON.parse(grantCall.body) as { server: string; namespace: string };
    expect(grantBody.server).toBe("scalekit-mcp-server");
    expect(grantBody.namespace).toBe("prod");
    expect(grantBody).not.toHaveProperty("auth");
    expect(openSpy).toHaveBeenCalledWith(
      "https://as.example/authorize?x=1",
      "ctxmesh-oauth-connect",
      expect.stringContaining("width="),
    );

    const invokesBefore = calls.filter((c) => c.url === "/api/invoke").length;

    // The popup reports back (auto-close bridge) → the run resumes in place: a second
    // invoke, with no user re-typing.
    window.dispatchEvent(
      new MessageEvent("message", {
        data: { type: MCP_OAUTH_MESSAGE, server: "scalekit-mcp-server", error: "" },
        origin: window.location.origin,
      }),
    );
    await waitFor(() => {
      expect(calls.filter((c) => c.url === "/api/invoke").length).toBe(invokesBefore + 1);
    });
  });

  it("exports the definition to a CRD via expand → apply (the config-builder path)", async () => {
    const calls = recordingFetch({
      expand: () => ({
        ok: true,
        text: "apiVersion: agents.ctxmesh.ai/v1alpha1\nkind: AgentDeployment\nmetadata:\n  name: echo-agent\n",
      }),
      create: () => ({
        ok: true,
        status: 201,
        json: { created: [{ kind: "AgentDeployment", name: "echo-agent", namespace: "prod" }] },
      }),
    });

    renderPage();
    fill("Agent name", "echo-agent");
    fill("Namespace", "prod");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");

    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));

    const preview = (await screen.findByLabelText("Exported CRD preview")) as HTMLTextAreaElement;
    await waitFor(() => expect(preview.value).toContain("kind: AgentDeployment"));

    // The expand posted the SAME agent.yaml the define form produced.
    const expandCall = calls.find((c) => c.url === "/api/expand");
    expect(expandCall?.method).toBe("POST");
    expect(expandCall?.body).toContain("name: echo-agent");
    expect(expandCall?.body).toContain("image: ghcr.io/ctxmesh/echo:v1");

    fireEvent.click(screen.getByRole("button", { name: /Apply to cluster/ }));
    expect(await screen.findByText("Applied to the cluster")).toBeInTheDocument();
    expect(screen.getByText("echo-agent")).toBeInTheDocument();

    // The create posted the SAME agent.yaml + the target namespace (no divergence).
    const createCall = calls.find((c) => c.url === "/api/agents");
    const createPayload = JSON.parse(createCall!.body) as { agentYAML: string; namespace: string };
    expect(createPayload.agentYAML).toContain("name: echo-agent");
    expect(createPayload.namespace).toBe("prod");
  });

  it("blocks a run on client-side validation and does not call /api/invoke", async () => {
    const calls = recordingFetch({});
    renderPage();
    // Name + image empty → client validation fails.
    fireEvent.click(screen.getByRole("button", { name: /Run agent/ }));

    expect(await screen.findByText(/Fix the highlighted fields before running/)).toBeInTheDocument();
    expect(calls.find((c) => c.url === "/api/invoke")).toBeUndefined();
  });

  it("surfaces an RBAC 403 from the run (viewer cannot invoke) without a trace panel", async () => {
    recordingFetch({
      invoke: () => ({ ok: false, status: 403, json: { error: "forbidden: not allowed to read the requested agent" } }),
    });

    renderPage();
    fill("Agent name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fireEvent.click(screen.getByRole("button", { name: /Run agent/ }));

    // A 403 renders the ForbiddenInline explain-and-suggest primitive.
    expect(await screen.findByText(/forbidden: not allowed to read the requested agent/)).toBeInTheDocument();
    expect(screen.getByText("Not allowed to run this agent")).toBeInTheDocument();
    // No trace link or iframe mounts for a failed run (m17.13: no Langfuse iframe anywhere).
    expect(screen.queryByTestId("view-full-trace")).toBeNull();
    expect(document.querySelector("iframe")).toBeNull();
  });

  it("rejects malformed JSON input before any round-trip", async () => {
    const calls = recordingFetch({});
    renderPage();
    fill("Agent name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fill("Input (JSON)", "{ not json");
    fireEvent.click(screen.getByRole("button", { name: /Run agent/ }));

    expect(await screen.findByText(/Input must be valid JSON/)).toBeInTheDocument();
    expect(calls.find((c) => c.url === "/api/invoke")).toBeUndefined();
  });
});

// RBAC-aware chrome: inside the capability providers, Run + Export-Apply are
// write affordances gated on create agentdeployments (§3, DISPLAY-ONLY). A
// viewer sees neither, but keeps Preview/Export (read-only console).
describe("PlaygroundPage — RBAC-gated Run/Apply", () => {
  function installFetch(canCreate: boolean) {
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string | URL) => {
        const url = typeof input === "string" ? input : input.toString();
        if (url.startsWith("/api/namespaces")) {
          return Promise.resolve({ ok: true, status: 200, json: async () => ({ namespaces: [] }) } as Response);
        }
        if (url.startsWith("/api/capabilities")) {
          return Promise.resolve({
            ok: true,
            status: 200,
            json: async () => ({ namespace: "", allowed: { agentdeployments: { create: canCreate } } }),
          } as Response);
        }
        if (url.split("?")[0] === "/api/expand") {
          return Promise.resolve({ ok: true, status: 200, text: async () => "kind: AgentDeployment\n", json: async () => ({}) } as Response);
        }
        return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
      }),
    );
  }

  function renderGated() {
    return render(
      <MemoryRouter>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <PlaygroundPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </MemoryRouter>,
    );
  }

  it("hides Run + Apply for a viewer (no create) but keeps Preview/Export", async () => {
    installFetch(false);
    renderGated();
    // Read-only notes for both write affordances; no Run button.
    expect(await screen.findByTestId("run-readonly-note")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Run agent/ })).toBeNull();
    // Preview stays available; Apply is hidden until a preview + create right.
    expect(screen.getByRole("button", { name: /Preview CRD/ })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Apply to cluster/ })).toBeNull();
  });

  it("shows Run + Apply for an operator (create allowed)", async () => {
    installFetch(true);
    renderGated();
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Run agent/ })).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await screen.findByLabelText("Exported CRD preview");
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Apply to cluster/ })).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("run-readonly-note")).toBeNull();
  });
});
