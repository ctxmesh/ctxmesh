import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach, expect, vi } from "vitest";
import { cleanup } from "@testing-library/react";
// Register vitest-axe's `toHaveNoViolations` matcher for the WCAG 2.1 AA a11y gate (M100 UI99,
// ADR-locked target). vitest-axe@0.1.0 ships no `exports` map, so its `extend-expect` subpath does
// not resolve — we extend expect from the matchers module directly. NOTE: under jsdom axe cannot
// compute color-contrast (no layout engine) — the automated gate covers STRUCTURAL a11y (accessible
// names, roles, aria, landmarks, table/list structure); contrast/focus-visible is verified on the
// live visual loop (carded, m52.UI99-layout).
import { toHaveNoViolations } from "vitest-axe/dist/matchers.js";

expect.extend({ toHaveNoViolations });

// jsdom teardown between component tests so DOM state does not leak.
afterEach(() => {
  cleanup();
});

// Every test starts in a CONCRETE workspace, because the console does.
//
// The shell's read scope used to default to "" ("All workspaces"), and a component test that
// never stubbed /api/namespaces inherited it. That is not a neutral default: "" makes every
// namespaced request cluster-wide, which a caller bound per-namespace cannot satisfy, and it
// makes the capability probe ask "may I do this in EVERY namespace at once" — whose definite
// all-false was then read as a verdict about the user. Dozens of viewer-chrome tests passed
// only because the console was in that state, so the suite agreed with a console that told a
// fully entitled user they had no permissions (M177).
//
// Pinning a namespace here keeps each test in the scope a real session has. A test that is
// specifically about the all-workspaces scope sets it back explicitly.
beforeEach(() => {
  localStorage.setItem("ctxmesh.namespace", "default");
});

// React Flow (@xyflow/react) needs a handful of browser APIs jsdom does not
// implement (ResizeObserver, DOMMatrixReadOnly, element measurement). Stub them
// so the topology graph mounts under vitest — the dashboard's render proof.
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}
if (!globalThis.ResizeObserver) {
  globalThis.ResizeObserver =
    ResizeObserverStub as unknown as typeof ResizeObserver;
}

class DOMMatrixStub {
  m22 = 1;
  constructor() {}
}
if (!globalThis.DOMMatrixReadOnly) {
  globalThis.DOMMatrixReadOnly =
    DOMMatrixStub as unknown as typeof DOMMatrixReadOnly;
}

if (!Element.prototype.getBoundingClientRect) {
  Element.prototype.getBoundingClientRect = vi.fn(
    () => ({ width: 800, height: 600, top: 0, left: 0, right: 800, bottom: 600, x: 0, y: 0, toJSON: () => ({}) }) as DOMRect,
  );
}
