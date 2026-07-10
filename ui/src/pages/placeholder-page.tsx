import { Construction } from "lucide-react";

import { Card, CardContent } from "@/components/ui/card";

// PlaceholderPage — a token-styled "surface arrives in a later task" panel for
// nav entries whose surface is not in the m12.4 foundation scope (config-builder
// m12.6, Playground m12.7). Keeps the shell/routing complete without pulling
// those surfaces forward.
export function PlaceholderPage({
  title,
  milestone,
}: {
  title: string;
  milestone: string;
}) {
  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <div>
        <h2 className="text-2xl font-semibold tracking-tight">{title}</h2>
      </div>
      <Card>
        <CardContent className="flex flex-col items-center gap-3 py-16 text-center">
          <Construction className="h-8 w-8 text-muted-foreground" />
          <p className="text-sm font-medium">Arrives in {milestone}</p>
          <p className="max-w-sm text-sm text-muted-foreground">
            The foundation (design tokens, app shell, BFF seams) is in place;
            this surface is built on top of it.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
