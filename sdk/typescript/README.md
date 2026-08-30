# ctxmesh — the agentry TypeScript SDK

Typed sugar over the **launcher localhost plane** — at parity with the Python SDK
(`sdk/python/`). The launcher (PID 1) injects a fixed env/port contract into every
agent container describing the localhost platform plane (memory, tools/discovery,
feedback, the model gateway, the OTLP collector) and the run context; this SDK
reads that contract and gives a Node/TS agent author the same ergonomics a Python
author has (ADR 0002, ADR 0070).

The SDK is **optional**: every capability it wraps is also a raw localhost endpoint,
so the raw contract stays first-class (ADR 0002). It is **not published to npm** in
v1 — it is vendored into the `base-node` agent image, mirroring the Python SDK's
not-on-PyPI posture.

## Status

Foundation (M77.1): the package scaffold + toolchain, `PlaneConfig` (the exact
env/port contract), the typed error hierarchy, and a `node:http` mock launcher
plane for offline tests. The data-plane clients (memory/knowledge/tools+MCP/
feedback/model), tracing (OTel-JS/OpenInference), `serve()` + the managed loop,
and the base-node image land in M77.2–M77.6.

## Toolchain

Pinned to the console's Node story (`ui/`): Node **22** (`.nvmrc`), pnpm
**9.15.0** (`packageManager`), TypeScript **5.7.2**, vitest **2.1.8**. Bootstrapped
on a clean host + CI through `hack/sdk-ts.sh` (nvm + corepack pnpm), the sibling of
`hack/ui-node.sh`.

```sh
make sdk-ts-build   # tsc -b  → dist/ (ESM + CJS + .d.ts)
make sdk-ts-test    # vitest run
make sdk-ts-lint    # eslint + tsc --noEmit
```

All three are folded into the harness `tier0` gate (via `make lint` / `make test`),
so the SDK is lint + typecheck + test-gated with the rest of the tree.

## Layout

```
sdk/typescript/
  package.json          name "ctxmesh", private, ESM+CJS export map
  tsconfig*.json         library build → dist/ (ESM + CJS + declarations)
  eslint.config.js       flat config, mirrors ui/ (minus React)
  vitest.config.ts
  .nvmrc                 22
  src/
    config.ts            PlaneConfig / RunContext — the env/port contract
    errors.ts            the typed error hierarchy
    index.ts             public entry
  test/
    plane.ts             the node:http mock launcher plane (stubs + fixtures)
    config.test.ts       fromEnv parity (markers, port defaults, gates)
    errors.test.ts       the error types + instanceof
```
