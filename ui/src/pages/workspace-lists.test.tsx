import * as React from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { ToastProvider } from "@/components/kit";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { CreateTeamPage } from "@/pages/create-team-page";
import { GuardrailPoliciesPage } from "@/pages/guardrail-policies-page";
import { KBDetailPage, KnowledgeBasesPage } from "@/pages/knowledge-bases-page";
import { ApprovalsPage } from "@/pages/approvals-page";
import { McpServersPage } from "@/pages/mcp-servers-page";
import { AgentRegistriesPage } from "@/pages/agent-registries-page";
import { AgentRegistryDetailPage } from "@/pages/agent-registry-detail-page";
import { AgentDetailPage } from "@/pages/agent-detail-page";
import { AgentsPage } from "@/pages/agents-page";
import { EvalsPage } from "@/pages/evals-page";
import { ModelRouteDetailPage } from "@/pages/model-route-detail-page";
import { ModelRoutesPage } from "@/pages/model-routes-page";
import { PromptsPage } from "@/pages/prompts-page";
import { SecretBindingDetailPage } from "@/pages/secret-binding-detail-page";
import { SecretBindingsPage } from "@/pages/secret-bindings-page";
import { TeamDetailPage } from "@/pages/team-detail-page";
import { TeamsPage } from "@/pages/teams-page";
import { TemplateGalleryPage } from "@/pages/template-gallery-page";
import { WorkflowsPage } from "@/pages/workflows-page";

// A person bound to one namespace by RoleBinding can read only the namespaces they work in, so
// every console list must say which: the selected workspace, or nothing for "All workspaces",
// which the BFF reads as every namespace the caller works in. Writes on an item are gated by the
// item's namespace, not the shell's.

const WORKSPACES = [{ name: "team-a" }, { name: "team-b" }];

interface Stub {
  // allowed[namespace][resource][verb]; a namespace absent here answers an empty map.
  caps?: Record<string, Record<string, Record<string, boolean>>>;
  // bodies by path, for the reads a test cares about.
  bodies?: Record<string, unknown>;
}

function installFetch(stub: Stub = {}): string[] {
  const urls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      urls.push(url);
      const [path, query = ""] = url.split("?");
      const ok = (body: unknown) =>
        Promise.resolve({
          ok: true,
          status: 200,
          json: async () => body,
          text: async () => JSON.stringify(body),
        } as Response);
      if (path === "/api/namespaces") return ok({ namespaces: WORKSPACES });
      if (path === "/api/capabilities") {
        const ns = new URLSearchParams(query).get("namespace") ?? "";
        return ok({ namespace: ns, allowed: stub.caps?.[ns] ?? {}, flows: {} });
      }
      if (stub.bodies && path in stub.bodies) return ok(stub.bodies[path]);
      // An empty answer in every shape a list page reads.
      return ok({ items: [], servers: [], tools: [], entries: [], runs: [], templates: [] });
    }),
  );
  return urls;
}

function renderAt(path: string, route: string, element: React.ReactElement) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path={route} element={element} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function selectWorkspace(ns: string) {
  localStorage.setItem("ctxmesh.namespace", ns);
}

afterEach(() => {
  vi.restoreAllMocks();
});

// The fourth field is the query that opens the page on the tab holding the list.
const SHELL_LISTS: [string, string, () => React.ReactElement, string?][] = [
  ["Knowledge Bases", "/api/knowledgebases", () => <KnowledgeBasesPage />],
  ["MCP servers", "/api/mcpservers", () => <McpServersPage />],
  ["MCP approvals", "/api/mcp/approvals", () => <ApprovalsPage />, "?tab=mcp"],
  ["Teams", "/api/teams", () => <TeamsPage />],
  ["Guardrails", "/api/guardrailpolicies", () => <GuardrailPoliciesPage />],
  ["Workflows", "/api/workflows", () => <WorkflowsPage />],
];

