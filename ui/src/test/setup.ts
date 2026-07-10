import "@testing-library/jest-dom/vitest";
import { afterEach, vi } from "vitest";
import { cleanup } from "@testing-library/react";

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
