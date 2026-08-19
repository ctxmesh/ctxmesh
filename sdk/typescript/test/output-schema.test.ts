/**
 * In-loop outputSchema validate + repair (O4) — parity with the Python SDK (m65.5). These are
 * internal exports (not on the package surface); the test imports them from the module. The validator
 * is RE-ASK-ONLY (never hard-fails), on the draft-2020-12 dialect matching the authoritative
 * server-side + Python validators.
 */

import { describe, expect, it } from "vitest";

import {
  compileOutputSchema,
  validateAgainstSchema,
  MAX_SCHEMA_REPAIR,
} from "../src/managed.js";

const schema = {
  type: "object",
  required: ["name", "age"],
  properties: {
    name: { type: "string" },
    age: { type: "number" },
  },
} as Record<string, unknown>;

describe("compileOutputSchema (O4)", () => {
  it("returns null for a null schema (validation off)", () => {
    expect(compileOutputSchema(null)).toBeNull();
  });

  it("compiles a valid schema", () => {
    expect(compileOutputSchema(schema)).toBeInstanceOf(Function);
  });

  it("returns null (skip) for an UNCOMPILABLE schema — never throws", () => {
    // `type` must be a string/array of strings; a number is invalid → ajv compile throws → we skip.
    expect(compileOutputSchema({ type: 123 } as Record<string, unknown>)).toBeNull();
  });
});

describe("validateAgainstSchema (O4)", () => {
  const validate = compileOutputSchema(schema)!;

  it("returns null when the answer conforms", () => {
    expect(validateAgainstSchema(`{"name":"Ada","age":36}`, validate)).toBeNull();
  });

  it("returns an error message naming the violation", () => {
    const err = validateAgainstSchema(`{"name":"Ada"}`, validate);
    expect(err).not.toBeNull();
    expect(err).toContain("age");
  });

  it("treats a non-JSON answer as a violation", () => {
    expect(validateAgainstSchema("not json at all", validate)).toContain("not valid JSON");
  });

  it("reports MULTIPLE violations in one message (allErrors)", () => {
    // both fields wrong type
    const err = validateAgainstSchema(`{"name":123,"age":"old"}`, validate) ?? "";
    expect(err).toContain("name");
    expect(err).toContain("age");
  });

  it("uses draft 2020-12 keywords (prefixItems)", () => {
    // prefixItems is a 2020-12 keyword — on the default draft-07 ajv it would be ignored.
    const tupleSchema = compileOutputSchema({
      type: "array",
      prefixItems: [{ type: "string" }, { type: "number" }],
      items: false,
    } as Record<string, unknown>)!;
    expect(validateAgainstSchema(`["a", 1]`, tupleSchema)).toBeNull();
    expect(validateAgainstSchema(`["a", "b"]`, tupleSchema)).not.toBeNull();
  });
});

describe("MAX_SCHEMA_REPAIR (O4)", () => {
  it("matches the Python bound (2)", () => {
    expect(MAX_SCHEMA_REPAIR).toBe(2);
  });
});