describe("every list page reads the selected workspace", () => {
  for (const [page, list, element, search = ""] of SHELL_LISTS) {
    it(`${page} sends the selected workspace`, async () => {
      selectWorkspace("team-b");
      const urls = installFetch();
      renderAt(`/page${search}`, "/page", element());
      await waitFor(() => expect(urls).toContain(`${list}?namespace=team-b`));
    });

    it(`${page} sends no namespace under All workspaces`, async () => {
      selectWorkspace("");
      const urls = installFetch();
      renderAt(`/page${search}`, "/page", element());
      await waitFor(() => expect(urls).toContain(list));
      expect(urls.filter((u) => u.startsWith(`${list}?`))).toEqual([]);
    });
  }

  it("a team's page reads the team's own namespace, whatever is selected", async () => {
    selectWorkspace("team-a");
    const urls = installFetch();
    renderAt("/teams/team-b/crew", "/teams/:ns/:name", <TeamDetailPage />);
    await waitFor(() => expect(urls).toContain("/api/teams?namespace=team-b"));
  });

  it("composing a team offers the registries of the workspace it lands in", async () => {
    selectWorkspace("team-b");
    const urls = installFetch();
    renderAt("/teams/new", "/teams/new", <CreateTeamPage />);
    await waitFor(() => expect(urls).toContain("/api/agentregistries?namespace=team-b"));
  });

  it("the template gallery reads the working namespace under All workspaces", async () => {
    selectWorkspace("");
    const urls = installFetch();
    renderAt("/templates", "/templates", <TemplateGalleryPage />);
    await waitFor(() => expect(urls).toContain("/api/templates?namespace=team-a"));
  });

  it("the MCP catalog reads, and checks ownership in, the working namespace", async () => {
    selectWorkspace("team-b");
    const urls = installFetch();
    renderAt("/templates?tab=mcp", "/templates", <TemplateGalleryPage />);
    await waitFor(() => expect(urls).toContain("/api/catalog?namespace=team-b"));
    expect(urls).toContain("/api/mcpservers?namespace=team-b");
  });
});

describe("write gates ask about the resource's namespace", () => {
  const MAY_UPDATE_KBS = { knowledgebases: { get: true, list: true, update: true } };
  const MAY_READ_KBS = { knowledgebases: { get: true, list: true, update: false } };
  const KB = {
    name: "my-kb",
    namespace: "team-b",
    phase: "Ready",
    chunkCount: 1,
    documentCount: 1,
    sizeBytes: 1,
    embeddingRoute: "embed",
    sourceType: "upload",
    conditions: [],
  };

  it("a knowledge base in a namespace the caller may change offers Upload and Ingest", async () => {
    selectWorkspace("team-a");
    installFetch({
      caps: { "team-a": MAY_READ_KBS, "team-b": MAY_UPDATE_KBS },
      bodies: { "/api/knowledgebases/my-kb": KB },
    });
    renderAt("/knowledgebases/team-b/my-kb", "/knowledgebases/:ns/:name", <KBDetailPage />);
    expect(await screen.findByTestId("ingest-button")).toBeInTheDocument();
    expect(screen.getByTestId("upload-submit")).toBeInTheDocument();
  });

  it("a knowledge base in a namespace the caller may only read offers neither", async () => {
    selectWorkspace("team-a");
    installFetch({
      caps: { "team-a": MAY_UPDATE_KBS, "team-b": MAY_READ_KBS },
      bodies: { "/api/knowledgebases/my-kb": KB },
    });
    renderAt("/knowledgebases/team-b/my-kb", "/knowledgebases/:ns/:name", <KBDetailPage />);
    expect(await screen.findByTestId("kb-upload-read-only")).toBeInTheDocument();
    expect(screen.queryByTestId("ingest-button")).not.toBeInTheDocument();
  });

  it("the catalog offers Bind per tool, by the tool's namespace, and keys each card uniquely", async () => {
    selectWorkspace("");
    const consoleError = vi.spyOn(console, "error");
    const tool = { name: "lookup", registry: "tools", description: "Look things up" };
    const urls = installFetch({
      caps: {
        "team-a": { mcptoolbindings: { create: true } },
        "team-b": { mcptoolbindings: { create: false } },
      },
      bodies: {
        "/api/tools": {
          tools: [
            { ...tool, namespace: "team-a" },
            { ...tool, namespace: "team-b" },
            { ...tool, name: "fetch", namespace: "team-b" },
          ],
        },
      },
    });
    renderAt("/tools?tab=tools", "/tools", <McpServersPage />);

    const cards = await screen.findAllByTestId("catalog-tool-lookup");
    expect(cards).toHaveLength(2);
    await waitFor(() => {
      expect(within(cards[0]).getByTestId("catalog-bind-lookup")).toBeEnabled();
      expect(within(cards[1]).getByTestId("catalog-bind-lookup")).toBeDisabled();
    });
    expect(within(screen.getByTestId("catalog-tool-fetch")).getByTestId("catalog-bind-fetch")).toBeDisabled();
    expect(urls.filter((u) => u === "/api/capabilities?namespace=team-b")).toHaveLength(1);
    const duplicateKey = consoleError.mock.calls.some((args) =>
      String(args[0]).includes("same key"),
    );
    expect(duplicateKey).toBe(false);
  });
});

