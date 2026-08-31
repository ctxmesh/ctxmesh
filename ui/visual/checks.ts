// checks.ts — the fit assertions the visual sweep runs in the page (M151).
//
// The two defects this milestone exists to close are "things do not fit" and
// "colour used without a doctrine". Neither is caught by tsc, a unit test, or a
// code review. This file catches the first one mechanically: it runs inside the
// rendered page and reports every element that is wider than the room it was
// given.
//
// The subtlety is that SOME horizontal overflow is correct. A wide table is
// supposed to scroll — inside its own container. So an element only counts as
// an offender when it overflows AND nothing above it is prepared to scroll.

export interface Offender {
  /** A short CSS-ish path, enough to find the element in the source. */
  selector: string;
  /** Why it was flagged. */
  reason: "self-overflow" | "past-viewport";
  /** How far past, in px. */
  overflowPx: number;
  /** First ~60 chars of text content, to identify it in a screenshot. */
  text: string;
}

export interface FitResult {
  /** The document scrolls sideways — always a bug, no exceptions. */
  documentScrollsX: boolean;
  documentOverflowPx: number;
  offenders: Offender[];
}

/** The function body evaluated inside the page. Kept as a string-free function
 *  so Playwright can serialise it; it must not close over anything. */
export function collectFitResult(): FitResult {
  const doc = document.documentElement;
  const SLOP = 2; // sub-pixel rounding and 1px borders are not bugs

  const describe = (el: Element): string => {
    const parts: string[] = [];
    let node: Element | null = el;
    for (let i = 0; node && i < 3; i++) {
      const tag = node.tagName.toLowerCase();
      const cls =
        typeof node.className === "string" && node.className.trim()
          ? "." + node.className.trim().split(/\s+/).slice(0, 3).join(".")
          : "";
      const id = node.id ? "#" + node.id : "";
      parts.unshift(tag + id + cls);
      node = node.parentElement;
    }
    return parts.join(" > ");
  };

  // An ancestor that is prepared to scroll (or clip) horizontally makes any
  // overflow below it intentional — that is the wide-table-in-a-scroller case.
  const hasScrollableAncestor = (el: Element): boolean => {
    let node = el.parentElement;
    while (node && node !== doc) {
      const ox = getComputedStyle(node).overflowX;
      if (ox === "auto" || ox === "scroll" || ox === "hidden") return true;
      node = node.parentElement;
    }
    return false;
  };

  const found: { el: Element; offender: Offender }[] = [];

  for (const el of Array.from(document.querySelectorAll("*"))) {
    // <html> and <body> are excluded deliberately. They contain everything, so
    // they always win the outermost filter below and would mask the element
    // that is actually too wide. Document-level overflow is already reported by
    // `documentScrollsX`; what this loop is for is finding the CAUSE.
    if (el === doc || el === document.body) continue;

    const cs = getComputedStyle(el);
    if (cs.display === "none" || cs.visibility === "hidden" || cs.opacity === "0") continue;
    // Fixed/sticky chrome is positioned against the viewport on purpose.
    if (cs.position === "fixed") continue;

    const rect = el.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) continue;

    const text = (el.textContent ?? "").trim().slice(0, 60);

    // 1. The element's own content is wider than its box, and it has not been
    //    told what to do about that.
    if (el.scrollWidth > el.clientWidth + SLOP && cs.overflowX === "visible") {
      found.push({
        el,
        offender: {
          selector: describe(el),
          reason: "self-overflow",
          overflowPx: Math.round(el.scrollWidth - el.clientWidth),
          text,
        },
      });
      continue;
    }

    // 2. The element is painted past the right edge of the viewport, and no
    //    ancestor is scrolling — so the page itself is being pushed wide.
    if (rect.right > window.innerWidth + SLOP && !hasScrollableAncestor(el)) {
      found.push({
        el,
        offender: {
          selector: describe(el),
          reason: "past-viewport",
          overflowPx: Math.round(rect.right - window.innerWidth),
          text,
        },
      });
    }
  }

  // Report the INNERMOST offenders — the elements that are actually too wide.
  //
  // The instinct is to report the outermost, but that is wrong here: overflow
  // propagates upward, so <html>, <body> and #root all overflow whenever
  // anything does, and reporting the outermost names the wrapper every single
  // time while hiding the one element with the real min-width. An offender with
  // no offending descendant is the boundary where the width originates.
  const offenders = found
    .filter(({ el }) => !found.some((o) => o.el !== el && el.contains(o.el)))
    .map(({ offender }) => offender)
    .sort((a, b) => b.overflowPx - a.overflowPx);

  return {
    documentScrollsX: doc.scrollWidth > doc.clientWidth + SLOP,
    documentOverflowPx: Math.max(0, Math.round(doc.scrollWidth - doc.clientWidth)),
    offenders: offenders.slice(0, 25),
  };
}
