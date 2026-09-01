import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { ConfigBuilderPage } from "@/pages/config-builder-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";

// A recording fetch mock: it captures every request (url, method, body) and
// answers /api/expand (YAML text) + /api/agents (JSON created list). The tests
// assert the RIGHT agent.yaml was posted and the create path was hit — the
// form → agent.yaml → expand → preview → apply flow, with mocked fetch (tier0
// determinism: no BFF, no cluster).
interface Captured {
  url: string;
  method: string;
  body: string;
}

function recordingFetch(opts: {
  expand?: (body: string) => { ok: boolean; status?: number; text: string };
  create?: (body: string) => { ok: boolean; status?: number; json: unknown };
}) {
  const calls: Captured[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url, method, body });

      if (url === "/api/expand") {
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
      if (url === "/api/agents") {
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
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
  return calls;
}

function fill(label: RegExp | string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

// The applied panel ends in a NextStepLink to the created agent (a router
// <Link>), so the page needs a Router in scope — the same wrapper the RBAC
// block below already uses.
function renderPage() {
  return render(
    <MemoryRouter>
      <ConfigBuilderPage />
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ConfigBuilderPage", () => {
  it("fills the form, posts the right agent.yaml to /api/expand, and shows the CRD preview", async () => {
    const calls = recordingFetch({
      expand: () => ({
        ok: true,
        text: "apiVersion: agents.ctxmesh.ai/v1alpha1\nkind: AgentDeployment\nmetadata:\n  name: echo-agent\n",
      }),
    });

    renderPage();
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fill("Model route", "default-model");

    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));

    // The preview well shows the server-expanded CRD. It is a <pre> code well,
    // not a textarea (M151 §4.5): YAML must not be soft-wrapped, because a
    // wrapped continuation line reads as a different indentation — i.e. a
    // different manifest — than the one that would be applied.
    const preview = await screen.findByLabelText("Expanded CRD preview");
    await waitFor(() => expect(preview).toHaveTextContent("kind: AgentDeployment"));

    // The right agent.yaml was posted to /api/expand.
    const expandCall = calls.find((c) => c.url === "/api/expand");
    expect(expandCall).toBeDefined();
    expect(expandCall?.method).toBe("POST");
    expect(expandCall?.body).toContain("name: echo-agent");
    expect(expandCall?.body).toContain("image: ghcr.io/ctxmesh/echo:v1");
    expect(expandCall?.body).toContain("route: default-model");
  });

  it("applies the previewed agent.yaml via POST /api/agents (create) and shows the result", async () => {
    const calls = recordingFetch({
      expand: () => ({ ok: true, text: "kind: AgentDeployment\n" }),
      create: () => ({
        ok: true,
        status: 201,
        json: { created: [{ kind: "AgentDeployment", name: "echo-agent", namespace: "default" }] },
      }),
    });

    renderPage();
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");

    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await screen.findByLabelText("Expanded CRD preview");

    fireEvent.click(screen.getByRole("button", { name: /Apply to cluster/ }));

    // Success state renders the created object.
    expect(await screen.findByText("Applied to the cluster")).toBeInTheDocument();
    expect(screen.getByText("echo-agent")).toBeInTheDocument();

    // The create posted the SAME agent.yaml the preview received (no divergence).
    const createCall = calls.find((c) => c.url === "/api/agents");
    expect(createCall?.method).toBe("POST");
    const payload = JSON.parse(createCall!.body) as { agentYAML: string };
    expect(payload.agentYAML).toContain("name: echo-agent");
    expect(payload.agentYAML).toContain("image: ghcr.io/ctxmesh/echo:v1");
  });

  it("renders the manifest in a scrolling code well — YAML indentation is never re-wrapped", async () => {
    recordingFetch({
      expand: () => ({ ok: true, text: "spec:\n  scaling:\n    min: 1\n" }),
    });

    renderPage();
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));

    const well = await screen.findByLabelText("Expanded CRD preview");
    // A <pre>, not a soft-wrapping <textarea>. In YAML a wrapped continuation
    // line reads as a different indentation — i.e. a different manifest — than
    // the one that would actually be applied.
    expect(well.tagName).toBe("PRE");
    await waitFor(() => expect(well.textContent).toContain("  scaling:\n    min: 1"));
    // It owns its own scrollbar, so a long line scrolls HERE and never widens
    // the page (§4.6), and it never break-alls mid-token (§4.5).
    expect(well.className).toMatch(/overflow-auto/);
    expect(well.className).not.toMatch(/break-all|whitespace-pre-wrap/);
    // A scroll container a mouse can reach must be reachable without one.
    expect(well).toHaveAttribute("tabindex", "0");
  });

  it("never claims the applied objects are serving — applied is not Ready", async () => {
    recordingFetch({
      expand: () => ({ ok: true, text: "kind: AgentDeployment\n" }),
      create: () => ({
        ok: true,
        status: 201,
        json: {
          created: [{ kind: "AgentDeployment", name: "echo-agent", namespace: "default" }],
        },
      }),
    });

    renderPage();
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await screen.findByLabelText("Expanded CRD preview");
    fireEvent.click(screen.getByRole("button", { name: /Apply to cluster/ }));

    const panel = await screen.findByTestId("crd-applied");
    // POST /api/agents reports what it CREATED and nothing about readiness, so
    // the page says so instead of implying a healthy agent (§7.1).
    expect(panel).toHaveTextContent(/not\s+serving\s+yet/);
    // The state tag is the converging pine tint, never the ok green.
    expect(within(panel).getByText("applied").className).toMatch(/bg-accent/);
    expect(within(panel).getByText("applied").className).not.toMatch(/bg-success/);
    // A Kind is the object's identity, not its health — the muted chip (§2.2).
    expect(within(panel).getByText("AgentDeployment").className).toMatch(/bg-surface-2/);
  });

  it("surfaces a validation error from the server (400) without applying", async () => {
    recordingFetch({
      expand: () => ({ ok: false, status: 400, text: "required field missing: image" }),
    });

    renderPage();
    fill("Name", "echo-agent");
    // image is client-valid enough to submit? No — image is required client-side,
    // so put a value then have the SERVER reject (simulating a server-only rule).
    fill("Image", "ghcr.io/ctxmesh/echo:v1");

    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));

    expect(await screen.findByText(/required field missing: image/)).toBeInTheDocument();
    // Apply stays disabled (there is no valid preview).
    expect(screen.getByRole("button", { name: /Apply to cluster/ })).toBeDisabled();
  });

  it("blocks preview on client-side validation and does not call the server", async () => {
    const calls = recordingFetch({});
    renderPage();
    // Leave name+image empty → client validation fails.
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));

    expect(await screen.findByText(/Fix the highlighted fields/)).toBeInTheDocument();
    expect(calls.find((c) => c.url === "/api/expand")).toBeUndefined();
  });

  it("surfaces an RBAC 403 from the apply path (viewer cannot create)", async () => {
    recordingFetch({
      expand: () => ({ ok: true, text: "kind: AgentDeployment\n" }),
      create: () => ({ ok: false, status: 403, json: { error: "forbidden: not allowed to create" } }),
    });

    renderPage();
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await screen.findByLabelText("Expanded CRD preview");
    fireEvent.click(screen.getByRole("button", { name: /Apply to cluster/ }));

    // A 403 now renders the ForbiddenInline explain-and-suggest primitive with a
    // custom title, not a terse red line — and never the raw BFF RBAC string.
    expect(await screen.findByText("Not allowed to apply")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/forbidden: not allowed/)).toBeNull();
  });
});

