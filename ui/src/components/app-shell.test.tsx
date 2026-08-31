import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { AppShell } from "@/components/app-shell";
import { ToastProvider } from "@/components/kit";
import { buildCrumbs } from "@/lib/nav";
import { THEME_STORAGE_KEY } from "@/lib/theme";

// The console FRAME (M151 §4.2). These tests pin, in order:
//
//   • the approved six-section IA (Home / Agents / Library / Govern / Activity /
//     Admin) and the RBAC-aware chrome that hides write-only destinations;
//   • the FIT contract — the content column is shrinkable by construction
//     (`minmax(0,1fr)` + `min-w-0`), which is the single fix for 184 of the 202
//     overflowing renders the baseline measured;
//   • the breadcrumb trail that replaced the hard-coded "Control plane" <h1>;
//   • the overlay drawer below 1024 (focus-trapped, Esc closes, switcher +
//     sign-out in its footer);
//   • the theme control — the first time the designed dark palette is reachable;
//   • the frame stop control, present on every page and wired to POST /api/kill;
//   • live nav counts, which render nothing at all when no backend answered.

interface FetchOptions {
  ok: boolean;
  status?: number;
  body: unknown;
  namespaces?: { name: string; displayName?: string }[];
  stops?: unknown;
  approvals?: unknown;
}

let killBodies: unknown[] = [];

function installFetch(caps: FetchOptions) {
  killBodies = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
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
      if (url.startsWith("/api/kills")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => caps.stops ?? [],
        } as Response);
      }
      if (url.startsWith("/api/kill")) {
        killBodies.push(JSON.parse(String(init?.body ?? "null")));
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({ scope: "ns/team-a", applied: true }),
        } as Response);
      }
      if (url.startsWith("/api/approvals")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => caps.approvals ?? [],
        } as Response);
      }
      // The mounted index surface fans out — answer everything else benignly so
      // the shell renders without unhandled rejections.
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ nodes: [], edges: [], items: [], agents: [], nextCursor: "", runs: [], summary: { totalCostUSD: 0, totalTokens: 0, observations: 0, byModel: [] }, latency: [], scale: [] }),
      } as Response);
    }),
  );
}

function renderShell(initialEntry = "/") {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <ToastProvider>
        <Routes>
          <Route path="/" element={<AppShell />}>
            <Route index element={<div>index surface</div>} />
            <Route path="agents" element={<div>agents surface</div>} />
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

beforeEach(() => {
  window.localStorage.clear();
  document.documentElement.classList.remove("dark");
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("AppShell — the approved IA", () => {
  it("renders the six sections and the surfaces they hold", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    const nav = await screen.findByRole("navigation", { name: "Sections" });
    // Every section eyebrow (§4.2: `text-2xs` mono uppercase) — except Home's,
    // which is suppressed because it would only repeat its single item's name.
    for (const heading of ["Agents", "Library", "Govern", "Activity", "Admin"]) {
      expect(within(nav).getByText(heading, { selector: "p" })).toBeInTheDocument();
    }
    expect(within(nav).queryByText("Home", { selector: "p" })).toBeNull();
    expect(within(nav).getByRole("link", { name: /Home/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Agents/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Runs/ })).toBeInTheDocument();
    // With create allowed, the write surfaces are present.
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /Config builder/ })).toBeInTheDocument(),
    );
    expect(screen.getByRole("link", { name: /Playground/ })).toBeInTheDocument();
    // The workspace switcher (friendly label over namespace — ADR 0068 §7) is in the top bar.
    expect(screen.getByLabelText("Workspace")).toBeInTheDocument();
  });

  it("routes the not-yet-built Stops destination to its honest placeholder", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    const stops = await screen.findByRole("link", { name: /Stops/ });
    expect(stops).toHaveAttribute("href", "/soon/stops");
  });

  it("hides write-only nav for a viewer (read-only chrome by construction)", async () => {
    installFetch(DENY_WRITE);
    renderShell();
    expect(await screen.findByRole("link", { name: /Agents/ })).toBeInTheDocument();
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
    expect(await screen.findByTestId("capability-banner")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Config builder/ })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Playground/ })).toBeInTheDocument();
  });

  it("offers namespace options from GET /api/namespaces, showing displayName when set", async () => {
    installFetch({
      ...ALLOW_ALL,
      namespaces: [{ name: "team-a", displayName: "Team A" }, { name: "prod" }],
    });
    renderShell();
    const picker = (await screen.findByLabelText("Workspace")) as HTMLSelectElement;
    await waitFor(() =>
      expect(within(picker).queryByText("Team A")).not.toBeNull(),
    );
    expect(within(picker).queryByText("prod")).not.toBeNull();
    expect(within(picker).queryByText("All workspaces")).not.toBeNull();
  });
});

