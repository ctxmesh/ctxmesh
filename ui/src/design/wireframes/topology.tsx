import * as React from "react";
import {
  Boxes,
  ChevronDown,
  List,
  Maximize2,
  Minus,
  Network,
  Plus,
  Search,
  ZoomIn,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DetailDrawer } from "@/components/kit";
import { ConsoleChrome } from "@/design/console-chrome";
import { KeyValue, Note, ScreenFrame } from "@/design/scaffold";

// A grouped "cluster" node — the collapsed-at-scale state. At 200 agents the
// graph groups by registry+namespace and renders each group as one node with a
// count, expandable on click. This is the KEY scale wireframe.
function GroupNode({
  name,
  ns,
  count,
  health,
  onClick,
}: {
  name: string;
  ns: string;
  count: number;
  health: { ready: number; warn: number; fail: number };
  onClick?: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className="w-52 rounded-lg border border-border-strong bg-card p-3 text-left shadow-card transition-shadow hover:shadow-elevated"
    >
      <div className="mb-2 flex items-center gap-2">
        <div className="flex h-7 w-7 items-center justify-center rounded-md bg-accent text-accent-foreground">
          <Boxes className="h-4 w-4" />
        </div>
        <div className="min-w-0">
          <p className="truncate text-sm font-medium">{name}</p>
          <p className="truncate text-[11px] text-muted-foreground">{ns}</p>
        </div>
        <ChevronDown className="ml-auto h-4 w-4 text-muted-foreground" />
      </div>
      <div className="flex items-center justify-between">
        <span className="text-xs text-muted-foreground">{count} agents</span>
        <div className="flex items-center gap-1.5 text-[10px]">
          <span className="flex items-center gap-0.5"><span className="h-2 w-2 rounded-full bg-success" />{health.ready}</span>
          {health.warn > 0 && <span className="flex items-center gap-0.5"><span className="h-2 w-2 rounded-full bg-warning" />{health.warn}</span>}
          {health.fail > 0 && <span className="flex items-center gap-0.5"><span className="h-2 w-2 rounded-full bg-destructive" />{health.fail}</span>}
        </div>
      </div>
    </button>
  );
}

const GROUPS = [
  { name: "billing-team", ns: "prod", count: 64, health: { ready: 60, warn: 3, fail: 1 } },
  { name: "support-team", ns: "prod", count: 48, health: { ready: 47, warn: 1, fail: 0 } },
  { name: "docs-team", ns: "prod", count: 22, health: { ready: 22, warn: 0, fail: 0 } },
  { name: "platform", ns: "staging", count: 39, health: { ready: 35, warn: 2, fail: 2 } },
  { name: "experiments", ns: "dev", count: 27, health: { ready: 24, warn: 3, fail: 0 } },
];

