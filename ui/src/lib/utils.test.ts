import { describe, expect, it } from "vitest";

import { cn } from "@/lib/utils";

describe("cn", () => {
  it("merges conditional classes", () => {
    const disabled = false;
    expect(cn("a", disabled && "b", "c")).toBe("a c");
  });

  it("de-duplicates conflicting tailwind utilities (last wins)", () => {
    // tailwind-merge must resolve conflicting bg-* to the last one so
    // token-based overrides behave predictably in components.
    expect(cn("bg-primary", "bg-secondary")).toBe("bg-secondary");
  });
});
