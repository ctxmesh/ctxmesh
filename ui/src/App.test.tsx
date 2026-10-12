import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { App } from "@/App";
import { forceClear } from "@/lib/session";

// The route table, mounted whole. Dev mode is the one boot that needs no login, so these tests
// reach the console routes without minting a session; every other endpoint answers benignly.
function stubFetch() {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/devmode")) {
        return Promise.resolve({ ok: true, status: 200, json: async () => ({ devMode: true }) } as Response);
      }
      if (url.startsWith("/api/namespaces") || url.startsWith("/api/capabilities")) {
        return Promise.resolve({
          ok: false,
          status: 501,
          json: async () => ({ error: "not available in dev mode" }),
        } as Response);
      }
      return Promise.resolve({
        ok: true,
        status: 200,
        json: async () => ({ items: [], agents: [], nodes: [], edges: [], nextCursor: "" }),
      } as Response);
    }),
  );
}

function renderAt(path: string) {
  stubFetch();
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

afterEach(() => {
  forceClear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("App routes — retired surfaces fall through to not-found", () => {
  // /soon/<id> was the placeholder for a nav destination a later milestone would ship, and
  // /design the M13 wireframe gallery. Both are gone; an old link must say so plainly.
  it.each(["/soon/stops", "/soon/settings", "/design", "/design/wireframes/agents"])(
    "%s renders the console's not-found page",
    async (path) => {
      renderAt(path);
      expect(await screen.findByTestId("not-found-block")).toBeInTheDocument();
      expect(screen.queryByText(/not yet built|arrives in/i)).toBeNull();
    },
  );
});

describe("App routes — a deep link outlives the first-run nav", () => {
  // Every list here answers empty, so the shell shows the short first-run nav; a page it leaves
  // out must still open from a link.
  it("opens Tenants from its URL while the nav leaves it out", async () => {
    renderAt("/tenants");
    expect(await screen.findByRole("heading", { name: "Tenants", level: 1 })).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Sections" });
    await waitFor(() => expect(within(nav).getByTestId("nav-first-run-note")).toBeInTheDocument());
    expect(within(nav).queryByRole("link", { name: /Tenants/ })).toBeNull();
  });
});
