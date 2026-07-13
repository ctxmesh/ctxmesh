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
  | "secretbinding";

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
    // Honest non-link: no detail page (or missing coordinates) → plain text.
    return (
      <span className={className} data-testid={testId}>
        {name}
      </span>
    );
  }
  return (
    <Link
      to={path}
      className={cn(
        "text-primary underline-offset-2 hover:underline",
        className,
      )}
      data-testid={testId}
    >
      {name}
    </Link>
  );
}
