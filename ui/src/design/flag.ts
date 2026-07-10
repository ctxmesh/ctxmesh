// Design-gallery flag gating (m13.1 design gate).
//
// The /design gallery is a REVIEW artifact, not a product surface: the user
// clicks through it to approve the whole arc's design before m13.2 builds. It
// must be INVISIBLE in a normal production build. Two ways to enable it:
//
//   1. Build-time  — `VITE_DESIGN_GALLERY=1 pnpm build` bakes it in (the flag
//      is statically replaced by Vite; without it the gallery route is dead
//      code the bundler can tree-shake).
//   2. Run-time    — a `?design` query param on any URL flips it on for a dev
//      preview without a special build (e.g. `pnpm dev` then /?design).
//
// A normal `pnpm build` (no env var) ships NO /design route reachable by URL.

export function designGalleryEnabled(): boolean {
  // Build-time flag — statically inlined by Vite; "1" enables.
  const flag = import.meta.env.VITE_DESIGN_GALLERY;
  if (flag === "1" || flag === "true") return true;

  // Run-time escape hatch for dev preview.
  if (typeof window !== "undefined") {
    return new URLSearchParams(window.location.search).has("design");
  }
  return false;
}
