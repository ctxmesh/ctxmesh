import * as React from "react";

// theme.tsx — the theme the console never had.
//
// `tokens.css` has carried a complete `.dark` block since M12 and NOTHING has
// ever added the class: no provider, no toggle, no OS bridge (spec §10.3 finding
// 2). Half the designed palette has been dead code in every build ever shipped.
// This module is the missing half — and it lives in `lib/` rather than inside the
// shell because the class belongs to <html>, which outlives any one route: the
// chrome-less surfaces (login, the agent chatbox, a public shared run) render
// OUTSIDE AppShell and must still honour the preference.
//
// Three states, not two. "System" is a real, persisted choice — the default —
// and it keeps tracking the OS while it is selected; picking light or dark is an
// explicit override that stops tracking. A two-state toggle would silently pin
// whatever the OS happened to say the first time it ran.
//
// The theme is applied by toggling ONE class on <html> (tailwind.config.ts sets
// `darkMode: ["class"]`). Nothing else changes: every surface reads semantic
// tokens, so the values swap and the utility classes do not (there is not a
// single `dark:` variant in the codebase, and there must never be one).

export type ThemePreference = "system" | "light" | "dark";
export type ResolvedTheme = "light" | "dark";

/** Where the preference is persisted. Namespaced so it cannot collide with the
 *  session token or a page's own key in the same origin's localStorage. */
export const THEME_STORAGE_KEY = "ctxmesh.theme";

const DARK_CLASS = "dark";
const DARK_QUERY = "(prefers-color-scheme: dark)";

function isPreference(v: unknown): v is ThemePreference {
  return v === "system" || v === "light" || v === "dark";
}

/**
 * The stored preference, defaulting to "system".
 *
 * Every storage read is guarded: Safari private mode throws on localStorage
 * access, and a console that cannot render because a theme lookup threw would be
 * an absurd way to lose the product. A failed read degrades to "system".
 */
export function readThemePreference(): ThemePreference {
  try {
    const raw = window.localStorage.getItem(THEME_STORAGE_KEY);
    return isPreference(raw) ? raw : "system";
  } catch {
    return "system";
  }
}

function writeThemePreference(pref: ThemePreference): void {
  try {
    window.localStorage.setItem(THEME_STORAGE_KEY, pref);
  } catch {
    /* storage is unavailable — the choice still applies for this session */
  }
}

/** What the OS asks for. False when the browser cannot answer (jsdom has no
 *  matchMedia), which resolves "system" to light — the historical rendering. */
export function prefersDark(): boolean {
  try {
    return (
      typeof window !== "undefined" &&
      typeof window.matchMedia === "function" &&
      window.matchMedia(DARK_QUERY).matches
    );
  } catch {
    return false;
  }
}

export function resolveTheme(pref: ThemePreference): ResolvedTheme {
  if (pref === "system") return prefersDark() ? "dark" : "light";
  return pref;
}

/** Put the resolved theme on <html>. The ONE place the class is written. */
export function applyTheme(theme: ResolvedTheme): void {
  if (typeof document === "undefined") return;
  document.documentElement.classList.toggle(DARK_CLASS, theme === "dark");
}

// Apply at module load, before React renders a single node. Two reasons: the
// first paint is already correct (no white flash on a dark-mode machine), and
// the chrome-less routes get the theme without mounting the shell. This module
// is imported by app-shell, which App.tsx imports statically, so it runs on
// every entry into the SPA.
if (typeof document !== "undefined") {
  applyTheme(resolveTheme(readThemePreference()));
}

export interface ThemeContextValue {
  /** What the user chose: system (default), light, or dark. */
  preference: ThemePreference;
  /** What that resolves to right now — what is actually on screen. */
  theme: ResolvedTheme;
  setPreference: (pref: ThemePreference) => void;
}

const ThemeContext = React.createContext<ThemeContextValue | null>(null);

export function ThemeProvider({ children }: { children: React.ReactNode }) {
  const [preference, setPreferenceState] =
    React.useState<ThemePreference>(readThemePreference);
  const [systemDark, setSystemDark] = React.useState<boolean>(prefersDark);

  // Track the OS while — and only while — "system" is the choice. An explicit
  // light/dark must not be quietly overridden at sunset by an OS schedule.
  React.useEffect(() => {
    if (preference !== "system") return;
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") {
      return;
    }
    const mq = window.matchMedia(DARK_QUERY);
    const onChange = (e: MediaQueryListEvent) => setSystemDark(e.matches);
    setSystemDark(mq.matches);
    // Safari < 14 has no addEventListener on MediaQueryList.
    if (typeof mq.addEventListener === "function") {
      mq.addEventListener("change", onChange);
      return () => mq.removeEventListener("change", onChange);
    }
    mq.addListener(onChange);
    return () => mq.removeListener(onChange);
  }, [preference]);

  const theme: ResolvedTheme =
    preference === "system" ? (systemDark ? "dark" : "light") : preference;

  React.useEffect(() => {
    applyTheme(theme);
  }, [theme]);

  const setPreference = React.useCallback((pref: ThemePreference) => {
    setPreferenceState(pref);
    writeThemePreference(pref);
  }, []);

  const value = React.useMemo<ThemeContextValue>(
    () => ({ preference, theme, setPreference }),
    [preference, theme, setPreference],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

/** The theme control's hook. Outside a provider it reports the resolved theme
 *  and no-ops on set, so a component can render in isolation (a test, a
 *  wireframe) without a provider wrapper. */
export function useTheme(): ThemeContextValue {
  const ctx = React.useContext(ThemeContext);
  if (ctx) return ctx;
  const preference = readThemePreference();
  return { preference, theme: resolveTheme(preference), setPreference: () => {} };
}

/** The order the frame's control cycles through: what you have → the two
 *  explicit choices → back to following the device. */
export const THEME_CYCLE: ThemePreference[] = ["system", "light", "dark"];

export function nextThemePreference(pref: ThemePreference): ThemePreference {
  const i = THEME_CYCLE.indexOf(pref);
  return THEME_CYCLE[(i + 1) % THEME_CYCLE.length];
}

/** How each state is spoken — the button's accessible name and its tooltip.
 *  The label states what IS, and the action states what pressing it does. */
export const THEME_LABEL: Record<ThemePreference, string> = {
  system: "follows your device",
  light: "light",
  dark: "dark",
};
