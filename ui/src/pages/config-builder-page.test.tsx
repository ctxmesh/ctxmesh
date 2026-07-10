import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";

import { ConfigBuilderPage } from "@/pages/config-builder-page";

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

    render(<ConfigBuilderPage />);
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fill("Model route", "default-model");

    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));

    // The preview textarea shows the server-expanded CRD.
    const preview = (await screen.findByLabelText(
      "Expanded CRD preview",
    )) as HTMLTextAreaElement;
    await waitFor(() => expect(preview.value).toContain("kind: AgentDeployment"));

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

    render(<ConfigBuilderPage />);
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

  it("surfaces a validation error from the server (400) without applying", async () => {
    recordingFetch({
      expand: () => ({ ok: false, status: 400, text: "required field missing: image" }),
    });

    render(<ConfigBuilderPage />);
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
    render(<ConfigBuilderPage />);
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

    render(<ConfigBuilderPage />);
    fill("Name", "echo-agent");
    fill("Image", "ghcr.io/ctxmesh/echo:v1");
    fireEvent.click(screen.getByRole("button", { name: /Preview CRD/ }));
    await screen.findByLabelText("Expanded CRD preview");
    fireEvent.click(screen.getByRole("button", { name: /Apply to cluster/ }));

    expect(await screen.findByText(/forbidden: not allowed to create/)).toBeInTheDocument();
  });
});
