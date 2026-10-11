import { beforeEach, describe, expect, it, vi } from "vitest";

import { firstRunPlaygroundLink, hasCompletedRun, markRunCompleted } from "./first-run";

describe("first-run latch", () => {
  beforeEach(() => {
    window.localStorage.clear();
    vi.restoreAllMocks();
  });

  it("is false until a run completes", () => {
    expect(hasCompletedRun("team-a")).toBe(false);
    markRunCompleted("team-a");
    expect(hasCompletedRun("team-a")).toBe(true);
  });

  it("is scoped per workspace — a run in one namespace does not check the step off in another", () => {
    markRunCompleted("team-a");
    expect(hasCompletedRun("team-b")).toBe(false);
    expect(hasCompletedRun("")).toBe(false);
  });

  it("treats the empty namespace as the all-workspaces scope, not as team-a", () => {
    markRunCompleted("");
    expect(hasCompletedRun("")).toBe(true);
    expect(hasCompletedRun("team-a")).toBe(false);
  });

  it("namespaces its key so it cannot collide with another page's storage", () => {
    markRunCompleted("team-a");
    const keys = Object.keys(window.localStorage);
    expect(keys).toHaveLength(1);
    expect(keys[0]).toBe("ctxmesh.firstRun.ranAgent:team-a");
  });

  // Safari private mode throws on localStorage access. A console that cannot render Home because
  // a checklist lookup threw would be an absurd way to lose the product, so both sides degrade.
  it("degrades to the feed's answer when storage throws", () => {
    vi.spyOn(window.localStorage.__proto__, "setItem").mockImplementation(() => {
      throw new Error("QuotaExceededError");
    });
    vi.spyOn(window.localStorage.__proto__, "getItem").mockImplementation(() => {
      throw new Error("SecurityError");
    });
    expect(() => markRunCompleted("team-a")).not.toThrow();
    expect(hasCompletedRun("team-a")).toBe(false);
  });
});

describe("firstRunPlaygroundLink", () => {
  it("picks the newest agent by creation time", () => {
    expect(
      firstRunPlaygroundLink(
        [
          { name: "a", namespace: "ns1", createdAt: "2026-10-01T00:00:00Z" },
          { name: "b", namespace: "ns2", createdAt: "2026-10-02T00:00:00Z" },
        ],
        "/playground",
      ),
    ).toBe("/playground?agent=b&ns=ns2");
  });

  it("uses a lone agent even without a creation time", () => {
    expect(firstRunPlaygroundLink([{ name: "solo", namespace: "default" }], "/playground")).toBe(
      "/playground?agent=solo&ns=default",
    );
  });

  it("falls back to the picker when it cannot tell which agent is new", () => {
    expect(
      firstRunPlaygroundLink(
        [
          { name: "a", namespace: "x" },
          { name: "b", namespace: "x" },
        ],
        "/playground",
      ),
    ).toBe("/playground");
    expect(firstRunPlaygroundLink([], "/playground")).toBe("/playground");
  });
});
