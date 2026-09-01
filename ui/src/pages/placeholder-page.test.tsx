import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { PlaceholderPage } from "@/pages/placeholder-page";

// PlaceholderPage — archetype A12. The one property worth a test is that the
// two destinations it serves do NOT render alike: "this arrives in M17" and
// "there is nothing at this address" are different truths, and telling someone
// their typo is scheduled for a milestone is worse than saying nothing.

function renderAt(ui: React.ReactElement) {
  return render(<MemoryRouter>{ui}</MemoryRouter>);
}

describe("PlaceholderPage — scheduled", () => {
  it("names the milestone that ships it and reads as scheduled, not broken", () => {
    renderAt(<PlaceholderPage title="Settings" milestone="M17" />);

    expect(screen.getByRole("heading", { name: "Settings" })).toBeInTheDocument();
    expect(screen.getByText("Not yet built")).toBeInTheDocument();
    expect(screen.getByText("M17")).toBeInTheDocument();
    expect(screen.getByText(/nothing here is broken/i)).toBeInTheDocument();
    expect(screen.getByTestId("placeholder-block")).toBeInTheDocument();
  });

  it("offers exactly one way back, and it goes home", () => {
    renderAt(<PlaceholderPage title="Settings" milestone="M17" />);
    const home = screen.getByTestId("placeholder-home");
    expect(home).toHaveAttribute("href", "/");
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });
});

describe("PlaceholderPage — not found", () => {
  it("says nothing is served here, and never claims a milestone", () => {
    // This is the call App.tsx's catch-all route makes, verbatim.
    renderAt(<PlaceholderPage title="Not found" milestone="this build" />);

    expect(screen.getByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText("No such page")).toBeInTheDocument();
    expect(screen.getByText(/Nothing is served at this address/)).toBeInTheDocument();
    expect(screen.queryByText(/arrives in/i)).toBeNull();
    expect(screen.queryByText("this build")).toBeNull();
    expect(screen.getByTestId("not-found-block")).toBeInTheDocument();
  });

  it("takes the explicit prop when a call site can give one", () => {
    renderAt(<PlaceholderPage title="Some page" milestone="M17" notFound />);
    expect(screen.getByTestId("not-found-block")).toBeInTheDocument();
    expect(screen.queryByText(/arrives in/i)).toBeNull();
  });

  it("still offers the way home", () => {
    renderAt(<PlaceholderPage title="Not found" milestone="this build" />);
    expect(screen.getByTestId("placeholder-home")).toHaveAttribute("href", "/");
  });
});
