import "@testing-library/jest-dom/vitest";
import { afterEach, expect, vi } from "vitest";
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
