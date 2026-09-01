import * as React from "react";

import { cn } from "@/lib/utils";
import { UNKNOWN_VALUE_TITLE } from "@/components/kit/quiet-note";

// KeyValueList — the detail rail's fact register (M151 §5.25).
//
// Every A2 detail page has a 300px rail whose job is "here is what governs this
// thing": its namespace, its route, its guardrails, who owns it, when it last
// ran. This is that register — a definition list, not a table, because these
// are facts about ONE object.
//
// The rule the whole component exists to enforce: A ROW IS NEVER BLANK. A value
// the backend did not send is not an empty cell; it is a stated absence, in
// words, with a `title` saying why (§7.1). An empty cell is indistinguishable
// from a rendering bug, and it is how a console quietly stops being trustworthy.
//
// The zero/unknown split is the other half of that rule and is tested: a value
// of `0` is a real measurement and renders `0`. Only undefined / null / "" (and
// the booleans, which React renders as nothing at all) are absent. A truthiness
// check here would print "not yet known" over a genuine zero — the exact lie
// this design system is built to refuse.
//
// Composition, not configuration: a value that names a resource is a
// `<ResourceLink>`, a value that carries a state is a `<Badge>`, a value with a
// unit is mono text. The caller passes the node; this component owns the row
// geometry, the key register, and the absent case.

export interface KeyValueItem {
  /**
   * The fact's name. Rendered in the uppercase mono key register, so a rail and
   * a form grid read as the same object.
   */
  key: string;
  /**
   * The fact. `undefined` / `null` / `""` (and booleans) mean the backend did
   * not answer — the row renders `absent` instead, never a blank.
   * A real `0` is a real value and renders as `0`.
   */
  value?: React.ReactNode;
  /**
   * The §7.1 words for the absent case: "not attached", "not yet known",
   * "never called". Defaults to "not yet known". Pass an em dash only for
   * numeric registers where a word would not fit.
   */
  absent?: string;
  /**
   * Tooltip. On a present value it is the full/untruncated form; on an absent
   * one it explains the absence, defaulting to the unknown-not-zero sentence.
   */
  title?: string;
  /** Render the value in mono (default). Turn off for prose values. */
  mono?: boolean;
  className?: string;
}

export interface KeyValueListProps {
  items: KeyValueItem[];
  className?: string;
}

/** The default words for a value the backend did not send (§7.1). */
export const KV_ABSENT_DEFAULT = "not yet known";

/**
 * True when React would render nothing at all for this node — the set that
 * would otherwise leave a silently empty row. `0` and `"0"` are NOT in it.
 */
function isAbsentValue(value: React.ReactNode): boolean {
  return (
    value === undefined ||
    value === null ||
    value === "" ||
    typeof value === "boolean"
  );
}

export function KeyValueList({ items, className }: KeyValueListProps) {
  return (
    <dl className={cn("w-full", className)}>
      {items.map((item) => {
        const absent = isAbsentValue(item.value);
        return (
          <div
            key={item.key}
            className={cn(
              "flex items-baseline justify-between gap-3 border-b border-border-soft py-2 last:border-0",
              item.className,
            )}
          >
            {/* The key register (§5.25): uppercase mono at the eyebrow size,
                faint — tertiary meta that must still be READ. */}
            <dt className="shrink-0 font-mono text-2xs uppercase tracking-wide text-faint">
              {item.key}
            </dt>
            <dd
              className={cn(
                "min-w-0 break-words text-right text-sm",
                item.mono === false ? undefined : "font-mono",
                // An absence recedes to the placeholder register; a fact does
                // not. Same row, visibly different weight of claim.
                absent && "text-ghost",
              )}
              title={
                absent
                  ? (item.title ?? UNKNOWN_VALUE_TITLE)
                  : item.title
              }
            >
              {absent ? (item.absent ?? KV_ABSENT_DEFAULT) : item.value}
            </dd>
          </div>
        );
      })}
    </dl>
  );
}
