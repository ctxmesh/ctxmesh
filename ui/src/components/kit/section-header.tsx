import * as React from "react";

import { cn } from "@/lib/utils";

// SectionHeader + ClosingNote — the in-page editorial furniture (§5.18).
//
// SectionHeader is the mock's `h2.sec` / `p.sub` pair: a serif head and an
// optional PLAIN-LANGUAGE lede that says what the section is, in words an
// operator would use. Every in-page section head goes through it, for the same
// reason PageHeader exists — 43 pages hand-tuning `<h2>` is how a type scale
// stops being one.
//
// Two rules the file enforces rather than documents:
//   • Serif never exceeds weight 500. The family HAS a 600 and it reads
//     bold-mechanical against a hairline-and-serif page, so `font-medium` is
//     hard-coded here and not a prop.
//   • The lede is `text-faint` — tertiary, but READABLE tertiary (4.8:1). It
//     carries real information, so it may never drop to `ghost`.
//
// ClosingNote is the italic serif line that ends a section — "Seven of two
// hundred. The other 193 are serving and need nothing." It is a SIGHTED
// FLOURISH: it must RESTATE what the section already showed, never be the only
// place a fact appears. It is deliberately left in the accessibility tree
// (hiding it would silently drop information the moment a caller misuses it);
// the contract is on the copy, not on an aria attribute.

export interface SectionHeaderProps {
  title: string;
  /** Plain-language sub-line: what this section is, in an operator's words. */
  lede?: React.ReactNode;
  /**
   * Right-side furniture for the head row — a "View all →" NextStepLink, a
   * filter chip. Keep it to one control; a section head is not a toolbar.
   */
  actions?: React.ReactNode;
  /** Heading level. Page sections are `h2`; sub-sections inside them `h3`. */
  as?: "h2" | "h3" | "h4";
  /** Anchor target, so a section can be deep-linked / labelled by a region. */
  id?: string;
  className?: string;
}

export function SectionHeader({
  title,
  lede,
  actions,
  as: Tag = "h2",
  id,
  className,
}: SectionHeaderProps) {
  return (
    // 12px below-margin (§5.18) — the head sits closer to its section than to
    // whatever came before it.
    <div className={cn("mb-3", className)}>
      <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
        <Tag id={id} className="min-w-0 font-serif text-xl font-medium">
          {title}
        </Tag>
        {actions && <div className="ml-auto shrink-0">{actions}</div>}
      </div>
      {lede && <p className="mt-1 max-w-[66ch] text-sm text-faint">{lede}</p>}
    </div>
  );
}

export interface ClosingNoteProps {
  children: React.ReactNode;
  className?: string;
}

export function ClosingNote({ children, className }: ClosingNoteProps) {
  return (
    <p className={cn("mt-4 font-serif text-md italic text-faint", className)}>
      {children}
    </p>
  );
}
