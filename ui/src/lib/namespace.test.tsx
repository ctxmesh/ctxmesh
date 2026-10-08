import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

import { NamespaceProvider, useNamespace } from "@/lib/namespace";

// The scope the console STARTS in, which is a correctness property and not a preference.
//
// "" means "every namespace at once", and a caller bound per-namespace — the shape the chart
// ships — cannot read that. M177 shipped with "" as the first-visit default, so all 13 namespaced
// list endpoints returned 403 and the console told a user with full rights in their own namespace
// that they had none. Three independent UX reviewers found it the same way: by opening the page.

// A name ending in "!" is returned flagged platform-owned, as the BFF flags the control-plane
// namespace, kube-*, knative-* and *-system on a cluster-wide list.
function stubNamespaces(names: string[]) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.startsWith("/api/namespaces")) {
        return Promise.resolve({
          ok: true,
          status: 200,
          json: async () => ({
            namespaces: names.map((n) =>
              n.endsWith("!") ? { name: n.slice(0, -1), platform: true } : { name: n },
            ),
          }),
        } as Response);
      }
      return Promise.resolve({ ok: true, status: 200, json: async () => ({}) } as Response);
    }),
  );
}

function Child() {
  return <span data-testid="child">rendered</span>;
}

function Probe() {
  const { namespace, workingNamespace } = useNamespace();
  return (
    <span data-testid="scope">{`selected=${namespace || "<all>"} working=${workingNamespace || "<none>"}`}</span>
  );
}

const scope = () => screen.getByTestId("scope").textContent;

describe("NamespaceProvider — the first-visit scope", () => {
  beforeEach(() => localStorage.clear());
  afterEach(() => {
    vi.unstubAllGlobals();
    localStorage.clear();
  });

  it("seeds a concrete workspace on a first visit, never 'all namespaces'", async () => {
    stubNamespaces(["team-a", "team-b"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=team-a working=team-a"));
  });

  it("keeps an explicit 'All workspaces' choice — the seed must not overrule the user", async () => {
    // "" is PERSISTED on an explicit pick precisely so it can be told apart from never having
    // chosen. Removing the key instead made a deliberate cluster-wide selection look untouched,
    // and the seed would silently re-scope a cluster-admin on every reload.
    localStorage.setItem("ctxmesh.namespace", "");
    stubNamespaces(["team-a"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=<all> working=team-a"));
  });

  it("honours a persisted selection over the first namespace returned", async () => {
    localStorage.setItem("ctxmesh.namespace", "team-b");
    stubNamespaces(["team-a", "team-b"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=team-b working=team-b"));
  });

  // A cluster-wide caller's list is every namespace, sorted, so the control plane's own namespace
  // and kube-* often come first. Landing there showed "No agents in ctxmesh" and pointed Connect a
  // provider at the control-plane namespace.
  it("skips platform namespaces on a first visit and lands in the first workspace", async () => {
    stubNamespaces(["ctxmesh!", "default", "knative-serving!", "kube-system!", "team-a"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=default working=default"));
  });

  it("opens 'All workspaces' when every namespace is platform-owned", async () => {
    stubNamespaces(["ctxmesh!", "kube-system!"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=<all> working=ctxmesh"));
  });

  it("keeps a stored choice, even a platform namespace", async () => {
    localStorage.setItem("ctxmesh.namespace", "ctxmesh");
    stubNamespaces(["ctxmesh!", "default"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=ctxmesh working=ctxmesh"));
  });

  it("writes into a workspace, not a platform namespace, under a stored 'All workspaces'", async () => {
    localStorage.setItem("ctxmesh.namespace", "");
    stubNamespaces(["ctxmesh!", "kube-system!", "team-a"]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=<all> working=team-a"));
  });

  it("leaves the scope empty when the caller genuinely has no namespace", async () => {
    // A first-class state the shell renders as manual entry — not something to paper over with a
    // hardcoded "default" the caller may not hold.
    stubNamespaces([]);
    render(
      <NamespaceProvider>
        <Probe />
      </NamespaceProvider>,
    );
    await waitFor(() => expect(scope()).toBe("selected=<all> working=<none>"));
  });

  // The first render is HELD until the scope is decided, so that pages do not mount against ""
  // and fire cluster-wide requests they cannot satisfy. A gate that can stick is far worse than
  // the bug it fixes — a blank console — so every terminal branch must release it.
  describe("the render gate releases in every branch", () => {
    it("releases once a namespace has been seeded", async () => {
      stubNamespaces(["team-a"]);
      render(
        <NamespaceProvider>
          <Child />
        </NamespaceProvider>,
      );
      await waitFor(() => expect(screen.getByTestId("child")).toBeInTheDocument());
    });

    it("releases when the caller has NO namespaces — not a blank console", async () => {
      stubNamespaces([]);
      render(
        <NamespaceProvider>
          <Child />
        </NamespaceProvider>,
      );
      await waitFor(() => expect(screen.getByTestId("child")).toBeInTheDocument());
    });

    it("releases when the namespace list is forbidden — manual entry must still render", async () => {
      vi.stubGlobal(
        "fetch",
        vi.fn(() =>
          Promise.resolve({
            ok: false,
            status: 403,
            json: async () => ({ error: "forbidden" }),
          } as Response),
        ),
      );
      render(
        <NamespaceProvider>
          <Child />
        </NamespaceProvider>,
      );
      await waitFor(() => expect(screen.getByTestId("child")).toBeInTheDocument());
    });

    it("renders immediately when a selection is already persisted", async () => {
      localStorage.setItem("ctxmesh.namespace", "team-b");
      stubNamespaces(["team-a", "team-b"]);
      render(
        <NamespaceProvider>
          <Child />
        </NamespaceProvider>,
      );
      expect(screen.getByTestId("child")).toBeInTheDocument();
    });
  });
});
