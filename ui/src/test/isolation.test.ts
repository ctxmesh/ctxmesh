import { describe, expect, it } from "vitest";

// The suite failed only under shuffled file order and passed every time in isolation (M182). The
// cause was localStorage: setup.ts pinned a namespace per test but never CLEARED the store, so
// real console state — ctxmesh.session.token, ctxmesh.theme, ctxmesh.oidc.flow — outlived the test
// that wrote it. A leaked session token changes whether the NEXT test renders an authenticated
// shell, which is exactly the shape of the failures (AppShell's drawer, the create-team wizard).
//
// These two tests encode the invariant deterministically, because the flake itself was not
// reproducible on demand: ~1 in 3 shuffled runs, 0 in 18 unshuffled. A probabilistic reproduction
// is not a regression test.
describe("test isolation — localStorage does not outlive a test", () => {
  it("leaves state behind for the next test to find", () => {
    localStorage.setItem("ctxmesh.session.token", "leaked-from-the-previous-test");
    localStorage.setItem("ctxmesh.theme", "dark");
    expect(localStorage.getItem("ctxmesh.session.token")).toBe("leaked-from-the-previous-test");
  });

  it("starts from a clean store, carrying only the namespace setup pins", () => {
    expect(localStorage.getItem("ctxmesh.session.token")).toBeNull();
    expect(localStorage.getItem("ctxmesh.theme")).toBeNull();
    // setup.ts pins this AFTER clearing — every test runs in a concrete workspace.
    expect(localStorage.getItem("ctxmesh.namespace")).toBe("default");
  });
});
