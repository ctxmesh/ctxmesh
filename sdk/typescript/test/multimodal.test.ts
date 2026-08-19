/**
 * Multimodal message-content helpers (O6) — surface parity with `sdk/python/src/ctxmesh/_multimodal.py`.
 * The wire output must be byte-identical to the Python helpers (OpenAI content-parts shape).
 */

import { describe, it, expect } from "vitest";

import { textPart, imageUrl, content } from "../src/_multimodal.js";

describe("multimodal helpers (O6)", () => {
  it("textPart returns the OpenAI text part", () => {
    expect(textPart("Hello, world!")).toEqual({ type: "text", text: "Hello, world!" });
  });

  it("imageUrl returns the OpenAI image_url part (no detail by default)", () => {
    expect(imageUrl("https://example.com/photo.jpg")).toEqual({
      type: "image_url",
      image_url: { url: "https://example.com/photo.jpg" },
    });
  });

  it("imageUrl includes detail when given", () => {
    expect(imageUrl("https://example.com/photo.jpg", "high")).toEqual({
      type: "image_url",
      image_url: { url: "https://example.com/photo.jpg", detail: "high" },
    });
  });

  it("imageUrl passes a data: inline URL straight through", () => {
    const data = "data:image/png;base64,iVBORw0KGgo=";
    expect(imageUrl(data)).toEqual({ type: "image_url", image_url: { url: data } });
  });

  it("content assembles positional parts", () => {
    expect(
      content(textPart("hi"), imageUrl("https://x.com/img.png")),
    ).toEqual([
      { type: "text", text: "hi" },
      { type: "image_url", image_url: { url: "https://x.com/img.png" } },
    ]);
  });

  it("content accepts a single array argument", () => {
    const parts = [textPart("a"), textPart("b")];
    expect(content(parts)).toEqual(parts);
    // …and returns a COPY, not the same array reference.
    expect(content(parts)).not.toBe(parts);
  });

  it("content flattens nested arrays one level", () => {
    expect(content(textPart("a"), [textPart("b"), textPart("c")])).toEqual([
      { type: "text", text: "a" },
      { type: "text", text: "b" },
      { type: "text", text: "c" },
    ]);
  });
});