// Under "All workspaces" a row's Edit, Delete, Share or Approve acts in the row's namespace, so it
// is offered by that namespace's answer. The working namespace is team-a, the first workspace.
describe("row controls follow the row's namespace under All workspaces", () => {
  const operator = (resource: string) => ({
    [resource]: { get: true, list: true, create: true, update: true, delete: true },
  });
  const viewer = (resource: string) => ({
    [resource]: { get: true, list: true, create: false, update: false, delete: false },
  });

  interface RowCase {
    page: string;
    list: string;
    element: () => React.ReactElement;
    /** The query that opens the page on the tab holding the list. */
    search?: string;
    resource: string;
    rows: (ns: string) => Record<string, unknown>;
    // The controls a row in ns offers to someone who may change it.
    controls: (ns: string) => string[];
  }

  const name = (ns: string) => `x-${ns}`;
  const CASES: RowCase[] = [
    {
      page: "MCP approvals",
      list: "/api/mcp/approvals",
      element: () => <ApprovalsPage />,
      search: "?tab=mcp",
      resource: "agentregistries",
      rows: (ns) => ({ namespace: ns, name: name(ns), toolCount: 1 }),
      controls: (ns) => [`mcp-approve-${ns}-${name(ns)}`, `mcp-reject-${ns}-${name(ns)}`],
    },
    {
      page: "MCP servers",
      list: "/api/mcpservers",
      element: () => <McpServersPage />,
      resource: "agentregistries",
      rows: (ns) => ({ namespace: ns, name: name(ns), url: "https://mcp.example/", toolCount: 1, status: "approved" }),
      controls: (ns) => [`share-mcp-${name(ns)}`, `delete-mcp-${name(ns)}`],
    },
    {
      page: "Agents",
      list: "/api/agents",
      element: () => <AgentsPage />,
      resource: "agentdeployments",
      rows: (ns) => ({ namespace: ns, name: name(ns), image: "img:1", phase: "Ready", ready: true }),
      controls: (ns) => [`edit-${name(ns)}`, `delete-${name(ns)}`],
    },
    {
      page: "Model routes",
      list: "/api/modelroutes",
      element: () => <ModelRoutesPage />,
      resource: "modelroutes",
      rows: (ns) => ({
        namespace: ns,
        name: name(ns),
        phase: "Ready",
        ready: true,
        providers: [{ provider: "openai", model: "gpt-4o", priority: 1, secretBindingRef: "k" }],
      }),
      controls: (ns) => [`edit-${name(ns)}`, `delete-${name(ns)}`],
    },
    {
      page: "Secret bindings",
      list: "/api/secretbindings",
      element: () => <SecretBindingsPage />,
      resource: "secretbindings",
      rows: (ns) => ({
        namespace: ns,
        name: name(ns),
        backend: "kubernetes",
        secretRef: { name: "s", key: "k" },
        phase: "Resolved",
        ready: true,
      }),
      controls: (ns) => [`edit-${name(ns)}`, `delete-${name(ns)}`],
    },
    {
      page: "Agent registries",
      list: "/api/agentregistries",
      element: () => <AgentRegistriesPage />,
      resource: "agentregistries",
      rows: (ns) => ({
        namespace: ns,
        name: name(ns),
        registryId: `reg-${ns}`,
        memberSelector: { matchLabels: {} },
        guards: { maxDepth: 5, hopBudget: 20 },
        roles: [],
        members: [],
        phase: "Ready",
        ready: true,
      }),
      controls: (ns) => [`edit-${name(ns)}`, `delete-${name(ns)}`],
    },
    {
      page: "Evals",
      list: "/api/evalsuites",
      element: () => <EvalsPage />,
      resource: "evalsuites",
      rows: (ns) => ({ namespace: ns, name: name(ns), datasetRef: "d", scorers: ["exact-match"], ready: true }),
      controls: (ns) => [`eval-delete-${name(ns)}`],
    },
    {
      page: "Prompts",
      list: "/api/promptversions",
      element: () => <PromptsPage />,
      resource: "promptversions",
      rows: (ns) => ({ namespace: ns, name: name(ns), ref: "abc1234", promptName: `p-${ns}`, createdAt: "2026-07-01T00:00:00Z" }),
      controls: (ns) => [`prompt-delete-${name(ns)}`],
    },
  ];

  for (const c of CASES) {
    for (const [may, mayNot] of [
      ["team-a", "team-b"],
      ["team-b", "team-a"],
    ] as const) {
      it(`${c.page}: an operator in ${may} and a viewer in ${mayNot} gets controls on ${may}'s rows only`, async () => {
        selectWorkspace("");
        installFetch({
          caps: { [may]: operator(c.resource), [mayNot]: viewer(c.resource) },
          bodies: {
            [c.list]: {
              items: [c.rows("team-a"), c.rows("team-b")],
              servers: [c.rows("team-a"), c.rows("team-b")],
              nextCursor: "",
            },
          },
        });
        renderAt(`/page${c.search ?? ""}`, "/page", c.element());
        expect((await screen.findAllByText(name(mayNot))).length).toBeGreaterThan(0);
        await waitFor(() => {
          for (const id of c.controls(may)) expect(screen.queryByTestId(id)).not.toBeNull();
          for (const id of c.controls(mayNot)) expect(screen.queryByTestId(id)).toBeNull();
        });
      });
    }
  }
});

