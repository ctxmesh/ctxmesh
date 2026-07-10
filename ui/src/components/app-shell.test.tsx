import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { ToastProvider } from "@/components/kit";

// The re-housed console shell (ui-foundation §6): it renders the APPROVED IA
// (NAV_SECTIONS), a namespace picker (§5), and RBAC-aware chrome (§3). These
// tests pin the re-housing (grouped nav present) + the viewer read-only chrome
// (write-only nav hidden) + the honest capability banner.

function installFetch(caps: {
  ok: boolean;
  status?: number;
  body: unknown;
  namespaces?: { name: string }[];
}) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/namespaces")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ namespaces: caps.namespaces ?? [] }),
        } as Response);
      }
      if (url.startsWith("/api/capabilities")) {
        return Promise.resolve({
          ok: caps.ok,
          status: caps.status ?? (caps.ok ? 200 : 500),
          json: async () => caps.body,
        } as Response);
      }
      // The mounted index surface (dashboard) fans out — answer everything else
      // benignly so the shell renders without unhandled rejections.
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ nodes: [], edges: [], items: [], agents: [], nextCursor: "", runs: [], summary: { totalCostUSD: 0, totalTokens: 0, observations: 0, byModel: [] }, latency: [], scale: [] }),
      } as Response);
    }),
  );
}

function renderShell() {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <ToastProvider>
        <Routes>
          <Route path="/" element={<AppShell />}>
            <Route index element={<div>index surface</div>} />
          </Route>
        </Routes>
      </ToastProvider>
    </MemoryRouter>,
  );
}

const ALLOW_ALL = {
  ok: true,
  body: {
    namespace: "",
    allowed: {
      agentdeployments: { get: true, list: true, create: true, update: true, delete: true },
    },
  },
};

const DENY_WRITE = {
  ok: true,
  body: {
    namespace: "",
    allowed: {
      agentdeployments: { get: true, list: true, create: false, update: false, delete: false },
    },
  },
};

afterEach(() => {
  vi.restoreAllMocks();
});

describe("AppShell (re-housed console)", () => {
  it("renders the approved grouped IA (Overview / Build / … sections + the re-housed surfaces)", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    // Section headings from NAV_SECTIONS.
    expect(await screen.findByText("Overview")).toBeInTheDocument();
    expect(screen.getByText("Build")).toBeInTheDocument();
    expect(screen.getByText("Observe")).toBeInTheDocument();
    expect(screen.getByText("Platform")).toBeInTheDocument();
    // The re-housed M12 surfaces are nav destinations.
    expect(screen.getByRole("link", { name: /Dashboard/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Agents/ })).toBeInTheDocument();
    // With create allowed, the write surfaces are present.
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /Config builder/ })).toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: /Playground/ })).toBeInTheDocument();
    // The namespace picker is in the header.
    expect(screen.getByLabelText("Namespace")).toBeInTheDocument();
  });

  it("hides write-only nav for a viewer (read-only chrome by construction)", async () => {
    installFetch(DENY_WRITE);
    renderShell();
    // Read surfaces stay.
    expect(await screen.findByRole("link", { name: /Agents/ })).toBeInTheDocument();
    // Write-only surfaces (config-builder, Playground) are HIDDEN for a viewer.
    await waitFor(() =>
      expect(screen.queryByRole("link", { name: /Config builder/ })).toBeNull(),
    );
    expect(screen.queryByRole("link", { name: /Playground/ })).toBeNull();
    // No banner — a definite deny is not a probe failure.
    expect(screen.queryByTestId("capability-banner")).toBeNull();
  });

  it("shows the honest capability banner (not all-disabled) on a probe 500, and keeps write nav visible", async () => {
    installFetch({ ok: false, status: 500, body: { error: "probe failed" } });
    renderShell();
    // The honest banner is raised…
    expect(await screen.findByTestId("capability-banner")).toBeInTheDocument();
    // …and write nav stays VISIBLE (fail-open for DISPLAY — never all-disabled).
    expect(screen.getByRole("link", { name: /Config builder/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Playground/ })).toBeInTheDocument();
  });

  it("offers namespace options from GET /api/namespaces", async () => {
    installFetch({ ...ALLOW_ALL, namespaces: [{ name: "team-a" }, { name: "prod" }] });
    renderShell();
    const picker = (await screen.findByLabelText("Namespace")) as HTMLSelectElement;
    await waitFor(() =>
      expect(within(picker).queryByText("team-a")).not.toBeNull(),
    );
    expect(within(picker).queryByText("prod")).not.toBeNull();
    // "All namespaces" is always the honest default.
    expect(within(picker).queryByText("All namespaces")).not.toBeNull();
  });
});
