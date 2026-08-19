// Type augmentation for the vitest-axe `toHaveNoViolations` matcher (M100 UI99-7). vitest-axe@0.1.0
// ships no `exports` map, so its bundled `matchers.d.ts` subpath does not resolve — we declare the
// matcher on vitest's Assertion here (it is registered at runtime in src/test/setup.ts). Kept minimal
// (the matcher takes no args and is used as `expect(await axe(el)).toHaveNoViolations()`).
import "vitest";

interface AxeMatchers<R = unknown> {
  toHaveNoViolations(): R;
}

// The matcher augmentation is inherently empty-bodied interface merging (the standard vitest
// pattern), which trips no-empty-object-type — disable it for this declaration block.
/* eslint-disable @typescript-eslint/no-empty-object-type, @typescript-eslint/no-explicit-any */
declare module "vitest" {
  interface Assertion<T = any> extends AxeMatchers<T> {}
  interface AsymmetricMatchersContaining extends AxeMatchers {}
}
/* eslint-enable @typescript-eslint/no-empty-object-type, @typescript-eslint/no-explicit-any */
