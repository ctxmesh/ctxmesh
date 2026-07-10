import { afterEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";

import { CapabilitiesProvider, Can, useCapabilities } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { RES_AGENTS } from "@/lib/nav";

// The RBAC-aware chrome context (ui-foundation §3, ADR 0011 DISPLAY-ONLY). These
// tests pin the reviewer carry-forwards that make the chrome HONEST:
//   • a capabilities 500/network error is a PROBE FAILURE → affordances stay
//     visible + a probeError is raised (NEVER a silently all-disabled console);
//   • a definite server `false` denies (viewer chrome);
//   • reprobe() re-fetches after a 403-despite-a-"yes" surprise.

function stubCapabilities(
  respond: (ns: string) => { ok: boolean; status?: number; body: unknown },
) {
  const calls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      calls.push(url);
      if (url.startsWith("/api/namespaces")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ namespaces: [] }),
        } as Response);
      }
      // /api/capabilities?namespace=<ns>
      const ns = new URLSearchParams(url.split("?")[1] ?? "").get("namespace") ?? "";
      const r = respond(ns);
      return Promise.resolve({
        ok: r.ok,
        status: r.status ?? (r.ok ? 200 : 500),
        json: async () => r.body,
      } as Response);
    }),
  );
  return calls;
}

// A probe of the current caps state + a reprobe trigger, for assertions.
function Probe() {
  const { can, probeError, reprobe } = useCapabilities();
  return (
    <div>
      <span data-testid="can-create">{String(can(RES_AGENTS, "create"))}</span>
      <span data-testid="probe-error">{probeError ?? ""}</span>
      <button onClick={reprobe}>reprobe</button>
      <Can resource={RES_AGENTS} verb="create">
        <span data-testid="create-affordance">Create</span>
      </Can>
    </div>
  );
}

function renderWithCaps() {
  return render(
    <NamespaceProvider>
      <CapabilitiesProvider>
        <Probe />
      </CapabilitiesProvider>
    </NamespaceProvider>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("capabilities context (RBAC-aware chrome)", () => {
  it("shows write affordances when the probe says allowed", async () => {
    stubCapabilities(() => ({
      ok: true,
      body: { namespace: "", allowed: { [RES_AGENTS]: { create: true, list: true } } },
    }));
    renderWithCaps();
    await waitFor(() =>
      expect(screen.getByTestId("can-create")).toHaveTextContent("true"),
    );
    expect(screen.getByTestId("create-affordance")).toBeInTheDocument();
    expect(screen.getByTestId("probe-error")).toHaveTextContent("");
  });

  it("hides write affordances when the probe says denied (viewer chrome)", async () => {
    stubCapabilities(() => ({
      ok: true,
      body: { namespace: "", allowed: { [RES_AGENTS]: { create: false, list: true } } },
    }));
    renderWithCaps();
    await waitFor(() =>
      expect(screen.getByTestId("can-create")).toHaveTextContent("false"),
    );
    // <Can create> renders nothing — no create affordance for a viewer.
    expect(screen.queryByTestId("create-affordance")).toBeNull();
    // A definite deny is NOT a probe error (no honest-failure banner).
    expect(screen.getByTestId("probe-error")).toHaveTextContent("");
  });

  it("a capabilities 500 is a PROBE FAILURE → banner + affordances STAY visible (not all-disabled)", async () => {
    stubCapabilities(() => ({ ok: false, status: 500, body: { error: "probe boom" } }));
    renderWithCaps();
    // The honest banner (probeError) is raised…
    await waitFor(() =>
      expect(screen.getByTestId("probe-error")).toHaveTextContent("probe boom"),
    );
    // …and can() is OPTIMISTIC (true) — the console is NOT silently all-disabled.
    expect(screen.getByTestId("can-create")).toHaveTextContent("true");
    expect(screen.getByTestId("create-affordance")).toBeInTheDocument();
  });

  it("reprobe() re-fetches the capabilities (the 403-despite-a-yes path)", async () => {
    // First probe: allowed. After a 403 surprise the surface calls reprobe();
    // the second probe now reports denied (the map was stale).
    let call = 0;
    const calls = stubCapabilities(() => {
      call += 1;
      const allow = call === 1;
      return {
        ok: true,
        body: { namespace: "", allowed: { [RES_AGENTS]: { create: allow } } },
      };
    });
    renderWithCaps();
    await waitFor(() =>
      expect(screen.getByTestId("can-create")).toHaveTextContent("true"),
    );

    const capCallsBefore = calls.filter((u) => u.startsWith("/api/capabilities")).length;
    act(() => {
      screen.getByRole("button", { name: "reprobe" }).click();
    });

    // reprobe issued a fresh capabilities fetch, and the map is corrected.
    await waitFor(() =>
      expect(screen.getByTestId("can-create")).toHaveTextContent("false"),
    );
    const capCallsAfter = calls.filter((u) => u.startsWith("/api/capabilities")).length;
    expect(capCallsAfter).toBeGreaterThan(capCallsBefore);
  });

  it("treats an UNPROBED cell as optimistic (missing entry ≠ denied)", async () => {
    // The map has `list` but not `create` — a missing cell is unknown, not deny.
    stubCapabilities(() => ({
      ok: true,
      body: { namespace: "", allowed: { [RES_AGENTS]: { list: true } } },
    }));
    renderWithCaps();
    await waitFor(() =>
      expect(screen.getByTestId("can-create")).toHaveTextContent("true"),
    );
    expect(screen.getByTestId("create-affordance")).toBeInTheDocument();
  });
});