export function TopologyScaleWireframe() {
  const [view, setView] = React.useState<"graph" | "list">("graph");
  const [drawerOpen, setDrawerOpen] = React.useState(false);

  return (
    <div className="space-y-4">
      <Note>
        Topology v2 at 200 agents (§31 scale UX): grouped/collapsed by
        registry+namespace, a search box, zoom controls, and a list↔graph toggle.
        Click a group to expand; click a node to open the detail drawer without
        leaving the map. THIS is the state that proves the console works at scale.
      </Note>
      <div className="flex justify-end gap-2">
        <Button size="sm" variant={view === "graph" ? "default" : "outline"} onClick={() => setView("graph")}><Network className="h-4 w-4" />Graph</Button>
        <Button size="sm" variant={view === "list" ? "default" : "outline"} onClick={() => setView("list")}><List className="h-4 w-4" />List</Button>
        <Button size="sm" variant="outline" onClick={() => setDrawerOpen(true)}><ZoomIn className="h-4 w-4" />Node drawer</Button>
      </div>

      <ScreenFrame caption="200 agents — grouped & collapsed">
        <div className="relative">
          <ConsoleChrome
            active="topology"
            title="Topology"
            headerActions={<Badge variant="secondary" className="text-[10px]">200 agents · 5 registries</Badge>}
          >
            {/* Controls row: search + view toggle + zoom */}
            <div className="mb-4 flex flex-wrap items-center gap-3">
              <div className="relative min-w-[18rem] flex-1">
                <Search className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                <Input placeholder="Search agents, registries, namespaces…" className="pl-9" />
              </div>
              <div className="flex items-center rounded-md border bg-card">
                <button className={`flex items-center gap-1.5 px-3 py-1.5 text-sm ${view === "graph" ? "bg-accent text-accent-foreground" : "text-muted-foreground"}`} onClick={() => setView("graph")}><Network className="h-4 w-4" />Graph</button>
                <button className={`flex items-center gap-1.5 px-3 py-1.5 text-sm ${view === "list" ? "bg-accent text-accent-foreground" : "text-muted-foreground"}`} onClick={() => setView("list")}><List className="h-4 w-4" />List</button>
              </div>
            </div>

            {view === "graph" ? (
              <div className="relative h-[26rem] overflow-hidden rounded-lg border bg-[radial-gradient(hsl(var(--border))_1px,transparent_1px)] [background-size:22px_22px]">
                {/* grouped nodes laid out around a hub */}
                <div className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2">
                  <div className="flex flex-wrap items-center justify-center gap-8" style={{ width: "40rem" }}>
                    {GROUPS.map((g) => (
                      <GroupNode key={g.name} {...g} onClick={() => setDrawerOpen(true)} />
                    ))}
                  </div>
                </div>
                {/* zoom controls */}
                <div className="absolute bottom-3 right-3 flex flex-col overflow-hidden rounded-md border bg-card shadow-card">
                  <button className="p-2 hover:bg-surface-2"><Plus className="h-4 w-4" /></button>
                  <button className="border-y p-2 hover:bg-surface-2"><Minus className="h-4 w-4" /></button>
                  <button className="p-2 hover:bg-surface-2"><Maximize2 className="h-4 w-4" /></button>
                </div>
                <div className="absolute left-3 top-3 rounded-md border bg-card/80 px-2.5 py-1 text-xs text-muted-foreground backdrop-blur">
                  Grouped by registry · namespace — click to expand
                </div>
              </div>
            ) : (
              <div className="space-y-2">
                {GROUPS.map((g) => (
                  <button key={g.name} onClick={() => setDrawerOpen(true)} className="flex w-full items-center gap-3 rounded-md border bg-card px-4 py-3 text-left shadow-card hover:bg-surface-2">
                    <Boxes className="h-4 w-4 text-primary" />
                    <div className="flex-1">
                      <p className="text-sm font-medium">{g.name}</p>
                      <p className="text-xs text-muted-foreground">{g.ns} · {g.count} agents</p>
                    </div>
                    <div className="flex items-center gap-2 text-xs">
                      <Badge variant="success" className="text-[10px]">{g.health.ready} ready</Badge>
                      {g.health.fail > 0 && <Badge variant="destructive" className="text-[10px]">{g.health.fail} failed</Badge>}
                    </div>
                    <ChevronDown className="h-4 w-4 text-muted-foreground" />
                  </button>
                ))}
              </div>
            )}
          </ConsoleChrome>

          <div className="absolute inset-0">
            <DetailDrawer
              open={drawerOpen}
              onClose={() => setDrawerOpen(false)}
              title="billing-invoice-agent"
              subtitle="billing-team · prod"
              status={<Badge variant="success" className="text-[10px]">Ready</Badge>}
              footer={
                <>
                  <Button variant="outline" size="sm">Open detail</Button>
                  <Button size="sm">Run</Button>
                </>
              }
            >
              <div className="space-y-4">
                <Note>Clicked-node state: detail without losing the map behind the drawer.</Note>
                <KeyValue
                  rows={[
                    { k: "Runtime", v: "managed" },
                    { k: "Model", v: <span className="font-mono text-xs">claude-sonnet-4</span> },
                    { k: "Runs 24h", v: "412" },
                    { k: "Neighbors", v: "acme-mcp, get_invoice, refund" },
                  ]}
                />
              </div>
            </DetailDrawer>
          </div>
        </div>
      </ScreenFrame>
    </div>
  );
}
