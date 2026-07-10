import { afterEach, describe, expect, it, vi } from "vitest";

import { designGalleryEnabled } from "@/design/flag";

// The flag mechanism (m13.1): /design is invisible in a normal build. It turns
// on ONLY when the build-time VITE_DESIGN_GALLERY flag is "1"/"true", or a
// `?design` query param is present at runtime. Anything else is off.

afterEach(() => {
  vi.unstubAllEnvs();
  // Reset the URL between cases.
  window.history.replaceState({}, "", "/");
});

describe("designGalleryEnabled", () => {
  it("is OFF by default (no flag, no param)", () => {
    vi.stubEnv("VITE_DESIGN_GALLERY", "");
    expect(designGalleryEnabled()).toBe(false);
  });

  it("is ON with VITE_DESIGN_GALLERY=1", () => {
    vi.stubEnv("VITE_DESIGN_GALLERY", "1");
    expect(designGalleryEnabled()).toBe(true);
  });

  it("is ON with the ?design runtime param", () => {
    vi.stubEnv("VITE_DESIGN_GALLERY", "");
    window.history.replaceState({}, "", "/anything?design");
    expect(designGalleryEnabled()).toBe(true);
  });

  it("is OFF for an unrelated query param", () => {
    vi.stubEnv("VITE_DESIGN_GALLERY", "");
    window.history.replaceState({}, "", "/?foo=bar");
    expect(designGalleryEnabled()).toBe(false);
  });
});
