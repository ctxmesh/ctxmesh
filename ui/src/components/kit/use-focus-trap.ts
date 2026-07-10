import * as React from "react";

// useFocusTrap — the shared modal a11y primitive for the kit's overlays
// (DetailDrawer, ConfirmDialog, CommandPalette). Productionized in m13.4: the
// skeletons each rolled their own Esc listener; this centralizes the three
// behaviors every accessible overlay owes the user:
//
//   1. Focus management — on open, move focus INTO the overlay (the first
//      focusable, or the container itself) and, on close, restore focus to the
//      element that had it before (so keyboard users are not dumped at <body>).
//   2. Focus trap — Tab / Shift+Tab cycle within the overlay; focus never
//      escapes to the dimmed page behind the backdrop.
//   3. Esc to close — a single, guardable Escape handler.
//
// Returns a ref to attach to the overlay container. Pass `onEscape` (usually the
// close handler) and an `active` flag (the overlay's `open`). `escapeEnabled`
// lets a caller veto Esc (Wizard's dirty-state guard supplies its own).

const FOCUSABLE =
  'a[href],button:not([disabled]),textarea:not([disabled]),input:not([disabled]),select:not([disabled]),[tabindex]:not([tabindex="-1"])';

export interface FocusTrapOptions {
  active: boolean;
  onEscape?: () => void;
  /** Set false to let the caller own Escape (e.g. a dirty-state guard). */
  escapeEnabled?: boolean;
}

export function useFocusTrap<T extends HTMLElement>({
  active,
  onEscape,
  escapeEnabled = true,
}: FocusTrapOptions): React.RefObject<T> {
  const ref = React.useRef<T>(null);
  // Latest handlers without re-installing the listener each render.
  const onEscapeRef = React.useRef(onEscape);
  onEscapeRef.current = onEscape;
  const escEnabledRef = React.useRef(escapeEnabled);
  escEnabledRef.current = escapeEnabled;

  React.useEffect(() => {
    if (!active) return;
    const container = ref.current;
    const previouslyFocused = document.activeElement as HTMLElement | null;

    // Move focus into the overlay: prefer the first focusable, else the
    // container itself (so screen-reader / keyboard focus is inside the modal).
    function focusFirst() {
      if (!container) return;
      const focusables = container.querySelectorAll<HTMLElement>(FOCUSABLE);
      if (focusables.length > 0) {
        focusables[0].focus();
      } else {
        container.focus();
      }
    }
    focusFirst();

    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        if (escEnabledRef.current) {
          e.preventDefault();
          onEscapeRef.current?.();
        }
        return;
      }
      if (e.key !== "Tab" || !container) return;
      const focusables = Array.from(
        container.querySelectorAll<HTMLElement>(FOCUSABLE),
      ).filter((el) => el.offsetParent !== null || el === document.activeElement);
      if (focusables.length === 0) {
        // Nothing focusable inside — keep focus pinned to the container.
        e.preventDefault();
        container.focus();
        return;
      }
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      const activeEl = document.activeElement as HTMLElement | null;
      if (e.shiftKey) {
        if (activeEl === first || !container.contains(activeEl)) {
          e.preventDefault();
          last.focus();
        }
      } else if (activeEl === last || !container.contains(activeEl)) {
        e.preventDefault();
        first.focus();
      }
    }

    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      // Restore focus to the opener on close (guard: it may have unmounted).
      if (previouslyFocused && document.contains(previouslyFocused)) {
        previouslyFocused.focus();
      }
    };
  }, [active]);

  return ref;
}
