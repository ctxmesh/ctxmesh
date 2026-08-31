import { describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

import { ForbiddenInline } from "@/components/kit";
import { ApiError } from "@/lib/api";

// ForbiddenInline is the reusable 403 primitive m13.5 surfaces render when
// api.ts hands them a typed 403 (ApiError.isForbidden). It must explain-and-
// suggest, never blank. M99 C1 made the boundary CALM (a lock, neutral tone —
// not an alarm); M100 UI99-403 completed it: the raw BFF RBAC string is NEVER
// surfaced on a 403 — a friendly, resource-named message carries it instead.

describe("ForbiddenInline", () => {
  it("renders the explain-and-suggest 403 state and NEVER the raw RBAC string", () => {
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
    // The raw BFF RBAC reason is NOT surfaced (M100 UI99-403 — it is noise + the audit's leak).
    expect(screen.queryByText(/is forbidden: user cannot list/)).toBeNull();
  });

  it("uses `resource` for the friendly, resource-named message", () => {
    render(<ForbiddenInline resource="agents" />);
    expect(
      screen.getByText("You don't have permission to view agents"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Ask an admin for a role that can read agents/),
    ).toBeInTheDocument();
  });

  // M151 §7 (A4): a denied WRITE must not read as a denied read — the user
  // would go ask an admin for the wrong role.
  it("names the missing permission on a write denial", () => {
    render(<ForbiddenInline resource="teams" permission="create" />);
    expect(
      screen.getByText("You don't have permission to create teams"),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Ask an admin for a role that can create teams."),
    ).toBeInTheDocument();
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
    // The raw message is passed but suppressed — never rendered on a 403.
    expect(screen.queryByText(/forbidden: cannot create/)).toBeNull();
  });
});
