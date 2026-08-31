import * as React from "react";
import { Link, Route, Routes, useParams } from "react-router-dom";
import {
  ArrowLeft,
  LayoutGrid,
  Map,
  Moon,
  Palette,
  Sun,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { IAMap } from "@/design/ia-map";
import { MilestoneTag, MILESTONE_LABEL } from "@/design/scaffold";
import type { Milestone } from "@/design/scaffold";
import { SECTION_ORDER, WIREFRAMES, wireframeBySlug } from "@/design/manifest";

// DesignGallery — the flag-gated /design surface (m13.1 design gate). Its own
// chrome (deliberately NOT the product shell) frames the wireframes so the
// reviewer always knows this is the design review, not the app. Routes:
//   /design            → index (all wireframes, grouped, + IA link)
//   /design/ia         → the IA map
//   /design/w/:slug    → a single clickable wireframe

const ALL_MILESTONES: Milestone[] = ["M13", "M14", "M15", "M16", "M17", "M18"];

// A tiny theme toggle so the reviewer can redline BOTH themes at the gate. The
// gallery is design-only, so managing the `dark` class here is acceptable.
function useDesignTheme(): [boolean, () => void] {
  const [dark, setDark] = React.useState(false);
  React.useEffect(() => {
    const root = document.documentElement;
    root.classList.toggle("dark", dark);
    return () => root.classList.remove("dark");
  }, [dark]);
  return [dark, () => setDark((v) => !v)];
}

function GalleryFrame({
  children,
  back,
}: {
  children: React.ReactNode;
  back?: { to: string; label: string };
}) {
  const [dark, toggle] = useDesignTheme();
  return (
    <div className="min-h-screen bg-background text-foreground">
      <header className="sticky top-0 z-30 flex h-14 items-center justify-between border-b bg-card/80 px-6 backdrop-blur">
        <div className="flex items-center gap-3">
          <Link to="/design" className="flex items-center gap-2">
            <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-gradient-to-br from-primary to-brand-2 text-primary-foreground">
              <Palette className="h-4 w-4" />
            </div>
            <span className="text-sm font-semibold tracking-tight">
              ctxmesh · design gallery
            </span>
          </Link>
          <Badge variant="secondary" className="text-[10px]">v2 console arc · m13.1</Badge>
        </div>
        <div className="flex items-center gap-2">
          {back && (
            <Button asChild variant="ghost" size="sm">
              <Link to={back.to}><ArrowLeft className="h-4 w-4" />{back.label}</Link>
            </Button>
          )}
          <Button asChild variant="outline" size="sm">
            <Link to="/design/ia"><Map className="h-4 w-4" />IA map</Link>
          </Button>
          <Button variant="outline" size="icon" onClick={toggle} aria-label="Toggle theme">
            {dark ? <Sun className="h-4 w-4" /> : <Moon className="h-4 w-4" />}
          </Button>
        </div>
      </header>
      <main className="mx-auto max-w-7xl px-6 py-8">{children}</main>
    </div>
  );
}

function GalleryIndex() {
  const [filter, setFilter] = React.useState<Milestone | "all">("all");
  const shown = WIREFRAMES.filter((w) => filter === "all" || w.milestone === filter);

  return (
    <GalleryFrame>
      <div className="mb-8">
        <div className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-primary">
          <LayoutGrid className="h-4 w-4" /> The design gate
        </div>
        <h1 className="mt-1 text-3xl font-semibold tracking-tight">
          Console arc — IA + wireframes
        </h1>
        <p className="mt-2 max-w-2xl text-sm text-muted-foreground">
          Click through the whole arc's design before any feature build. Every
          screen is a real component on the refined token system — low-fi
          placeholder data, no live calls. Approve or redline; m13.2 builds on
          what you sign off here. Start with the{" "}
          <Link to="/design/ia" className="font-medium text-primary underline-offset-4 hover:underline">IA map</Link>.
        </p>
      </div>

      {/* Milestone filter */}
      <div className="mb-6 flex flex-wrap items-center gap-2">
        <button
          onClick={() => setFilter("all")}
          className={cn(
            "rounded-full border px-3 py-1.5 text-xs font-medium",
            filter === "all" ? "border-primary bg-accent text-accent-foreground" : "text-muted-foreground hover:bg-surface-2",
          )}
        >
          All ({WIREFRAMES.length})
        </button>
        {ALL_MILESTONES.map((m) => {
          const count = WIREFRAMES.filter((w) => w.milestone === m).length;
          return (
            <button
              key={m}
              onClick={() => setFilter(m)}
              title={MILESTONE_LABEL[m]}
              className={cn(
                "rounded-full border px-3 py-1.5 text-xs font-medium",
                filter === m ? "border-primary bg-accent text-accent-foreground" : "text-muted-foreground hover:bg-surface-2",
              )}
            >
              {m} ({count})
            </button>
          );
        })}
      </div>

      {SECTION_ORDER.map((section) => {
        const items = shown.filter((w) => w.section === section);
        if (items.length === 0) return null;
        return (
          <section key={section} className="mb-10">
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wide text-muted-foreground">
              {section}
            </h2>
            <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
              {items.map((w) => (
                <Link
                  key={w.slug}
                  to={`/design/w/${w.slug}`}
                  className="group rounded-xl border bg-card p-5 shadow-card transition-shadow hover:shadow-elevated"
                >
                  <div className="mb-2 flex items-center justify-between">
                    <MilestoneTag m={w.milestone} />
                    <span className="text-xs text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100">
                      open →
                    </span>
                  </div>
                  <p className="text-base font-semibold tracking-snug">{w.title}</p>
                  <p className="mt-1 text-sm text-muted-foreground">{w.purpose}</p>
                </Link>
              ))}
            </div>
          </section>
        );
      })}
    </GalleryFrame>
  );
}

function WireframeRoute() {
  const { slug } = useParams<{ slug: string }>();
  const wf = slug ? wireframeBySlug(slug) : undefined;

  if (!wf) {
    return (
      <GalleryFrame back={{ to: "/design", label: "Gallery" }}>
        <div className="rounded-lg border bg-card p-10 text-center">
          <p className="text-lg font-semibold">Unknown wireframe</p>
          <p className="mt-1 text-sm text-muted-foreground">
            No screen registered for “{slug}”.
          </p>
          <Button asChild className="mt-4"><Link to="/design">Back to gallery</Link></Button>
        </div>
      </GalleryFrame>
    );
  }

  const idx = WIREFRAMES.findIndex((w) => w.slug === wf.slug);
  const prev = WIREFRAMES[idx - 1];
  const next = WIREFRAMES[idx + 1];
  const Component = wf.component;

  return (
    <GalleryFrame back={{ to: "/design", label: "Gallery" }}>
      <div className="mb-6 flex flex-wrap items-center justify-between gap-3">
        <div>
          <div className="flex items-center gap-2">
            <MilestoneTag m={wf.milestone} />
            <span className="text-xs text-muted-foreground">{wf.section}</span>
          </div>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">{wf.title}</h1>
          <p className="text-sm text-muted-foreground">{wf.purpose}</p>
        </div>
        <div className="flex items-center gap-2">
          {prev && (
            <Button asChild variant="outline" size="sm">
              <Link to={`/design/w/${prev.slug}`}><ArrowLeft className="h-4 w-4" />Prev</Link>
            </Button>
          )}
          {next && (
            <Button asChild variant="outline" size="sm">
              <Link to={`/design/w/${next.slug}`}>Next</Link>
            </Button>
          )}
        </div>
      </div>

      <Component />
    </GalleryFrame>
  );
}

function IARoute() {
  return (
    <GalleryFrame back={{ to: "/design", label: "Gallery" }}>
      <IAMap />
    </GalleryFrame>
  );
}

export function DesignGallery() {
  return (
    <Routes>
      <Route index element={<GalleryIndex />} />
      <Route path="ia" element={<IARoute />} />
      <Route path="w/:slug" element={<WireframeRoute />} />
      <Route path="*" element={<GalleryIndex />} />
    </Routes>
  );
}
