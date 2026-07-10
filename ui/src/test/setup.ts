import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// jsdom teardown between component tests so DOM state does not leak.
afterEach(() => {
  cleanup();
});
