import js from "@eslint/js";
import globals from "globals";
import tseslint from "typescript-eslint";

// Flat ESLint config (ESLint 9), mirroring ui/eslint.config.js (minus the React
// plugins — this is a Node library, not a browser SPA). Runs in `make sdk-ts-lint`
// (folded into tier0) via `pnpm lint` -> `eslint .`. Fails the target on any error.
export default tseslint.config(
  { ignores: ["dist", "node_modules", "coverage"] },
  {
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    files: ["**/*.ts"],
    languageOptions: {
      ecmaVersion: 2022,
      sourceType: "module",
      globals: globals.node,
    },
    rules: {
      // Honor the `_`-prefix convention for intentionally-unused bindings (mirrors
      // the ui config) — e.g. stub route handlers whose params exist only to shape
      // the recorded-request assertions.
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
    // Test + config files may use Node globals + the vitest globals.
    files: ["**/*.{test,spec}.ts", "*.config.{ts,js,mjs}", "scripts/**", "test/**"],
    languageOptions: {
      globals: { ...globals.node },
    },
  },
);
