// finalize-cjs.mjs — mark the CJS build output as CommonJS.
//
// The package is ESM ("type": "module"), so Node would otherwise interpret the
// .js files under dist/cjs/ as ESM. Dropping a { "type": "commonjs" } package.json
// into dist/cjs/ overrides that for the require() entry, so both `import ctxmesh`
// (dist/esm) and `require("ctxmesh")` (dist/cjs) resolve correctly. Pure Node
// stdlib — no bundler, mirroring the "no heavy deps" constraint (ADR 0070).
import { writeFileSync, mkdirSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const cjsDir = resolve(here, "..", "dist", "cjs");

mkdirSync(cjsDir, { recursive: true });
writeFileSync(
  resolve(cjsDir, "package.json"),
  JSON.stringify({ type: "commonjs" }, null, 2) + "\n",
);
