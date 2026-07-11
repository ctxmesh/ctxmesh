import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { UsedBySection } from "@/components/used-by-section";

function stubUsedBy(items: unknown[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/usedby"))
        return Promise.resolve({ ok: true, status: 200, json: async () => ({ items }) } as Response);
      return Promise.resolve({ ok: false, status: 404, json: async () => ({}) } as Response);
    }),
  );
}

afterEach(() => vi.restoreAllMocks());

describe("UsedBySection", () => {
  it("renders links to the referencing resources (route detail for a ModelRoute)", async () => {
    stubUsedBy([{ kind: "AgentDeployment", name: "billing-agent", namespace: "default" }]);
    render(
      <MemoryRouter>
        <UsedBySection kind="modelroute" name="anthropic" namespace="default" title="Used by agents" />
      </MemoryRouter>,
    );
    const link = await screen.findByTestId("used-by-billing-agent");
    expect(link).toHaveAttribute("href", "/agents/default/billing-agent");
    expect(screen.getByText("Used by", { exact: false })).toBeInTheDocument();
  });

  it("renders nothing when no resource references the object", async () => {
    stubUsedBy([]);
    render(
      <MemoryRouter>
        <UsedBySection kind="secretbinding" name="x" namespace="default" />
      </MemoryRouter>,
    );
    await waitFor(() =>
      expect(screen.queryByTestId("used-by-section")).not.toBeInTheDocument(),
    );
  });
});
