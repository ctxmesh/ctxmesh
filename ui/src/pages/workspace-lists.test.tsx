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
import { McpApprovalsPage } from "@/pages/mcp-approvals-page";
import { McpServersPage } from "@/pages/mcp-servers-page";
import { TeamDetailPage } from "@/pages/team-detail-page";
import { TeamsPage } from "@/pages/teams-page";
import { TemplateGalleryPage } from "@/pages/template-gallery-page";
import { ToolCatalogPage } from "@/pages/tool-catalog-page";
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

const SHELL_LISTS: [string, string, () => React.ReactElement][] = [
  ["Knowledge Bases", "/api/knowledgebases", () => <KnowledgeBasesPage />],
  ["MCP servers", "/api/mcpservers", () => <McpServersPage />],
  ["MCP approvals", "/api/mcp/approvals", () => <McpApprovalsPage />],
  ["Teams", "/api/teams", () => <TeamsPage />],
  ["Guardrails", "/api/guardrailpolicies", () => <GuardrailPoliciesPage />],
  ["Workflows", "/api/workflows", () => <WorkflowsPage />],
];

describe("every list page reads the selected workspace", () => {
  for (const [page, list, element] of SHELL_LISTS) {
    it(`${page} sends the selected workspace`, async () => {
      selectWorkspace("team-b");
      const urls = installFetch();
      renderAt("/page", "/page", element());
      await waitFor(() => expect(urls).toContain(`${list}?namespace=team-b`));
    });

    it(`${page} sends no namespace under All workspaces`, async () => {
      selectWorkspace("");
      const urls = installFetch();
      renderAt("/page", "/page", element());
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
    renderAt("/tools", "/tools", <ToolCatalogPage />);

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
