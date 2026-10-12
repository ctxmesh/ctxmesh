import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { NotFoundPage } from "@/pages/not-found-page";

function renderPage() {
  return render(
    <MemoryRouter>
      <NotFoundPage />
    </MemoryRouter>,
  );
}

describe("NotFoundPage", () => {
  it("says nothing is served here, and never promises the page is coming", () => {
    renderPage();
    expect(screen.getByRole("heading", { name: "Not found" })).toBeInTheDocument();
    expect(screen.getByText("No such page")).toBeInTheDocument();
    expect(screen.getByText(/Nothing is served at this address/)).toBeInTheDocument();
    expect(screen.queryByText(/arrives in|not yet built/i)).toBeNull();
    expect(screen.getByTestId("not-found-block")).toBeInTheDocument();
  });

  it("offers exactly one way back, and it goes home", () => {
    renderPage();
    expect(screen.getByTestId("not-found-home")).toHaveAttribute("href", "/");
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });
});
