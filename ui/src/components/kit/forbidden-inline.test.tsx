import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { ForbiddenInline } from "@/components/kit";
import { ApiError } from "@/lib/api";

// ForbiddenInline is the reusable 403 primitive m13.5 surfaces render when
// api.ts hands them a typed 403 (ApiError.isForbidden). It must explain-and-
// suggest, never blank — and surface the BFF's real reason.

describe("ForbiddenInline", () => {
  it("renders the explain-and-suggest 403 state with the BFF reason", () => {
    render(
      <ForbiddenInline
        title="Can't list agents in team-a"
        detail="agentdeployments.agents.ctxmesh.ai is forbidden: user cannot list"
      />,
    );
    expect(screen.getByRole("alert")).toBeInTheDocument();
    expect(screen.getByText("Can't list agents in team-a")).toBeInTheDocument();
    // The default forbidden description always names a next step (kit invariant).
    expect(
      screen.getByText(/Ask an admin to grant access|switch to a namespace/i),
    ).toBeInTheDocument();
    // The real RBAC reason is surfaced.
    expect(screen.getByText(/is forbidden: user cannot list/)).toBeInTheDocument();
  });

  it("wires an optional next action", () => {
    const onClick = vi.fn();
    render(
      <ForbiddenInline
        title="Denied"
        action={{ label: "Switch namespace", onClick }}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Switch namespace" }));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("is the surface a caller reaches for a typed 403 from api.ts", () => {
    // The pattern a surface uses: on ApiError.isForbidden → ForbiddenInline.
    const err = new ApiError("forbidden: cannot create", 403);
    const node = err.isForbidden ? (
      <ForbiddenInline title="No write access" detail={err.message} />
    ) : (
      <div>generic error</div>
    );
    render(node);
    expect(screen.getByText("No write access")).toBeInTheDocument();
    expect(screen.getByText(/forbidden: cannot create/)).toBeInTheDocument();
  });
});
