import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

// Flat ESLint config (ESLint 9). Runs in `make lint` (tier0) via
// `pnpm lint` -> `eslint .`. Fails the target on any error.
export default tseslint.config(
  { ignores: ["dist", "node_modules", "coverage"] },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": [
        "warn",
        { allowConstantExport: true },
      ],
      // Honor the `_`-prefix convention for intentionally-unused bindings — e.g. a
      // typed mock whose params exist only to shape `mock.calls[0]` (oidc.test.ts).
      "@typescript-eslint/no-unused-vars": [
        "error",
        {
          argsIgnorePattern: "^_",
          varsIgnorePattern: "^_",
          caughtErrorsIgnorePattern: "^_",
        },
      ],
    },
  },
  {
    // Test + config files may use Node globals.
    files: ["**/*.{test,spec}.{ts,tsx}", "*.config.{ts,js}", "src/test/**"],
    languageOptions: {
      globals: { ...globals.node, ...globals.browser },
    },
  },
  {
    // shadcn/ui primitives co-locate a component with its cva variants
    // (the canonical shadcn pattern); the react-refresh "components-only
    // export" rule does not apply to these hand-vendored primitives. The same
    // exception covers the scale-primitive kit (components + a co-located hook
    // like useCommandK / variant maps — m13.1, spec §5), the /design gallery
    // scaffolding (design-time helpers alongside their wireframe components —
    // a review-only surface, never hot-reloaded in production), and the RBAC
    // chrome context providers (m13.5), which by design co-locate a Provider
    // component with its consumer hooks (useCapabilities/useCan/Can,
    // useNamespace) — the standard React context pattern.
    files: [
      "src/components/ui/**/*.{ts,tsx}",
      "src/components/kit/**/*.{ts,tsx}",
      "src/design/**/*.{ts,tsx}",
      "src/lib/capabilities.tsx",
      "src/lib/namespace.tsx",
      // Same pattern again (M151): ThemeProvider + useTheme co-located with the
      // pure helpers that read/apply the theme, which the chrome-less routes and
      // the module-load bootstrap call without mounting a component.
      "src/lib/theme.tsx",
    ],
    rules: {
      "react-refresh/only-export-components": "off",
    },
  },
);
