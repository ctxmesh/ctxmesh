import { NextStepLink } from "@/components/kit";

// NotFoundPage — what the console's catch-all route renders: nothing is served
// at this address. A solid, quiet frame (archetype A12): serif title, one mono
// eyebrow, one lede line, ONE pine link home.
//
// It does not compose `kit/EmptyState`: A12 specifies a page-scale serif title
// and a pine "Next step" link, and EmptyState's action slot renders a Button
// inside a `<p>`-bodied description.
export function NotFoundPage() {
  return (
    <div className="mx-auto min-w-0 max-w-3xl py-6">
      <div
        role="region"
        aria-label="Not found"
        data-testid="not-found-block"
        className="flex flex-col items-center rounded-lg border border-border bg-surface-2/40 px-6 py-16 text-center"
      >
        <p className="font-mono text-2xs uppercase tracking-wide text-faint">No such page</p>
        <h1 className="mt-2 max-w-full break-words font-serif text-3xl font-medium tracking-tight">
          Not found
        </h1>
        <p className="mt-3 max-w-[52ch] text-md text-muted-foreground">
          Nothing is served at this address. The link may be stale, or the page may
          have been renamed.
        </p>
        <p className="mt-6">
          <NextStepLink label="Back to Home" to="/" testId="not-found-home" />
        </p>
      </div>
    </div>
  );
}
