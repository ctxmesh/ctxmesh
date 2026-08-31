import type { ReactElement } from "react";
import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { axe } from "vitest-axe";

import { PageHeader, type PageHeaderAction } from "./page-header";
import { ClosingNote, SectionHeader } from "./section-header";
import {
  NEXT_STEP_MAX_CHARS,
  NextStepLink,
  nextStepRank,
} from "./next-step-link";

// The page furniture, pinned (M151 §5.17–§5.19). These three components are
// adopted by all 43 pages, so a regression here is a regression everywhere —
// which is exactly why they ship with a test instead of on trust.
//
// The assertions are deliberately about STRUCTURE and CONTRACT, not about
// appearance: which element wraps first, what survives an action collapse, what
// is a link and what is inert. The few class assertions that DO appear are the
// ones where the class IS the behavior (`truncate`, `hidden lg:inline-flex`,
// `line-clamp-2`) or where a rule of the design system is being enforced
// (serif never above weight 500).

function renderRouted(ui: ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

// A real 63-character Kubernetes name — the longest a resource can be.
const LONG_NAME = "a".repeat(30) + "-ingest-validator-canary-" + "b".repeat(8);

describe("PageHeader — the §4.3 header band", () => {
  it("renders the title as the page's h1, serif for prose titles", () => {
    renderRouted(<PageHeader title="Agents" lede="Every agent in this workspace." />);
    const h1 = screen.getByRole("heading", { level: 1, name: "Agents" });
    expect(h1).toHaveClass("font-serif");
    // §5.18/§3: the serif family HAS a 600 and it reads bold-mechanical. 500 is
    // the ceiling for every serif heading in the console.
    expect(h1).toHaveClass("font-medium");
    expect(h1.className).not.toMatch(/font-(semibold|bold)/);
    expect(screen.getByText("Every agent in this workspace.")).toBeInTheDocument();
  });

  it("renders a resource name mono at 600, on ONE line, with the full name recoverable", () => {
    renderRouted(<PageHeader title={LONG_NAME} titleMono />);
    const h1 = screen.getByRole("heading", { level: 1 });
    expect(h1).toHaveClass("font-mono", "font-semibold");
    // §4.5: a 63-char name TRUNCATES (end-ellipsis) — it never wraps, never
    // break-alls, and never pushes the header to a third line.
    expect(h1).toHaveClass("truncate");
    expect(h1.className).not.toMatch(/break-all|whitespace-normal/);
    // ...and the whole string is still available on hover / to a screen reader.
    expect(h1).toHaveAttribute("title", LONG_NAME);
    expect(h1).toHaveTextContent(LONG_NAME);
  });

  it("caps the h1 so META wraps before the TITLE gives way (§4.3 order 2 before 3)", () => {
    // The cap is the mechanism, not decoration: flex line-breaking is greedy
    // over hypothetical sizes, so an uncapped h1 would win the line and clip
    // FIRST — inverting the specified order. Pinning the cap pins the order.
    renderRouted(<PageHeader title={LONG_NAME} titleMono meta="team-a · v7 · a4f2c1" />);
    const h1 = screen.getByRole("heading", { level: 1 });
    expect(h1).toHaveClass("max-w-[28rem]");

    // The identity group (h1 + status + meta) is ONE outer flex item, so the
    // only thing that can break to a second line at the outer level is the
    // actions group — §4.3 order 1.
    const identity = h1.parentElement!;
    expect(identity).toHaveClass("flex", "flex-wrap", "min-w-0");
    expect(within(identity).getByText("team-a · v7 · a4f2c1")).toBeInTheDocument();
  });

  it("clamps a long prose title at two lines — never a third (§4.3)", () => {
    renderRouted(<PageHeader title="Model context protocol servers and their approvals" />);
    expect(screen.getByRole("heading", { level: 1 })).toHaveClass("line-clamp-2");
  });

  it("puts the actions in their own outer flex item, pushed right by ml-auto", () => {
    renderRouted(
      <PageHeader title="Agents" actions={[{ label: "New agent", onClick: () => {} }]} />,
    );
    const button = screen.getByRole("button", { name: "New agent" });
    const actionGroup = button.parentElement!;
    // Right-aligned on whichever line it lands on — the header's own line when
    // there is room, its own line beneath when there is not.
    expect(actionGroup).toHaveClass("ml-auto", "shrink-0");
    const row = actionGroup.parentElement!;
    expect(row).toHaveClass("flex", "flex-wrap", "items-end");
    expect(row.children).toHaveLength(2); // identity group + actions. Nothing else.
  });
});

describe("PageHeader — the §4.3 action collapse", () => {
  const three: PageHeaderAction[] = [
    { label: "Duplicate", variant: "outline", onClick: () => {} },
    { label: "Export", variant: "outline", onClick: () => {} },
    { label: "Stop", variant: "destructive", onClick: () => {} },
  ];

  it("does not collapse at two or fewer actions, at any width", () => {
    renderRouted(
      <PageHeader
        title="Agents"
        actions={[
          { label: "New agent", onClick: () => {} },
          { label: "Stop", variant: "destructive", onClick: () => {} },
        ]}
      />,
    );
    expect(screen.queryByRole("button", { name: "More actions" })).toBeNull();
  });

  it("keeps the primary and the destructive below lg, folds the rest into a ⋯ menu", () => {
    renderRouted(<PageHeader title="Agents" actions={three} />);
    // No pine `default` action here, so the FIRST action stands in as primary —
    // a collapsed header always still offers something to press.
    expect(screen.getByRole("button", { name: "Duplicate" }).className).not.toMatch(
      /(^|\s)hidden(\s|$)/,
    );
    expect(screen.getByRole("button", { name: "Stop" }).className).not.toMatch(
      /(^|\s)hidden(\s|$)/,
    );
    // The folded one is display:none below lg — out of the layout AND out of
    // the accessibility tree, so the ⋯ menu is never a duplicate announcement.
    expect(screen.getByRole("button", { name: "Export" })).toHaveClass(
      "hidden",
      "lg:inline-flex",
    );
    // ...and the ⋯ is the mirror image: present only below lg.
    const more = screen.getByRole("button", { name: "More actions" });
    expect(more.parentElement).toHaveClass("lg:hidden");
    expect(more).toHaveAttribute("aria-haspopup", "menu");
    expect(more).toHaveAttribute("aria-expanded", "false");
  });

  it("prefers an explicit primary over the first action when folding", () => {
    renderRouted(
      <PageHeader
        title="Agents"
        actions={[
          { label: "Duplicate", variant: "outline", onClick: () => {} },
          { label: "Promote", variant: "outline", primary: true, onClick: () => {} },
          { label: "Export", variant: "outline", onClick: () => {} },
        ]}
      />,
    );
    expect(screen.getByRole("button", { name: "Promote" }).className).not.toMatch(
      /(^|\s)hidden(\s|$)/,
    );
    expect(screen.getByRole("button", { name: "Duplicate" })).toHaveClass("hidden");
  });

  it("the ⋯ menu opens, runs the folded action, and closes", () => {
    const onExport = vi.fn();
    renderRouted(
      <PageHeader
        title="Agents"
        actions={[
          { label: "Duplicate", variant: "outline", onClick: () => {} },
          { label: "Export", variant: "outline", onClick: onExport },
          { label: "Stop", variant: "destructive", onClick: () => {} },
        ]}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "More actions" }));
    const menu = screen.getByRole("menu", { name: "More actions" });
    const item = within(menu).getByRole("menuitem", { name: "Export" });
    fireEvent.click(item);
    expect(onExport).toHaveBeenCalledOnce();
    expect(screen.queryByRole("menu")).toBeNull();
  });

  it("Escape closes the ⋯ menu and returns focus to its trigger", () => {
    renderRouted(<PageHeader title="Agents" actions={three} />);
    const trigger = screen.getByRole("button", { name: "More actions" });
    fireEvent.click(trigger);
    expect(screen.getByRole("menu")).toBeInTheDocument();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();
    expect(trigger).toHaveFocus();
  });

  it("never collapses the actionsSlot escape hatch", () => {
    renderRouted(
      <PageHeader
        title="Agents"
        actions={three}
        actionsSlot={<span data-testid="slot">custom</span>}
      />,
    );
    expect(screen.getByTestId("slot")).toBeInTheDocument();
  });
});