// The fit contract. This is the whole reason the frame was rebuilt, so it is
// asserted structurally rather than left to a screenshot: a grid child is
// `min-width:auto` by default — its own MIN-CONTENT width — so without both of
// these a single wide table sets the width of the document.
describe("AppShell — the content column is shrinkable by construction (§4.6)", () => {
  it("declares minmax(0,1fr) for the content column at every sidebar width", async () => {
    installFetch(ALLOW_ALL);
    const { container } = renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    const grid = container.querySelector("div.grid") as HTMLElement;
    expect(grid).toBeTruthy();
    // No `1fr` (= minmax(auto,1fr)) anywhere in the template, at any breakpoint.
    expect(grid.className).toContain("lg:grid-cols-[4rem_minmax(0,1fr)]");
    expect(grid.className).toContain("xl:grid-cols-[15rem_minmax(0,1fr)]");
    expect(grid.className).not.toMatch(/grid-cols-\[[^\]]*_1fr\]/);
  });

  it("carries min-w-0 on the content column and on <main>", async () => {
    installFetch(ALLOW_ALL);
    const { container } = renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    const main = container.querySelector("main") as HTMLElement;
    expect(main.className).toContain("min-w-0");
    expect((main.parentElement as HTMLElement).className).toContain("min-w-0");
  });

  it("gutters the page 16 / 24 / 32 px as the viewport grows (§4.2)", async () => {
    installFetch(ALLOW_ALL);
    const { container } = renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    const main = container.querySelector("main") as HTMLElement;
    expect(main.className).toContain("px-4"); // <768
    expect(main.className).toContain("md:px-6"); // 768–1279
    expect(main.className).toContain("xl:px-8"); // ≥1280
  });
});

describe("buildCrumbs — the trail that replaced the 'Control plane' h1", () => {
  it("names Home at the index, with nothing to click", () => {
    expect(buildCrumbs("/")).toEqual([{ label: "Home" }]);
  });

  it("walks Home → section → destination for a routed surface", () => {
    expect(buildCrumbs("/approvals")).toEqual([
      { label: "Home", to: "/" },
      { label: "Govern" },
      { label: "Approvals" },
    ]);
  });

  it("adds the page's own tail and makes the destination a link once it is not the leaf", () => {
    expect(buildCrumbs("/agents/team-a/checkout-bot")).toEqual([
      { label: "Home", to: "/" },
      // The "Agents" section crumb is dropped rather than stuttering next to the
      // "Agents" destination of the same name.
      { label: "Agents", to: "/agents" },
      { label: "team-a/checkout-bot" },
    ]);
  });

  it("keeps the section crumb when it says something the destination does not", () => {
    expect(buildCrumbs("/runs")).toEqual([
      { label: "Home", to: "/" },
      { label: "Activity" },
      { label: "Runs" },
    ]);
  });

  it("prefers the longest matching route (/tools/mcp-servers, not /tools)", () => {
    const crumbs = buildCrumbs("/tools/mcp-servers");
    expect(crumbs[crumbs.length - 1]).toEqual({ label: "MCP servers" });
  });

  it("resolves a /soon placeholder through the IA", () => {
    expect(buildCrumbs("/soon/stops")).toEqual([
      { label: "Home", to: "/" },
      { label: "Govern" },
      { label: "Stops" },
    ]);
  });

  it("says what the URL says for a path the IA does not name", () => {
    expect(buildCrumbs("/no-such-page")).toEqual([
      { label: "Home", to: "/" },
      { label: "No such page" },
    ]);
  });

  it("renders the trail in the top bar, and no 'Control plane' heading anywhere", async () => {
    installFetch(ALLOW_ALL);
    renderShell("/agents");
    const trail = await screen.findByTestId("breadcrumb");
    expect(
      within(trail).getByText("Agents", { selector: 'span[aria-current="page"]' }),
    ).toBeInTheDocument();
    expect(screen.queryByText("Control plane")).toBeNull();
  });
});

