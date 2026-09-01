import { NextStepLink } from "@/components/kit";
import { cn } from "@/lib/utils";

// PlaceholderPage — archetype A12, the shell state (M151 §6.1 A12).
//
// It serves two destinations that must NOT read alike, because they are two
// different truths:
//
//   • SCHEDULED (`/soon/:id`) — the approved IA lists this surface and a later
//     milestone ships it. Nothing is broken; the nav carries it now so the shape
//     of the console is honest about what is coming. A dashed fence says "a
//     surface belongs here", the same grammar `EmptyState`'s teaching intent
//     uses, and the copy NAMES the milestone (`arrives in M17`) rather than
//     saying "coming soon", which is how a placeholder becomes permanent.
//   • MISSING (the `*` route) — nothing is served at this address at all. A
//     solid, quiet frame, and copy that says so plainly. Telling someone their
//     typo "arrives in this build" is worse than saying nothing.
//
// Composition per A12: serif title, one mono eyebrow, one lede line, ONE pine
// link home. No illustration — the old Construction icon is dropped with it.
//
// This does not compose `kit/EmptyState` even though the frame is its cousin:
// A12 specifies a page-scale serif title and a pine "Next step" link, and
// EmptyState's action slot renders a Button inside a `<p>`-bodied description.
// Bending it here would produce invalid markup and a control in the wrong
// register; the shared thing between them is the frame recipe, which is two
// utility classes.

/** A real milestone handle (`M17`, `m151.4`) as opposed to a prose stand-in. */
const MILESTONE_ID = /^m\d/i;

export interface PlaceholderPageProps {
  /** The destination's name — the nav label, or "Not found". */
  title: string;
  /** The milestone that ships it, e.g. "M17". Ignored by the missing variant. */
  milestone: string;
  /**
   * Render the MISSING variant rather than the scheduled one.
   *
   * `App.tsx` mounts the catch-all route as `<PlaceholderPage title="Not found"
   * milestone="this build" />` and is outside this page's remit to change, so
   * the title is the fallback signal. The prop exists so that call site can be
   * made explicit the moment it is touched; a nav item can never be labelled
   * "Not found", so the fallback cannot misfire on the scheduled variant.
   */
  notFound?: boolean;
}

export function PlaceholderPage({ title, milestone, notFound }: PlaceholderPageProps) {
  const missing = notFound ?? title.trim().toLowerCase() === "not found";

  return (
    <div className="mx-auto min-w-0 max-w-3xl py-6">
      <div
        role="region"
        aria-label={title}
        data-testid={missing ? "not-found-block" : "placeholder-block"}
        className={cn(
          "flex flex-col items-center rounded-lg px-6 py-16 text-center",
          // A dashed fence says something belongs here; a page that does not
          // exist is not a placeholder for anything, so it gets the calm solid
          // frame instead (the §7 teaching / unavailable distinction).
          missing
            ? "border border-border bg-surface-2/40"
            : "border border-dashed border-border-strong bg-card",
        )}
      >
        <p className="font-mono text-2xs uppercase tracking-wide text-faint">
          {missing ? "No such page" : "Not yet built"}
        </p>
        <h1 className="mt-2 max-w-full break-words font-serif text-3xl font-medium tracking-tight">
          {title}
        </h1>
        <p className="mt-3 max-w-[52ch] text-md text-muted-foreground">
          {missing ? (
            <>
              Nothing is served at this address. The link may be stale, or the
              page may have been renamed.
            </>
          ) : (
            <>
              This surface arrives in{" "}
              {/* A milestone ID is machine-owned and set in mono; App.tsx's
                  fallback for a nav entry with no milestone ("a later
                  milestone") is prose, and mono would dress a shrug up as a
                  commitment. */}
              {MILESTONE_ID.test(milestone) ? (
                <span className="font-mono text-secondary-foreground">{milestone}</span>
              ) : (
                milestone
              )}
              . The navigation carries it already so the console&rsquo;s shape is
              honest about what is coming — nothing here is broken.
            </>
          )}
        </p>
        <p className="mt-6">
          <NextStepLink label="Back to Home" to="/" testId="placeholder-home" />
        </p>
      </div>
    </div>
  );
}
