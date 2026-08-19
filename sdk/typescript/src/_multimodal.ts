// Multimodal message-content helpers (O6) — the TS parity for the Python SDK's `_multimodal.py`.
//
// They assemble the content-parts array that `model.chat` accepts as a message's `content` field.
// The model gateway and provider relay content-parts VERBATIM, so these are purely client-side
// conveniences — users don't hand-assemble the OpenAI wire format. Both `https://` URLs and `data:`
// URLs (base64-encoded inline images) are accepted; they pass straight through (the gateway does not
// inspect the URL type). Function names are camelCase (TS convention); the WIRE output keeps OpenAI's
// `type: "image_url"` / `image_url: {...}` shape — byte-identical to the Python helpers' output.
//
// Usage:
//   import { textPart, imageUrl, content } from "ctxmesh";
//   const messages = [
//     { role: "user", content: content(
//       textPart("What is in this image?"),
//       imageUrl("https://example.com/photo.jpg"),
//     ) },
//   ];
//   const res = await client.model.chat("gpt-4o", messages);

/** A text content-part: `{ type: "text", text }`. */
export interface TextPart {
  type: "text";
  text: string;
}

/** An image content-part in the OpenAI wire shape: `{ type: "image_url", image_url: { url, detail? } }`. */
export interface ImageUrlPart {
  type: "image_url";
  image_url: { url: string; detail?: string };
}

/** One assembled content-part. */
export type ContentPart = TextPart | ImageUrlPart;

/** textPart("Hello") → `{ type: "text", text: "Hello" }`. */
export function textPart(text: string): TextPart {
  return { type: "text", text };
}

/**
 * imageUrl(url, detail?) → `{ type: "image_url", image_url: { url, detail? } }`.
 * `url` may be an `https://` URL or a `data:image/...;base64,...` inline URL. `detail` is the optional
 * OpenAI detail level ("low" | "high" | "auto"); omit it to let the provider use its default.
 */
export function imageUrl(url: string, detail?: string): ImageUrlPart {
  const inner: { url: string; detail?: string } = { url };
  if (detail !== undefined) inner.detail = detail;
  return { type: "image_url", image_url: inner };
}

/**
 * content(...) assembles the content-parts array for a message's `content` field. Accepts either
 * multiple positional parts — `content(textPart("…"), imageUrl("…"))` — or a single array —
 * `content([textPart("…"), imageUrl("…")])`. Nested arrays are flattened one level.
 */
export function content(
  ...parts: (ContentPart | ContentPart[])[]
): ContentPart[] {
  if (parts.length === 1 && Array.isArray(parts[0])) {
    return [...parts[0]];
  }
  const result: ContentPart[] = [];
  for (const part of parts) {
    if (Array.isArray(part)) result.push(...part);
    else result.push(part);
  }
  return result;
}