describe("AppShell — the overlay drawer (<1024)", () => {
  it("opens from the hamburger, traps focus, and closes on Escape", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });

    expect(screen.queryByTestId("nav-drawer")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Open the navigation" }));

    const drawer = await screen.findByRole("dialog", { name: "Navigation" });
    expect(drawer).toHaveAttribute("aria-modal", "true");
    // Focus moved INTO the drawer (useFocusTrap), not left on the opener.
    await waitFor(() => expect(drawer.contains(document.activeElement)).toBe(true));

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByTestId("nav-drawer")).toBeNull());
  });

  it("carries the whole IA plus the workspace switcher and sign-out in its footer", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    fireEvent.click(screen.getByRole("button", { name: "Open the navigation" }));
    const drawer = await screen.findByTestId("nav-drawer");

    expect(within(drawer).getByRole("link", { name: /Agents/ })).toBeInTheDocument();
    expect(within(drawer).getByRole("link", { name: /Approvals/ })).toBeInTheDocument();
    // The two things the bar gave up on the way down (§4.2's <768 row).
    expect(within(drawer).getByLabelText("Workspace")).toBeInTheDocument();
    expect(within(drawer).getByRole("button", { name: "Sign out" })).toBeInTheDocument();
  });

  it("closes itself when a nav item routes away", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    await screen.findByRole("link", { name: /Agents/ });
    fireEvent.click(screen.getByRole("button", { name: "Open the navigation" }));
    const drawer = await screen.findByTestId("nav-drawer");
    fireEvent.click(within(drawer).getByRole("link", { name: /Agents/ }));
    await waitFor(() => expect(screen.queryByTestId("nav-drawer")).toBeNull());
    expect(screen.getByText("agents surface")).toBeInTheDocument();
  });
});

describe("AppShell — the theme control", () => {
  it("defaults to following the device and cycles system → light → dark → system", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    const control = await screen.findByTestId("theme-control");
    expect(control).toHaveAttribute("data-theme-preference", "system");
    // jsdom has no matchMedia, so "system" resolves light — no class on <html>.
    expect(document.documentElement).not.toHaveClass("dark");

    fireEvent.click(control);
    expect(control).toHaveAttribute("data-theme-preference", "light");
    expect(document.documentElement).not.toHaveClass("dark");

    fireEvent.click(control);
    expect(control).toHaveAttribute("data-theme-preference", "dark");
    // THE point of this task: the `.dark` block in tokens.css finally renders.
    expect(document.documentElement).toHaveClass("dark");

    fireEvent.click(control);
    expect(control).toHaveAttribute("data-theme-preference", "system");
    expect(document.documentElement).not.toHaveClass("dark");
  });

  it("persists the choice under one namespaced key", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    const control = await screen.findByTestId("theme-control");
    fireEvent.click(control); // → light
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("light");
    fireEvent.click(control); // → dark
    expect(window.localStorage.getItem(THEME_STORAGE_KEY)).toBe("dark");
  });

  it("restores a stored preference on the next boot", async () => {
    window.localStorage.setItem(THEME_STORAGE_KEY, "dark");
    installFetch(ALLOW_ALL);
    renderShell();
    const control = await screen.findByTestId("theme-control");
    expect(control).toHaveAttribute("data-theme-preference", "dark");
    expect(document.documentElement).toHaveClass("dark");
  });

  it("names both the current theme and what pressing it does", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    expect(
      await screen.findByRole("button", {
        name: "Theme: follows your device. Switch to light.",
      }),
    ).toBeInTheDocument();
  });
});