// A detail page opened from an "All workspaces" row gates its writes on the item's namespace.
describe("a detail page gates on the item's namespace", () => {
  const CASES: [string, string, string, string, string, string, () => React.ReactElement, Record<string, unknown>][] = [
    [
      "model route",
      "/routes/:ns/:name",
      "/api/modelroutes",
      "modelroutes",
      "edit-route-button",
      "route-detail-page",
      () => <ModelRouteDetailPage />,
      { phase: "Ready", ready: true, providers: [] },
    ],
    [
      "secret binding",
      "/secrets/:ns/:name",
      "/api/secretbindings",
      "secretbindings",
      "edit-secret-button",
      "secret-detail-page",
      () => <SecretBindingDetailPage />,
      { backend: "kubernetes", secretRef: { name: "s", key: "k" }, phase: "Resolved", ready: true },
    ],
    [
      "agent registry",
      "/registries/:ns/:name",
      "/api/agentregistries",
      "agentregistries",
      "edit-registry-button",
      "registry-detail-page",
      () => <AgentRegistryDetailPage />,
      {
        registryId: "r",
        memberSelector: { matchLabels: {} },
        roles: [],
        status: { members: [], phase: "Ready", ready: true },
      },
    ],
  ];

  for (const [kind, route, endpoint, resource, edit, root, element, body] of CASES) {
    const prefix = route.split("/:")[0];
    for (const mayEdit of [true, false]) {
      it(`a ${kind} in team-b ${mayEdit ? "offers" : "does not offer"} Edit by team-b's answer while the shell is in team-a`, async () => {
        selectWorkspace("team-a");
        const write = { get: true, list: true, update: true, delete: true };
        const read = { get: true, list: true, update: false, delete: false };
        installFetch({
          // The shell's namespace answers the opposite of team-b, so only team-b's answer can pass.
          caps: {
            "team-a": { [resource]: mayEdit ? read : write },
            "team-b": { [resource]: mayEdit ? write : read },
          },
          bodies: { [`${endpoint}/team-b/item`]: { name: "item", namespace: "team-b", ...body } },
        });
        renderAt(`${prefix}/team-b/item`, route, element());
        if (mayEdit) {
          expect(await screen.findByTestId(edit)).toBeInTheDocument();
        } else {
          expect(await screen.findByTestId(root)).toBeInTheDocument();
          await waitFor(() => expect(screen.queryByTestId(edit)).not.toBeInTheDocument());
        }
      });
    }
  }

  for (const mayEdit of [true, false]) {
    it(`an agent in team-b ${mayEdit ? "offers" : "does not offer"} Edit by team-b's answer while the shell is in team-a`, async () => {
      selectWorkspace("team-a");
      const write = { get: true, list: true, create: true, update: true, delete: true };
      const read = { get: true, list: true, create: false, update: false, delete: false };
      installFetch({
        caps: {
          "team-a": { agentdeployments: mayEdit ? read : write },
          "team-b": { agentdeployments: mayEdit ? write : read },
        },
        bodies: {
          "/api/agents/team-b/item": {
            name: "item",
            namespace: "team-b",
            image: "img:1",
            executionModel: "serving",
            role: "assistant",
            scaling: { min: 0, max: 1 },
            phase: "Ready",
            ready: true,
            conditions: [],
            bindings: [],
            versions: [],
            managedOutsideUI: false,
            drift: false,
          },
        },
      });
      renderAt("/agents/team-b/item", "/agents/:ns/:name", <AgentDetailPage />);
      if (mayEdit) {
        expect(await screen.findByRole("button", { name: "Edit" })).toBeInTheDocument();
      } else {
        expect(await screen.findByTestId("agent-detail-page")).toBeInTheDocument();
        await waitFor(() => expect(screen.queryByRole("button", { name: "Edit" })).toBeNull());
      }
    });
  }
});