describe("PageHeader — breadcrumb, tabs, loading", () => {
  it("renders the trail with the current page as text, not a link", () => {
    renderRouted(
      <PageHeader
        breadcrumb={[
          { label: "Agents", to: "/agents" },
          { label: "team-a", to: "/agents/team-a" },
          { label: "validator" },
        ]}
        title="validator"
        titleMono
      />,
    );
    const nav = screen.getByRole("navigation", { name: "Breadcrumb" });
    expect(within(nav).getByRole("link", { name: "Agents" })).toHaveAttribute(
      "href",
      "/agents",
    );
    // The leaf is where you already are — never a link to itself.
    expect(within(nav).queryByRole("link", { name: "validator" })).toBeNull();
    expect(within(nav).getByText("validator")).toHaveAttribute("aria-current", "page");
  });

  it("renders tabs as a tablist that reports and changes selection", () => {
    const onTabChange = vi.fn();
    renderRouted(
      <PageHeader
        title="validator"
        titleMono
        tabs={[
          { id: "overview", label: "Overview", current: true },
          { id: "runs", label: "Runs" },
        ]}
        onTabChange={onTabChange}
      />,
    );
    const list = screen.getByRole("tablist");
    expect(within(list).getByRole("tab", { name: "Overview" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    fireEvent.click(within(list).getByRole("tab", { name: "Runs" }));
    expect(onTabChange).toHaveBeenCalledWith("runs");
    // Roving tabindex means the arrow keys are not optional — without them the
    // unselected tabs would be unreachable by keyboard.
    fireEvent.keyDown(list, { key: "ArrowRight" });
    expect(onTabChange).toHaveBeenLastCalledWith("runs");
  });

  it("loading renders one busy region shaped like the header, and no h1", () => {
    renderRouted(<PageHeader title="Agents" lede="…" loading />);
    // Shaped skeleton (§5.17): a 32×240 title bar plus a lede line.
    const busy = screen.getByRole("status", { name: "Loading" });
    expect(busy).toHaveAttribute("aria-busy", "true");
    expect(screen.queryByRole("heading", { level: 1 })).toBeNull();
    // One announcement, not one per bar.
    expect(screen.getAllByRole("status")).toHaveLength(1);
  });

  it("has no structural a11y violations with the full furniture mounted", async () => {
    const { container } = renderRouted(
      <PageHeader
        breadcrumb={[{ label: "Agents", to: "/agents" }, { label: "validator" }]}
        title="validator"
        titleMono
        status={<span>Ready</span>}
        meta="team-a · v7 · a4f2c1"
        lede="What this agent does."
        actions={[
          { label: "Promote", onClick: () => {} },
          { label: "Export", variant: "outline", onClick: () => {} },
          { label: "Stop", variant: "destructive", onClick: () => {} },
        ]}
        tabs={[{ id: "overview", label: "Overview", current: true }]}
      />,
    );
    expect(
      await axe(container, { rules: { "color-contrast": { enabled: false } } }),
    ).toHaveNoViolations();
  });
});

describe("SectionHeader + ClosingNote (§5.18)", () => {
  it("renders a serif h2 at weight 500 with a plain-language lede", () => {
    render(
      <SectionHeader
        title="Needs a person"
        lede="Seven runs are paused until someone decides."
      />,
    );
    const h2 = screen.getByRole("heading", { level: 2, name: "Needs a person" });
    expect(h2).toHaveClass("font-serif", "font-medium");
    expect(h2.className).not.toMatch(/font-(semibold|bold)/);
    const lede = screen.getByText("Seven runs are paused until someone decides.");
    // Tertiary but READABLE (4.8:1). The lede carries information, so it may
    // never drop to `ghost`.
    expect(lede).toHaveClass("text-faint");
    expect(lede.className).not.toMatch(/text-ghost/);
  });

  it("can render at a lower level for a sub-section", () => {
    render(<SectionHeader as="h3" title="Recent runs" />);
    expect(screen.getByRole("heading", { level: 3, name: "Recent runs" })).toBeInTheDocument();
  });

  it("ClosingNote is an italic serif line that stays in the accessibility tree", () => {
    render(<ClosingNote>The other 193 are serving and need nothing.</ClosingNote>);
    const note = screen.getByText("The other 193 are serving and need nothing.");
    expect(note).toHaveClass("font-serif", "italic", "text-faint");
    // It is a sighted flourish that RESTATES — but hiding it would silently
    // drop information the moment a caller misuses it, so it is never aria-hidden.
    expect(note).not.toHaveAttribute("aria-hidden");
  });
});

describe("NextStepLink (§5.19) — the console's signature element", () => {
  it("appends the arrow itself and keeps it out of the accessible name", () => {
    renderRouted(<NextStepLink label="Review the stop" to="/runs/abc" />);
    // The verb phrase is the name; the arrow is decoration.
    const link = screen.getByRole("link", { name: "Review the stop" });
    expect(link).toHaveAttribute("href", "/runs/abc");
    expect(link).toHaveTextContent("Review the stop →");
  });

  it("wears the pine resting underline that firms on hover (§2.3)", () => {
    renderRouted(<NextStepLink label="Raise the cap" to="/quotas" />);
    const link = screen.getByRole("link", { name: "Raise the cap" });
    expect(link).toHaveClass("text-primary", "border-b", "border-accent", "hover:border-primary");
    // Never truncated and never wrapped — it is the point of the row (§4.4).
    expect(link).toHaveClass("whitespace-nowrap");
    expect(link.className).not.toMatch(/truncate|line-clamp/);
  });

  it("crit tone is reserved for a failure or a stop", () => {
    renderRouted(<NextStepLink label="Open failing run" to="/runs/x" tone="crit" />);
    const link = screen.getByRole("link", { name: "Open failing run" });
    expect(link).toHaveClass("text-destructive", "border-destructive-surface");
    expect(link.className).not.toMatch(/text-primary/);
  });

  it("renders a button when the next step is an action rather than a destination", () => {
    const onClick = vi.fn();
    renderRouted(<NextStepLink label="Clear the queue" onClick={onClick} />);
    fireEvent.click(screen.getByRole("button", { name: "Clear the queue" }));
    expect(onClick).toHaveBeenCalledOnce();
  });

  it('tone="none" is the words "Nothing needed" — readable, and honestly inert', () => {
    const { container } = renderRouted(
      <NextStepLink tone="none" label="ignored" to="/should-not-be-used" />,
    );
    // The literal copy, never the caller's label, and never a guessed dash.
    expect(screen.getByText("Nothing needed")).toBeInTheDocument();
    expect(screen.queryByText("ignored")).toBeNull();
    // Not a link, not a button, not focusable — an inert state must not look
    // or behave clickable.
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(container.querySelector("a, button, [tabindex]")).toBeNull();
    const span = screen.getByText("Nothing needed");
    // No underline of any kind: the resting rule is the promise of a destination.
    expect(span.className).not.toMatch(/border-b|underline/);
    // Readable (4.8:1), not the contrast-exempt ghost the mock used.
    expect(span).toHaveClass("text-faint", "font-normal");
    expect(span.className).not.toMatch(/text-ghost/);
  });

  it("degrades a destination-less next step to honest text, not a dead link", () => {
    renderRouted(<NextStepLink label="Finish setup" />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
    expect(screen.getByText("Finish setup")).toHaveClass("text-faint");
  });

  it("sorts rows that need something above rows that do not (§5.19 sort contract)", () => {
    expect(nextStepRank("default")).toBeLessThan(nextStepRank("none"));
    expect(nextStepRank("crit")).toBeLessThan(nextStepRank("none"));
    expect(nextStepRank(undefined)).toBe(nextStepRank("default"));
  });

  it("the §7.2 vocabulary fits the 22-character column budget", () => {
    // The Next step column is never dropped and never truncated (§4.4), so the
    // COPY is what gets budgeted. If a phrase here stops fitting, the column
    // has stopped being budgetable.
    for (const phrase of [
      "Review the stop",
      "Open failing run",
      "Raise the cap",
      "Finish setup",
      "Promote to 100%",
      "Approve $42.00",
      "Review 4 holds",
      "Fix the roster",
      "Test it",
      "Clear the queue",
      "Nothing needed",
    ]) {
      expect(phrase.length, phrase).toBeLessThanOrEqual(NEXT_STEP_MAX_CHARS);
    }
  });
});
