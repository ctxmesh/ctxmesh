// Design-gallery flag gating (m13.1 design gate).
//
// The /design gallery is a REVIEW artifact, not a product surface: the user
// clicks through it to approve the whole arc's design before m13.2 builds. It
// must be INVISIBLE in a normal production build. Two ways to enable it:
//
//   1. Build-time  — `VITE_DESIGN_GALLERY=1 pnpm build` bakes it in (the flag
//      is statically replaced by Vite; without it the gallery route is dead
//      code the bundler can tree-shake).
//   2. Dev server   — `pnpm dev` (import.meta.env.DEV), which never ships.
//
// A normal `pnpm build` (no env var) ships NO /design route reachable by URL,
// and the gallery is tree-shaken out of the bundle entirely.
//
// The `?design` RUN-TIME escape hatch this used to carry is gone (M151
// hardening pass, A2). It defeated its own purpose twice over: the check read
// `window.location.search` at runtime, so Rollup could never prove the gallery
// unreachable and shipped it in every production build — and because the query
// param worked in production, `/design?design` served internal wireframes to
// anyone who could reach the console, with no login. A flag that can be turned
// on by the person you are hiding the thing from is not a flag.

// A module-level `const` of two statically-inlined values, not a function
// call: Vite substitutes literals for both, the constant folds to `false` in a
// production build, and Rollup drops the guarded `<Route>` — and with it the
// lazy `import("@/design/gallery")` — out of the graph entirely. As a function
// the fold could not happen, and the 92 kB gallery chunk shipped on every
// build even when the route was unreachable.
export const DESIGN_GALLERY_ENABLED: boolean =
  import.meta.env.VITE_DESIGN_GALLERY === "1" ||
  import.meta.env.VITE_DESIGN_GALLERY === "true" ||
  import.meta.env.DEV;