describe("AppShell — the frame stop control (§5.23)", () => {
  it("is present on the page and never collapses", async () => {
    installFetch(ALLOW_ALL);
    renderShell();
    expect(await screen.findByRole("button", { name: /Stop/ })).toBeInTheDocument();
  });

  it("stops the selected workspace through POST /api/kill, with the reason", async () => {
    installFetch({ ...ALLOW_ALL, namespaces: [{ name: "team-a" }] });
    renderShell();
    const picker = (await screen.findByLabelText("Workspace")) as HTMLSelectElement;
    await waitFor(() => expect(within(picker).queryByText("team-a")).not.toBeNull());
    fireEvent.change(picker, { target: { value: "team-a" } });

    fireEvent.click(screen.getByRole("button", { name: /^Stop$/ }));
    const dialog = await screen.findByRole("alertdialog");
    // The workspace scope is pre-selected from page context, so no typed gate.
    fireEvent.change(within(dialog).getByLabelText("Why are you stopping this?"), {
      target: { value: "runaway delegation loop" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Stop this workspace" }));

    await waitFor(() => expect(killBodies).toHaveLength(1));
    expect(killBodies[0]).toEqual({
      level: "namespace",
      namespace: "team-a",
      reason: "runaway delegation loop",
    });
  });
});

describe("AppShell — live nav counts", () => {
  it("renders nothing beside a nav item when no backend answered (never a 0)", async () => {
    installFetch(ALLOW_ALL); // no namespace selected ⇒ the approvals queue is unscoped
    renderShell();
    const approvals = await screen.findByRole("link", { name: /Approvals/ });
    expect(approvals.textContent).toBe("Approvals");
  });

  it("renders the approvals queue in warn and the active stops in crit", async () => {
    installFetch({
      ...ALLOW_ALL,
      namespaces: [{ name: "team-a" }],
      approvals: [{ runId: "r1" }, { runId: "r2" }],
      stops: [{ scope: "ns/team-a", level: "namespace", namespace: "team-a", reason: "why", principal: "me" }],
    });
    renderShell();
    const picker = (await screen.findByLabelText("Workspace")) as HTMLSelectElement;
    await waitFor(() => expect(within(picker).queryByText("team-a")).not.toBeNull());
    fireEvent.change(picker, { target: { value: "team-a" } });

    const waiting = await screen.findByTitle("2 waiting on a person");
    expect(waiting).toHaveClass("text-warning");
    expect(waiting).toHaveClass("font-mono");
    const stopped = await screen.findByTitle("1 in force");
    expect(stopped).toHaveClass("text-destructive");
  });

  it("states a known zero as 0, distinct from an unknown", async () => {
    installFetch({
      ...ALLOW_ALL,
      namespaces: [{ name: "team-a" }],
      approvals: [],
      stops: [],
    });
    renderShell();
    const picker = (await screen.findByLabelText("Workspace")) as HTMLSelectElement;
    await waitFor(() => expect(within(picker).queryByText("team-a")).not.toBeNull());
    fireEvent.change(picker, { target: { value: "team-a" } });

    const none = await screen.findByTitle("None waiting on a person");
    expect(none).toHaveTextContent("0");
    expect(none).toHaveClass("text-ghost");
  });
});
