// tokens.contrast.test.ts — the palette is legible, in both themes (M151).
//
// A design token file can be internally consistent and still unreadable. This
// test parses tokens.css directly and computes the actual WCAG contrast ratio
// for every foreground/surface pair the UI puts text on, in BOTH themes, so a
// palette edit that looks fine on the author's monitor cannot ship a caption
// nobody can read.
//
// It reads the CSS rather than a duplicated table in TypeScript on purpose: a
// second copy of the palette is a second thing to forget to update.

import { describe, expect, it } from "vitest";
import fs from "node:fs";
import path from "node:path";

const CSS = fs.readFileSync(path.resolve(__dirname, "tokens.css"), "utf8");

/** WCAG 2.2 minimums. Body text and UI labels are "normal" text; non-text UI
 *  (borders, focus rings, control boundaries) is WCAG 1.4.11 at 3:1. */
const NORMAL_TEXT = 4.5;
const NON_TEXT = 3.0;

type Pairs = { fg: string; bg: string; min: number; note: string }[];

// ── Known failures in the PRE-REDESIGN palette (M151 baseline) ──────────────
// The M12/m13.1 indigo palette fails six of these pairs. They are recorded here
// with their measured ratio rather than deleted or the threshold lowered, so:
//   • the failures are visible in the repo instead of in nobody's memory,
//   • a further regression still fails the build (each entry is a floor), and
//   • m151.3 empties this map when the editorial palette lands — the test below
//     fails if an entry is fixed but left here, so it cannot silently rot.
// DELETING AN ENTRY IS THE FIX. Raising a number here is not.
const KNOWN_BELOW: Record<string, number> = {
  "light:muted-foreground on surface-2": 4.27,
  "light:success-foreground on success": 3.14,
  "light:info-foreground on info": 3.58,
  "light:border-strong on background": 1.54,
  "dark:destructive-foreground on destructive": 4.01,
  "dark:border-strong on background": 2.05,
};

// Every place the UI paints text or a meaningful boundary. Keep this list in
// step with tailwind.config.ts: a token that surfaces text and is not listed
// here is simply unchecked.
const PAIRS: Pairs = [
  { fg: "foreground", bg: "background", min: NORMAL_TEXT, note: "body text on the page ground" },
  { fg: "foreground", bg: "card", min: NORMAL_TEXT, note: "body text on a panel" },
  { fg: "foreground", bg: "surface-2", min: NORMAL_TEXT, note: "body text on a raised row" },
  { fg: "foreground", bg: "surface-3", min: NORMAL_TEXT, note: "body text in an inset well" },
  { fg: "muted-foreground", bg: "background", min: NORMAL_TEXT, note: "captions and meta on the ground" },
  { fg: "muted-foreground", bg: "card", min: NORMAL_TEXT, note: "captions and meta on a panel" },
  { fg: "muted-foreground", bg: "surface-2", min: NORMAL_TEXT, note: "captions on a raised row" },
  { fg: "primary", bg: "background", min: NORMAL_TEXT, note: "links and the accent as text" },
  { fg: "primary", bg: "card", min: NORMAL_TEXT, note: "links on a panel" },
  { fg: "primary-foreground", bg: "primary", min: NORMAL_TEXT, note: "label inside a primary button" },
  { fg: "secondary-foreground", bg: "secondary", min: NORMAL_TEXT, note: "label inside a secondary button" },
  { fg: "accent-foreground", bg: "accent", min: NORMAL_TEXT, note: "label on an accent surface" },
  { fg: "destructive-foreground", bg: "destructive", min: NORMAL_TEXT, note: "label inside a destructive button" },
  { fg: "success-foreground", bg: "success", min: NORMAL_TEXT, note: "success badge label" },
  { fg: "warning-foreground", bg: "warning", min: NORMAL_TEXT, note: "warning badge label" },
  { fg: "info-foreground", bg: "info", min: NORMAL_TEXT, note: "info badge label" },
  { fg: "popover-foreground", bg: "popover", min: NORMAL_TEXT, note: "text in a menu or dialog" },
  { fg: "card-foreground", bg: "card", min: NORMAL_TEXT, note: "panel text" },
  { fg: "border-strong", bg: "background", min: NON_TEXT, note: "a boundary that must be seen" },
  { fg: "ring", bg: "background", min: NON_TEXT, note: "the keyboard focus ring" },
];

/** Pull `--name: H S% L%;` declarations out of one CSS block. */
function parseBlock(selector: string): Record<string, string> {
  // Match the selector's block, tolerating the @layer indentation in tokens.css.
  const re = new RegExp(`${selector.replace(/[.*+?^${}()|[\\]\\\\]/g, "\\\\$&")}\\s*\\{([\\s\\S]*?)\\n\\s*\\}`, "m");
  const m = CSS.match(re);
  if (!m) throw new Error(`tokens.css has no ${selector} block`);
  const out: Record<string, string> = {};
  for (const decl of m[1].matchAll(/--([a-z0-9-]+)\s*:\s*([^;]+);/gi)) {
    out[decl[1]] = decl[2].trim();
  }
  return out;
}

