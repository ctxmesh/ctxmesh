# agent-engine UI

A Vite + React + TypeScript single-page app (SPA) for the agent-engine control
plane. It compiles to **static assets** (`dist/`) served by the Go BFF — there
is **no Node runtime** in production; Node is a build-time-only dependency.

Stack: Vite, React 18, TypeScript, Tailwind CSS, shadcn/ui primitives,
React Flow (`@xyflow/react`), React Router, Vitest + Testing Library, ESLint.

## Toolchain (Node is build-time only)

Node is managed by **nvm** and pinned by [`.nvmrc`](./.nvmrc); dependencies are
pinned by [`pnpm-lock.yaml`](./pnpm-lock.yaml). Everything bootstraps
deterministically on a clean host (no node/pnpm/nvm) through the repo-root
`make` targets, which call `hack/ui-node.sh`:

```sh
make ui-deps      # install nvm (if absent) + node from .nvmrc + pnpm + frozen install
make ui-lint      # eslint + tsc typecheck   (also runs inside `make lint`)
make ui-test      # vitest run               (also runs inside `make test`)
make ui-build     # vite build -> ui/dist    (also feeds the BFF image)
make ui-versions  # print the resolved node + pnpm
```

`make lint` and `make test` (and therefore the harness **tier0**) run the UI
eslint + vitest alongside the Go and Python suites.

Local dev (talks to a BFF on `:9090` via the Vite `/api` proxy):

```sh
make ui-deps
./hack/ui-node.sh run dev
```

## The design-token system (the brand)

The theme is a **single design-token set** — the brand lives in **one file**:

> **`src/styles/tokens.css`** — the single source of truth for the theme.

It defines CSS custom properties for color, typography, radius, and shadow (plus
a `.dark` override). [`tailwind.config.ts`](./tailwind.config.ts) maps those
tokens into Tailwind's theme, and the shadcn/ui primitives in
`src/components/ui/` consume the **semantic** utilities (`bg-primary`,
`text-foreground`, `rounded-lg`, `shadow-card`, …).

**To re-brand the whole UI, change the values in `tokens.css`.** Nothing else
needs to change — every surface re-themes by construction. Surfaces must **never
hardcode colors**; they compose the token utilities and the shadcn primitives.

The current values are a neutral, professional slate + indigo system —
placeholders for the real brand, refined at the token layer.

## Layout

```
ui/
  index.html                 SPA entry
  src/
    main.tsx                 React root + router
    App.tsx                  route table (dashboard / agents / config / playground)
    index.css                Tailwind entry (imports the tokens)
    styles/tokens.css        >>> THE BRAND TOKENS <<<
    lib/
      api.ts                 typed client for the Go BFF (/api/*)
      utils.ts               cn() class-merge helper
    components/
      app-shell.tsx          persistent sidebar/header layout
      ui/                    shadcn/ui token primitives (button, card, badge)
    pages/
      dashboard-page.tsx     landing surface (renders BFF /api/health)
      agents-page.tsx        foundation proof — renders GET /api/agents
      placeholder-page.tsx   stub for surfaces that arrive later
    test/setup.ts            vitest + jsdom setup
```

Surface implementations (dashboard topology/cost, config-builder, Playground)
build on this foundation in later tasks.
