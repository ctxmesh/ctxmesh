import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { buildCommands } from "@/lib/palette-commands";
import { ToastProvider } from "@/components/kit";
import * as session from "@/lib/session";
import { RES_AGENTS } from "@/lib/nav";

// m13.6b — the cmd-K command palette wired into the PRODUCTION shell. These tests
// pin the four contracts the task calls out: ⌘K opens the palette from the shell;
// a Navigate command routes to its destination; the palette is RBAC-filtered
// EXACTLY like the nav (a viewer's palette omits the write-only surfaces the nav
// also omits); the Sign out command runs the shell's logout; and Esc closes.

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
      // The mounted surfaces fan out — answer everything benignly so the shell
      // renders without unhandled rejections.
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
            {/* A real destination so a Navigate command's route is observable. */}
            <Route path="agents" element={<div>agents surface</div>} />
          </Route>
          {/* RequireAuth is not in this tree; logout navigates to /login. */}
          <Route path="/login" element={<div>login surface</div>} />
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

// Open the palette with a real ⌘K keydown on window (useCommandK's listener).
function pressCmdK() {
  fireEvent.keyDown(window, { key: "k", metaKey: true });
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ShellCommandPalette (cmd-K in the production shell)", () => {
  it("opens the palette on ⌘K and lists RBAC-visible Navigate commands", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    // Wait for the shell to mount (nav visible) before opening.
    await screen.findByRole("link", { name: /Agents/ });

    // No dialog until ⌘K.
    expect(screen.queryByRole("dialog", { name: /command palette/i })).toBeNull();
    pressCmdK();

    const dialog = await screen.findByRole("dialog", { name: /command palette/i });
    expect(dialog).toBeInTheDocument();
    // Navigate options are present (as listbox options, distinct from nav links).
    expect(screen.getByRole("option", { name: /Dashboard/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /Agents/ })).toBeInTheDocument();
    // With create allowed, the write surface IS offered (like the nav).
    expect(screen.getByRole("option", { name: /Config builder/ })).toBeInTheDocument();
    // The Sign out action is offered.
    expect(screen.getByRole("option", { name: /Sign out/ })).toBeInTheDocument();
  });

  it("routes to a destination when a Navigate command is selected", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    expect(screen.getByText("index surface")).toBeInTheDocument();

    pressCmdK();
    await screen.findByRole("dialog", { name: /command palette/i });
    fireEvent.click(screen.getByRole("option", { name: /^Agents/ }));

    // Navigated to /agents (and the palette closed).
    await waitFor(() =>
      expect(screen.getByText("agents surface")).toBeInTheDocument(),
    );
    expect(screen.queryByRole("dialog", { name: /command palette/i })).toBeNull();
  });

  it("RBAC-filters the palette like the nav: a viewer's palette omits write-only surfaces", async () => {
    installFetch(DENY_WRITE);
    renderShell();
    // The nav omits write-only surfaces for a viewer (established shell contract).
    await waitFor(() =>
      expect(screen.queryByRole("link", { name: /Config builder/ })).toBeNull(),
    );

    pressCmdK();
    await screen.findByRole("dialog", { name: /command palette/i });
    // Read surface is still navigable…
    expect(screen.getByRole("option", { name: /^Agents/ })).toBeInTheDocument();
    // …but write-only surfaces are absent from the palette too (parity with nav).
    expect(screen.queryByRole("option", { name: /Config builder/ })).toBeNull();
    expect(screen.queryByRole("option", { name: /Playground/ })).toBeNull();
  });

  it("runs the shell logout when Sign out is selected", async () => {
    installFetch(ALLOW_ALL);
    const logoutSpy = vi.spyOn(session, "logout").mockImplementation(() => {});
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });

    pressCmdK();
    await screen.findByRole("dialog", { name: /command palette/i });
    fireEvent.click(screen.getByRole("option", { name: /Sign out/ }));

    expect(logoutSpy).toHaveBeenCalledTimes(1);
    // The shell's logout flow redirects to /login.
    await waitFor(() =>
      expect(screen.getByText("login surface")).toBeInTheDocument(),
    );
  });

  it("closes on Esc", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });

    pressCmdK();
    await screen.findByRole("dialog", { name: /command palette/i });
    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() =>
      expect(screen.queryByRole("dialog", { name: /command palette/i })).toBeNull(),
    );
  });

  it("opens the palette from the header ⌘K affordance click", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    fireEvent.click(screen.getByRole("button", { name: /command palette/i }));
    expect(
      await screen.findByRole("dialog", { name: /command palette/i }),
    ).toBeInTheDocument();
  });
});

// A focused unit check of the RBAC filter + namespace sub-commands, independent
// of the mounted shell (buildCommands is the pure command-builder).
describe("buildCommands (RBAC + namespace sub-commands)", () => {
  it("omits write-only nav when the caller can't create", () => {
    const cmds = buildCommands({
      can: (_r, verb) => verb !== "create", // viewer: no create
      navigate: () => {},
      namespaces: [],
      currentNamespace: "",
      setNamespace: () => {},
      onLogout: () => {},
    });
    const labels = cmds.map((c) => c.label);
    expect(labels).toContain("Agents");
    expect(labels).not.toContain("Config builder");
    expect(labels).not.toContain("Playground");
  });

  it("offers non-current namespaces as switch sub-commands + Sign out", () => {
    const cmds = buildCommands({
      can: () => true,
      navigate: () => {},
      namespaces: ["team-a", "prod"],
      currentNamespace: "team-a",
      setNamespace: () => {},
      onLogout: () => {},
    });
    const nsCmds = cmds.filter((c) => c.group === "Switch namespace").map((c) => c.label);
    // The current namespace is not re-offered; the others + "All namespaces" are.
    expect(nsCmds).not.toContain("team-a");
    expect(nsCmds).toContain("prod");
    expect(nsCmds).toContain("All namespaces");
    expect(cmds.some((c) => c.label === "Sign out")).toBe(true);
  });

  it("gates on the same resource the nav uses (agentdeployments create)", () => {
    // Sanity: the write gate references the agents resource, matching nav.ts.
    const calls: Array<[string, string]> = [];
    buildCommands({
      can: (r, v) => {
        calls.push([r, v]);
        return true;
      },
      navigate: () => {},
      namespaces: [],
      currentNamespace: "",
      setNamespace: () => {},
      onLogout: () => {},
    });
    expect(calls).toContainEqual([RES_AGENTS, "create"]);
  });
});
