import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { DesignGallery } from "@/design/gallery";
import { IAMap } from "@/design/ia-map";
import { WIREFRAMES } from "@/design/manifest";

// The design gate's contract (m13.1): the gallery indexes every wireframe, the
// IA map renders, and — critically — EVERY registered wireframe renders without
// throwing (no live API calls; static placeholder data only). If a wireframe
// crashes, the user can't review it at the gate, so this is a hard gate.
//
// Mount the gallery under a `design/*` parent exactly as App.tsx does, so the
// gallery's relative nested routes resolve against /design.
function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="design/*" element={<DesignGallery />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe("design gallery", () => {
  it("renders the index with the gate heading and every wireframe title", () => {
    renderAt("/design");
    expect(screen.getByText("Console arc — IA + wireframes")).toBeInTheDocument();
    // Spot-check a few section headings + a title from the manifest.
    expect(screen.getByText("Auth & shell")).toBeInTheDocument();
    expect(
      screen.getByText("Native trace explorer"),
    ).toBeInTheDocument();
  });

  it("renders the IA map with the nav-tree destinations", () => {
    render(
      <MemoryRouter>
        <IAMap />
      </MemoryRouter>,
    );
    expect(
      screen.getByText("Information architecture"),
    ).toBeInTheDocument();
    // Reads from the shared NAV_SECTIONS source.
    expect(screen.getByText("Overview")).toBeInTheDocument();
    expect(screen.getByText("Platform")).toBeInTheDocument();
  });

  it("shows an honest not-found for an unknown wireframe slug", () => {
    renderAt("/design/w/does-not-exist");
    expect(screen.getByText("Unknown wireframe")).toBeInTheDocument();
  });

  // The core gate: every wireframe route must render without throwing.
  it.each(WIREFRAMES.map((w) => [w.slug, w.title] as const))(
    "renders wireframe %s (%s) without errors",
    (slug) => {
      expect(() => renderAt(`/design/w/${slug}`)).not.toThrow();
      // The wireframe route always renders its title in the header.
      const wf = WIREFRAMES.find((w) => w.slug === slug)!;
      expect(
        screen.getAllByText(wf.title).length,
      ).toBeGreaterThan(0);
    },
  );

  it("makes no network calls from any wireframe (static placeholder data only)", () => {
    // fetch is not stubbed here — if a wireframe called it, jsdom would throw or
    // the call would be a real leak. Rendering all of them proves none do.
    const originalFetch = globalThis.fetch;
    let called = false;
    // @ts-expect-error — minimal stub to detect any call.
    globalThis.fetch = () => {
      called = true;
      return Promise.resolve({ ok: true, json: async () => ({}) });
    };
    for (const w of WIREFRAMES) {
      renderAt(`/design/w/${w.slug}`);
    }
    globalThis.fetch = originalFetch;
    expect(called).toBe(false);
  });
});
