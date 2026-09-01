import { Link } from "react-router-dom";

import { cn } from "@/lib/utils";

// ResourceLink — the navigability seam (M22 / Theme 1). The rule: anywhere the
// console renders the name of a resource that HAS a detail page, it is a link to
// that detail, never dead-end text ("if it names a resource, it's a link"). One
// place resolves (kind, namespace, name) → the detail route, so routing/appearance
// change in a single spot and the rule stays lint-able (grep for resource names
// rendered as raw <span>/<td> text in detail/list surfaces).

export type ResourceKind =
  | "agent"
  | "registry"
  | "route"
  | "secretbinding"
  // The team detail page (M151 A3) — a roster's members and its supervisor are
  // agents, but the team itself is now a destination too, so a team name
  // rendered anywhere obeys the same "if it names a resource, it's a link" rule.
  | "team";

// resourcePath maps a resource to its detail route, or null when there is no
// detail page for that kind (then ResourceLink renders honest text, not a dead link).
export function resourcePath(
  kind: ResourceKind,
  namespace: string,
  name: string,
): string | null {
  const ns = encodeURIComponent(namespace);
  const nm = encodeURIComponent(name);
  switch (kind) {
    case "agent":
      return `/agents/${ns}/${nm}`;
    case "registry":
      return `/registries/${ns}/${nm}`;
    case "route":
      return `/routes/${ns}/${nm}`;
    case "secretbinding":
      return `/secrets/${ns}/${nm}`;
    case "team":
      return `/teams/${ns}/${nm}`;
    default:
      return null;
  }
}

export function ResourceLink({
  kind,
  namespace,
  name,
  className,
  testId,
}: {
  kind: ResourceKind;
  namespace: string;
  name: string;
  className?: string;
  testId?: string;
}) {
  const path = namespace && name ? resourcePath(kind, namespace, name) : null;
  if (!path) {
    // Honest non-link: no detail page (or missing coordinates) → plain mono ink,
    // explicitly NOT underlined — the resting underline is the promise of a destination.
    return (
      <span className={cn("font-mono no-underline", className)} data-testid={testId}>
        {name}
      </span>
    );
  }
  return (
    <Link
      to={path}
      className={cn(
        // The ONE link treatment console-wide (§2.3/§5.7): pine text over a resting
        // pine-surface rule that firms to pine on hover — not a decoration-underline.
        "font-mono text-primary border-b border-accent hover:border-primary",
        className,
      )}
      data-testid={testId}
    >
      {name}
    </Link>
  );
}
