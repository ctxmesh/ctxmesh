import { useEffect, useState } from "react";
import { Link } from "react-router-dom";

import { api, type UsedByRef } from "@/lib/api";

// UsedBySection renders the reverse-lookup "Used by" list for a resource (m18.9),
// consuming GET /api/usedby (m18.8). It turns the detail pages from islands into a
// navigable graph: a ModelRoute/PromptVersion shows the agents that use it; a
// SecretBinding shows the model routes that reference it — each a clickable link.
//
// It renders nothing while loading or when nothing references the object (so it
// never adds empty chrome), and swallows read errors quietly (the section is
// supplementary, never the primary surface).
export function UsedBySection({
  kind,
  name,
  namespace,
  title,
}: {
  kind: "modelroute" | "promptversion" | "secretbinding";
  name: string;
  namespace: string;
  title?: string;
}) {
  const [items, setItems] = useState<UsedByRef[] | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setItems(null);
    api
      .usedBy(kind, name, namespace, controller.signal)
      .then((res) => {
        if (!controller.signal.aborted) setItems(res.items);
      })
      .catch(() => {
        // Supplementary section — a read failure just hides it.
        if (!controller.signal.aborted) setItems([]);
      });
    return () => controller.abort();
  }, [kind, name, namespace]);

  if (!items || items.length === 0) return null;

  return (
    <div className="rounded-lg border bg-card p-4 shadow-card" data-testid="used-by-section">
      <p className="mb-2 text-sm font-medium">
        {title ?? "Used by"}{" "}
        <span className="text-xs font-normal text-muted-foreground">
          ({items.length})
        </span>
      </p>
      <div className="space-y-1">
        {items.map((r) => (
          <Link
            key={`${r.kind}/${r.namespace}/${r.name}`}
            to={usedByHref(r)}
            className="block text-sm text-primary hover:underline"
            data-testid={`used-by-${r.name}`}
          >
            {r.name}{" "}
            <span className="text-xs text-muted-foreground">({r.kind})</span>
          </Link>
        ))}
      </div>
    </div>
  );
}

// usedByHref maps a referencing resource to its detail route.
function usedByHref(r: UsedByRef): string {
  const ns = encodeURIComponent(r.namespace);
  const name = encodeURIComponent(r.name);
  if (r.kind === "AgentDeployment") return `/agents/${ns}/${name}`;
  if (r.kind === "ModelRoute") return `/routes/${ns}/${name}`;
  return "#";
}
