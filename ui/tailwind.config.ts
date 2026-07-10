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
        },
        muted: {
          DEFAULT: "hsl(var(--muted))",
          foreground: "hsl(var(--muted-foreground))",
        },
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
        // Semantic status tokens for topology/health (dashboard m12.5).
        success: {
          DEFAULT: "hsl(var(--success))",
          foreground: "hsl(var(--success-foreground))",
        },
        warning: {
          DEFAULT: "hsl(var(--warning))",
          foreground: "hsl(var(--warning-foreground))",
        },
        info: {
          DEFAULT: "hsl(var(--info))",
          foreground: "hsl(var(--info-foreground))",
        },
      },
      borderRadius: {
        xl: "calc(var(--radius) + 4px)",
        lg: "var(--radius)",
        md: "calc(var(--radius) - 3px)",
        sm: "calc(var(--radius) - 6px)",
      },
      fontFamily: {
        sans: "var(--font-sans)",
        mono: "var(--font-mono)",
      },
      // Modular type scale — surfaces size text by role via these tokens.
      fontSize: {
        xs: ["var(--text-xs)", { lineHeight: "1rem" }],
        sm: ["var(--text-sm)", { lineHeight: "1.1875rem" }],
        base: ["var(--text-base)", { lineHeight: "1.375rem" }],
        md: ["var(--text-md)", { lineHeight: "1.4375rem" }],
        lg: ["var(--text-lg)", { lineHeight: "1.5rem" }],
        xl: ["var(--text-xl)", { lineHeight: "1.75rem", letterSpacing: "var(--tracking-snug)" }],
        "2xl": ["var(--text-2xl)", { lineHeight: "2rem", letterSpacing: "var(--tracking-tight)" }],
        "3xl": ["var(--text-3xl)", { lineHeight: "2.5rem", letterSpacing: "var(--tracking-tight)" }],
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
