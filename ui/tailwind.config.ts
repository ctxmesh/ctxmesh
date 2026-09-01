import type { Config } from "tailwindcss";
import animate from "tailwindcss-animate";

// Tailwind theme = the DESIGN-TOKEN MAPPING (M12, ADR 0010 §"one design-token
// system"). Every value here resolves to a CSS custom property defined in
// src/styles/tokens.css — that file is the SINGLE SOURCE OF TRUTH for the brand
// (colors / radius). To re-brand the whole UI, change the token VALUES in
// tokens.css; do NOT hardcode colors in components. Surfaces (dashboard /
// config-builder / Playground) compose these semantic utilities only.
const config: Config = {
  darkMode: ["class"],
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    container: {
      center: true,
      padding: "2rem",
      screens: { "2xl": "1400px" },
    },
    extend: {
      colors: {
        border: "hsl(var(--border))",
        // Row separators inside a bordered panel — lighter than the panel frame
        // so a table reads as one object with internal divisions (M151 §1.4).
        "border-soft": "hsl(var(--border-soft))",
        "border-strong": "hsl(var(--border-strong))",
        input: "hsl(var(--input))",
        ring: "hsl(var(--ring))",
        background: "hsl(var(--background))",
        foreground: "hsl(var(--foreground))",
        // Plane hierarchy — raised rows / inset wells above the base bg.
        "surface-2": "hsl(var(--surface-2))",
        "surface-3": "hsl(var(--surface-3))",
        "brand-2": "hsl(var(--brand-2))",
        primary: {
          DEFAULT: "hsl(var(--primary))",
          foreground: "hsl(var(--primary-foreground))",
        },
        secondary: {
          DEFAULT: "hsl(var(--secondary))",
          foreground: "hsl(var(--secondary-foreground))",
        },
        destructive: {
          DEFAULT: "hsl(var(--destructive))",
          foreground: "hsl(var(--destructive-foreground))",
          surface: "hsl(var(--destructive-surface))",
        },
        muted: {
          DEFAULT: "hsl(var(--muted))",
          foreground: "hsl(var(--muted-foreground))",
        },
        // The ink ramp below muted-foreground. `faint` is tertiary meta that must
        // still be READ (timestamps, eyebrows, column heads) and clears WCAG.
        // `ghost` is decoration only — disabled text, placeholders, pips — and is
        // deliberately contrast-exempt. Never put information in `ghost`.
        faint: "hsl(var(--faint))",
        ghost: "hsl(var(--ghost))",
        accent: {
          DEFAULT: "hsl(var(--accent))",
          foreground: "hsl(var(--accent-foreground))",
        },
        popover: {
          DEFAULT: "hsl(var(--popover))",
          foreground: "hsl(var(--popover-foreground))",
        },
        card: {
          DEFAULT: "hsl(var(--card))",
          foreground: "hsl(var(--card-foreground))",
        },
        // Semantic status hues (M151 §2.2). Each carries THREE values, and the
        // recipe is the same in both themes:
        //   tint chip  = bg-{hue}-surface text-{hue}
        //   solid fill = bg-{hue}        text-{hue}-foreground
        // Dark mode swaps the underlying values, not the classes, so no surface
        // ever needs a per-theme utility.
        success: {
          DEFAULT: "hsl(var(--success))",
          foreground: "hsl(var(--success-foreground))",
          surface: "hsl(var(--success-surface))",
        },
        warning: {
          DEFAULT: "hsl(var(--warning))",
          foreground: "hsl(var(--warning-foreground))",
          surface: "hsl(var(--warning-surface))",
        },
        // `info` is the LEGACY NAME for what is now the hold hue — violet,
        // meaning "a person must decide" (ADR 0128). It is kept only so existing
        // utilities keep compiling during the M151 migration. NEW CODE WRITES
        // `hold`. Retiring this alias is carded as m52 M151-info-rename.
        info: {
          DEFAULT: "hsl(var(--info))",
          foreground: "hsl(var(--info-foreground))",
          surface: "hsl(var(--info-surface))",
        },
        hold: {
          DEFAULT: "hsl(var(--info))",
          foreground: "hsl(var(--info-foreground))",
          surface: "hsl(var(--info-surface))",
        },
      },
      // Near-square (M151 §2.6): --radius is 3px, not 12px. The old derivation
      // (+4 / r / r-3 / r-6) goes NEGATIVE at this base, so the steps are
      // re-derived. Cards already use rounded-lg and controls rounded-md, so
      // they inherit the new geometry without touching a single class.
      borderRadius: {
        xl: "calc(var(--radius) + 3px)", // 6px — dialogs, popovers, command palette
        lg: "calc(var(--radius) + 1px)", // 4px — cards, tables, drawers
        md: "var(--radius)", //             3px — buttons, inputs, selects
        sm: "calc(var(--radius) - 1px)", // 2px — tags, kbd, meters, mini-bars
      },
      fontFamily: {
        serif: "var(--font-serif)",
        sans: "var(--font-sans)",
        mono: "var(--font-mono)",
      },
      // Role-mapped type scale (M151 §3.2) — sized by what the text IS, not by a
      // strict modular ratio. 2xs is the uppercase-mono register: eyebrows,
      // column heads, tag text. Serif headings never exceed weight 500.
      fontSize: {
        "2xs": ["var(--text-2xs)", { lineHeight: "0.875rem", letterSpacing: "var(--tracking-wide)" }],
        xs: ["var(--text-xs)", { lineHeight: "0.9375rem" }],
        sm: ["var(--text-sm)", { lineHeight: "1.1875rem" }],
        base: ["var(--text-base)", { lineHeight: "1.3125rem" }],
        md: ["var(--text-md)", { lineHeight: "1.4375rem" }],
        lg: ["var(--text-lg)", { lineHeight: "1.5rem", letterSpacing: "var(--tracking-snug)" }],
        xl: ["var(--text-xl)", { lineHeight: "1.75rem", letterSpacing: "var(--tracking-snug)" }],
        "2xl": ["var(--text-2xl)", { lineHeight: "2rem", letterSpacing: "var(--tracking-tight)" }],
        "3xl": ["var(--text-3xl)", { lineHeight: "2.25rem", letterSpacing: "var(--tracking-tight)" }],
      },
      letterSpacing: {
        tight: "var(--tracking-tight)",
        snug: "var(--tracking-snug)",
        wide: "var(--tracking-wide)",
      },
      boxShadow: {
        card: "var(--shadow-card)",
        elevated: "var(--shadow-elevated)",
        overlay: "var(--shadow-overlay)",
      },
      keyframes: {
        "accordion-down": {
          from: { height: "0" },
          to: { height: "var(--radix-accordion-content-height)" },
        },
        "accordion-up": {
          from: { height: "var(--radix-accordion-content-height)" },
          to: { height: "0" },
        },
      },
      animation: {
        "accordion-down": "accordion-down 0.2s ease-out",
        "accordion-up": "accordion-up 0.2s ease-out",
      },
    },
  },
  plugins: [animate],
};

export default config;