// RBAC-aware chrome: rendered inside the capability providers so the Apply
// affordance is gated (§3, DISPLAY-ONLY). A denied caller sees no Apply button
// but can still Preview (read-only console by construction).
describe("ConfigBuilderPage — RBAC-gated Apply", () => {
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
        if (url === "/api/expand") {
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
            <ConfigBuilderPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </MemoryRouter>,
    );
  }

  it("hides the Apply affordance for a viewer (no create) but keeps Preview", async () => {
    installFetch(false);
    renderGated();
    // The read-only note appears; there is NO Apply button.
    expect(await screen.findByTestId("apply-readonly-note")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Apply to cluster/ })).toBeNull();
    // Preview stays available.
    expect(screen.getByRole("button", { name: /Preview CRD/ })).toBeInTheDocument();
  });

  it("shows the Apply affordance for an operator (create allowed)", async () => {
    installFetch(true);
    renderGated();
    // The well now renders only once there IS a manifest (it used to be an
    // always-present empty textarea), so the preview has to be a real one.
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await screen.findByLabelText("Expanded CRD preview");
    // With create allowed the Apply button is present (enabled after a preview).
    await waitFor(() =>
      expect(screen.getByRole("button", { name: /Apply to cluster/ })).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("apply-readonly-note")).toBeNull();
  });
});
