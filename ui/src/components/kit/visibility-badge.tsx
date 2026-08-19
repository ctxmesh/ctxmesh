import { Building2, Globe, Lock, Users } from "lucide-react";

import { Badge } from "@/components/ui/badge";

// VisibilityBadge — the ONE visibility chip across the console (H5). It unifies the two near-duplicate
// badges that had drifted: the gallery's `VisibilityBadge` rendered `org` as `secondary` while
// mcp-servers' `ServerVisibilityBadge` rendered it `outline`, and only one carried a testid. One
// place now owns the icon, variant, and casing:
//   - public / org → `secondary` (a shared/broad scope), each with its own icon;
//   - team + any unknown value → `outline` (team = Users, unknown = a Lock).
// Renders nothing for an absent visibility. Pass `name` to get a stable `visibility-<name>` testid
// (the mcp-servers list keys its row assertions on it).
export function VisibilityBadge({
  visibility,
  name,
}: {
  visibility?: string;
  name?: string;
}) {
  if (!visibility) return null;
  const icon =
    visibility === "public" ? (
      <Globe className="h-3 w-3" />
    ) : visibility === "org" ? (
      <Building2 className="h-3 w-3" />
    ) : visibility === "team" ? (
      <Users className="h-3 w-3" />
    ) : (
      <Lock className="h-3 w-3" />
    );
  const variant =
    visibility === "public" || visibility === "org" ? "secondary" : "outline";
  return (
    <Badge
      variant={variant}
      className="gap-1 text-[10px]"
      data-testid={name ? `visibility-${name}` : undefined}
    >
      {icon}
      {visibility}
    </Badge>
  );
}