/** "220 24% 97%" → sRGB 0..1 triple. */
function hslChannelsToRgb(value: string): [number, number, number] | null {
  const m = value.match(/^([\d.]+)\s+([\d.]+)%\s+([\d.]+)%$/);
  if (!m) return null;
  const h = parseFloat(m[1]) / 360;
  const s = parseFloat(m[2]) / 100;
  const l = parseFloat(m[3]) / 100;
  if (s === 0) return [l, l, l];
  const q = l < 0.5 ? l * (1 + s) : l + s - l * s;
  const p = 2 * l - q;
  const hue = (t: number) => {
    let x = t;
    if (x < 0) x += 1;
    if (x > 1) x -= 1;
    if (x < 1 / 6) return p + (q - p) * 6 * x;
    if (x < 1 / 2) return q;
    if (x < 2 / 3) return p + (q - p) * (2 / 3 - x) * 6;
    return p;
  };
  return [hue(h + 1 / 3), hue(h), hue(h - 1 / 3)];
}

function luminance([r, g, b]: [number, number, number]): number {
  const lin = (c: number) => (c <= 0.03928 ? c / 12.92 : Math.pow((c + 0.055) / 1.055, 2.4));
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

function ratio(a: [number, number, number], b: [number, number, number]): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

describe.each([
  ["light", ":root"],
  ["dark", ".dark"],
])("token contrast — %s theme", (themeName, selector) => {
  const tokens = parseBlock(selector);

  it.each(PAIRS.map((p) => [`${p.fg} on ${p.bg}`, p] as const))("%s", (_label, pair) => {
    const fgRaw = tokens[pair.fg];
    const bgRaw = tokens[pair.bg];
    // A token the theme does not define is a real gap, not a skip: the utility
    // still resolves to nothing and the element paints unstyled.
    expect(fgRaw, `${selector} defines no --${pair.fg}`).toBeDefined();
    expect(bgRaw, `${selector} defines no --${pair.bg}`).toBeDefined();

    const fg = hslChannelsToRgb(fgRaw);
    const bg = hslChannelsToRgb(bgRaw);
    expect(fg, `--${pair.fg} is not bare HSL channels: "${fgRaw}"`).not.toBeNull();
    expect(bg, `--${pair.bg} is not bare HSL channels: "${bgRaw}"`).not.toBeNull();

    const r = Number(ratio(fg!, bg!).toFixed(2));
    const key = `${themeName}:${pair.fg} on ${pair.bg}`;
    const floor = KNOWN_BELOW[key];

    if (floor !== undefined) {
      // A recorded pre-redesign failure: hold the line, do not let it worsen.
      expect(
        r,
        `${key} is a known M151 baseline failure at ${floor}:1 and has got WORSE (${r}:1)`,
      ).toBeGreaterThanOrEqual(floor);
      return;
    }

    expect(
      r,
      `${themeName}: --${pair.fg} on --${pair.bg} is ${r}:1, below ${pair.min}:1 — ${pair.note}`,
    ).toBeGreaterThanOrEqual(pair.min);
  });
});

it("both themes define the same COLOUR tokens", () => {
  // Only colour tokens are compared: :root also carries type, spacing and
  // radius, which are theme-independent by design and are not redeclared under
  // .dark. A COLOUR token defined in one theme only is the real bug — the
  // utility silently inherits the other theme's value and paints the wrong
  // ground.
  const colours = (block: Record<string, string>) =>
    Object.entries(block)
      .filter(([, v]) => hslChannelsToRgb(v) !== null)
      .map(([k]) => k)
      .sort();
  const light = colours(parseBlock(":root"));
  const dark = colours(parseBlock(".dark"));
  expect(
    {
      onlyLight: light.filter((k) => !dark.includes(k)),
      onlyDark: dark.filter((k) => !light.includes(k)),
    },
    "every colour token must be declared in both themes",
  ).toEqual({ onlyLight: [], onlyDark: [] });
});

it("the M151 baseline allowlist contains only pairs that still fail", () => {
  // Keeps KNOWN_BELOW honest: once a pair is fixed it must be removed, or the
  // allowlist quietly becomes a list of thresholds nobody is enforcing.
  const stale: string[] = [];
  for (const [themeName, selector] of [
    ["light", ":root"],
    ["dark", ".dark"],
  ] as const) {
    const tokens = parseBlock(selector);
    for (const pair of PAIRS) {
      const key = `${themeName}:${pair.fg} on ${pair.bg}`;
      if (!(key in KNOWN_BELOW)) continue;
      const fg = hslChannelsToRgb(tokens[pair.fg] ?? "");
      const bg = hslChannelsToRgb(tokens[pair.bg] ?? "");
      if (!fg || !bg) continue;
      if (ratio(fg, bg) >= pair.min) stale.push(key);
    }
  }
  expect(stale, `these pairs now pass — delete them from KNOWN_BELOW: ${stale.join(", ")}`).toEqual([]);
});
