// visual.spec.ts — render every route, at every width, in both themes (M151).
//
// 49 routes × 4 widths × 2 themes = 392 renders. Each one:
//   1. seeds a session token so the app is signed in (no backend runs),
//   2. intercepts every /api/** call and answers it from visual/fixtures.ts,
//   3. forces the theme,
//   4. waits for the page to settle,
//   5. asserts nothing overflows (visual/checks.ts),
//   6. writes a screenshot for a person to read.
//
// Step 6 is not decoration. A passing assertion is not the same as a page that
// looks right, and the milestone's polish task is driven by reading these.

import { test, expect } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";
import { ROUTES, WIDTHS, THEMES, type Theme, type RouteCase } from "./routes";
import { collectFitResult, type FitResult } from "./checks";
import { resolveFixture, type FixtureMode } from "./fixtures";

const MODE = (process.env.VISUAL_MODE ?? "populated") as FixtureMode;
const LABEL = process.env.VISUAL_LABEL ?? MODE;
const SHOT_DIR = path.resolve("visual/shots", LABEL);
const REPORT_DIR = path.resolve("visual/report");

// sessionStorage slot the app reads its bearer token from (src/lib/session.ts).
const SESSION_KEY = "ctxmesh.session.token";

interface Row extends FitResult {
  route: string;
  id: string;
  label: string;
  archetype: string;
  width: number;
  theme: Theme;
  shot: string;
  consoleErrors: string[];
}

const rows: Row[] = [];

test.beforeAll(() => {
  fs.mkdirSync(SHOT_DIR, { recursive: true });
  fs.mkdirSync(REPORT_DIR, { recursive: true });
});

test.afterAll(() => {
  // Each worker writes its OWN shard; global-teardown merges them.
  //
  // This used to read-modify-write one shared file, which is a lost update
  // across processes: with four workers a sweep silently reported 18 of 24
  // renders while 24 screenshots sat on disk. A gate that under-reports is
  // worse than one that fails — it says PASS for work it never looked at.
  const dir = path.join(REPORT_DIR, ".shards");
  fs.mkdirSync(dir, { recursive: true });
  const worker = process.env.TEST_WORKER_INDEX ?? "0";
  fs.writeFileSync(path.join(dir, `${LABEL}.${worker}.json`), JSON.stringify(rows));
});


for (const route of ROUTES) {
  for (const width of WIDTHS) {
    for (const theme of THEMES) {
      test(`${route.id} @ ${width} ${theme}`, async ({ page }) => {
        const consoleErrors: string[] = [];
        page.on("console", (m) => {
          if (m.type() === "error") consoleErrors.push(m.text().slice(0, 200));
        });
        page.on("pageerror", (e) => consoleErrors.push(`pageerror: ${String(e).slice(0, 200)}`));

        await page.setViewportSize({ width, height: 1000 });

        // Sign in and pin the theme before any app code runs.
        //
        // An init script runs at document-start, when `document.documentElement`
        // may not exist yet — touching it unguarded throws, the whole script
        // dies, and the theme silently never applies. (That exact bug made the
        // first baseline run render light twice and report it as two themes.)
        await page.addInitScript(
          ([key, wantDark]) => {
            try {
              sessionStorage.setItem(key as string, "visual-sweep-token");
            } catch {
              /* private mode — the app treats this as signed out; the test will show it */
            }
            const dark = wantDark as boolean;
            const apply = () => {
              document.documentElement?.classList.toggle("dark", dark);
            };
            const arm = () => {
              apply();
              if (!document.documentElement) return;
              // The app may rewrite the root class on boot; keep it pinned.
              new MutationObserver(apply).observe(document.documentElement, {
                attributes: true,
                attributeFilter: ["class"],
              });
            };
            apply();
            if (document.readyState === "loading") {
              document.addEventListener("DOMContentLoaded", arm, { once: true });
            } else {
              arm();
            }
          },
          [SESSION_KEY, theme === "dark"] as const,
        );

        await page.route("**/api/**", async (r) => {
          const url = new URL(r.request().url());
          const { status, body } = resolveFixture(
            url.pathname,
            url.search,
            r.request().method(),
            MODE,
          );
          await r.fulfill({
            status,
            contentType: "application/json",
            body: JSON.stringify(body),
          });
        });

        // Anything else external (fonts, telemetry) is aborted so a slow or
        // absent network cannot make a layout look different run to run.
        await page.route(/^https?:\/\/(?!127\.0\.0\.1|localhost)/, (r) => r.abort());

        await page.goto(route.path, { waitUntil: "domcontentloaded" });
        await page.waitForLoadState("networkidle").catch(() => {
          /* a page holding an SSE/stream open never goes idle; that is fine */
        });
        // Let layout settle after data lands.
        await page.waitForTimeout(400);

        const fit = (await page.evaluate(collectFitResult)) as FitResult;

        const shot = path.join(SHOT_DIR, `${route.id}__${width}__${theme}.png`);
        await page.screenshot({ path: shot, fullPage: true });

        rows.push({
          ...fit,
          route: route.path,
          id: route.id,
          label: route.label,
          archetype: route.archetype,
          width,
          theme,
          shot: path.relative(process.cwd(), shot),
          consoleErrors,
        });

        // The gate. Baseline runs record failures instead of failing, so the
        // pre-redesign state can be captured honestly; the redesign must pass.
        if (process.env.VISUAL_BASELINE !== "1") {
          expect(
            fit.documentScrollsX,
            `${route.path} @ ${width}/${theme} scrolls sideways by ${fit.documentOverflowPx}px`,
          ).toBe(false);
          expect(
            fit.offenders,
            `${route.path} @ ${width}/${theme} has elements that do not fit:\n` +
              fit.offenders.map((o) => `  • ${o.selector} (+${o.overflowPx}px, ${o.reason}) "${o.text}"`).join("\n"),
          ).toEqual([]);
        }
      });
    }
  }
}

// The inventory must not drift from the app's route table.
test("route inventory covers every route in App.tsx", async () => {
  const src = fs.readFileSync(path.resolve("src/App.tsx"), "utf8");
  const declared = Array.from(src.matchAll(/<Route\s+(?:[^>]*?\s)?path="([^"]+)"/g)).map((m) => m[1]);
  const missing = declared.filter((p) => {
    if (p === "*" || p.startsWith("design")) return false; // catch-all + flagged gallery
    if (p === "tools/mcp-catalog") return false; // a <Navigate> redirect, not a surface
    const literal = "/" + p.replace(/:[^/]+/g, "");
    return !ROUTES.some((r: RouteCase) => {
      const rp = r.path.replace(/\/[^/]*$/, "/");
      return r.path === "/" + p || rp.startsWith(literal.replace(/\/$/, "")) || r.path.startsWith(literal.replace(/\/+$/, ""));
    });
  });
  expect(missing, `App.tsx declares routes the visual sweep does not cover: ${missing.join(", ")}`).toEqual([]);
});
