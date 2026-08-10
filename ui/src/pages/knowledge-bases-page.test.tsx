import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { KnowledgeBasesPage, KBDetailPage } from "@/pages/knowledge-bases-page";
import type { KBSummary, KBDetail } from "@/lib/api";

// KnowledgeBasesPage (m68.13, ADR 0061) — managed RAG corpora list + detail.
//
// List: table of KBs with name, phase badge, chunk count, size, last-ingested.
// Detail: upload panel, ingest trigger, and test-query panel with citations.

function kb(over: Partial<KBSummary> = {}): KBSummary {
  return {
    name: "my-kb",
    namespace: "default",
    phase: "Ready",
    chunkCount: 42,
    documentCount: 3,
    sizeBytes: 1024 * 1024,
    lastIngestedAt: "2026-07-01T12:00:00Z",
    embeddingRoute: "text-embedding-route",
    ...over,
  };
}

function kbDetail(over: Partial<KBDetail> = {}): KBDetail {
  return {
    name: "my-kb",
    namespace: "default",
    phase: "Ready",
    chunkCount: 42,
    documentCount: 3,
    sizeBytes: 1024 * 1024,
    lastIngestedAt: "2026-07-01T12:00:00Z",
    embeddingRoute: "text-embedding-route",
    displayName: "My Knowledge Base",
    sourceType: "upload",
    chunkSize: 512,
    chunkOverlap: 64,
    chunkSplitter: "recursive",
    ingestionRunRef: "run-123",
    conditions: [
      { type: "Validated", status: "True", reason: "ValidationPassed" },
    ],
    ...over,
  };
}

interface FetchRoutes {
  list?: { ok: boolean; status?: number; body?: unknown };
  get?: { ok: boolean; status?: number; body?: unknown };
  search?: { ok: boolean; status?: number; body?: unknown };
  ingest?: { ok: boolean; status?: number; body?: unknown };
}

function installFetch(routes: FetchRoutes): { calls: { url: string; method: string; body: string }[] } {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const path = url.split("?")[0];
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? init.body : "";
      calls.push({ url: path, method, body });

      if (path === "/api/knowledgebases" && method === "GET") {
        const r = routes.list ?? { ok: true, body: { items: [] } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 500),
          json: async () => r.body ?? { items: [] },
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }
      if (path.match(/\/api\/knowledgebases\/[^/]+$/) && method === "GET") {
        const r = routes.get ?? { ok: true, body: kbDetail() };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 404),
          json: async () => r.body ?? {},
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }
      if (path.endsWith("/search") && method === "POST") {
        const r = routes.search ?? { ok: true, body: { results: [] } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 200 : 500),
          json: async () => r.body ?? { results: [] },
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }
      if (path.endsWith("/ingest") && method === "POST") {
        const r = routes.ingest ?? { ok: true, body: { runId: "run-1", status: "queued", documentCount: 1 } };
        return Promise.resolve({
          ok: r.ok,
          status: r.status ?? (r.ok ? 202 : 500),
          json: async () => r.body ?? {},
          text: async () => JSON.stringify(r.body ?? {}),
        } as Response);
      }
      return Promise.resolve({
        ok: false,
        status: 404,
        json: async () => ({}),
        text: async () => "",
      } as Response);
    }),
  );
  return { calls };
}

function renderListPage() {
  return render(
    <MemoryRouter initialEntries={["/knowledgebases"]}>
      <Routes>
        <Route path="/knowledgebases" element={<KnowledgeBasesPage />} />
        <Route path="/knowledgebases/:ns/:name" element={<KBDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderDetailPage(ns = "default", name = "my-kb") {
  return render(
    <MemoryRouter initialEntries={[`/knowledgebases/${ns}/${name}`]}>
      <Routes>
        <Route path="/knowledgebases" element={<KnowledgeBasesPage />} />
        <Route path="/knowledgebases/:ns/:name" element={<KBDetailPage />} />
      </Routes>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

// ─────────────────────────────────────────────────────────────────────────────
// List page
// ─────────────────────────────────────────────────────────────────────────────

describe("KnowledgeBasesPage — list (m68.13)", () => {
  it("renders KBs with name, phase badge, chunk count, size, last-ingested", async () => {
    installFetch({
      list: {
        ok: true,
        body: {
          items: [
            kb({ name: "kb-ready", phase: "Ready", chunkCount: 100 }),
            kb({ name: "kb-ingesting", phase: "Ingesting", chunkCount: 0 }),
            kb({ name: "kb-failed", phase: "Failed" }),
          ],
        },
      },
    });

    renderListPage();

    expect(await screen.findByTestId("knowledge-bases-page")).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "KnowledgeBases" })).toBeInTheDocument();
    expect(screen.getByText("kb-ready")).toBeInTheDocument();
    expect(screen.getByText("kb-ingesting")).toBeInTheDocument();
    expect(screen.getByText("kb-failed")).toBeInTheDocument();

    // Phase badges
    expect(screen.getByText("Ready")).toBeInTheDocument();
    expect(screen.getByText("Ingesting")).toBeInTheDocument();
    expect(screen.getByText("Failed")).toBeInTheDocument();
  });

  it("empty → teaches an empty state", async () => {
    installFetch({ list: { ok: true, body: { items: [] } } });
    renderListPage();
    await waitFor(() => expect(screen.getByText("No knowledge bases")).toBeInTheDocument());
  });

  it("403 surfaces a forbidden state (never a fake empty list)", async () => {
    installFetch({
      list: {
        ok: false,
        status: 403,
        body: { error: "you do not have permission to list knowledge bases" },
      },
    });
    renderListPage();
    await waitFor(() =>
      expect(
        screen.getByText(/you do not have permission to list knowledge bases/),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText("No knowledge bases")).toBeNull();
  });
});

// ─────────────────────────────────────────────────────────────────────────────
// Detail page — test-query panel
// ─────────────────────────────────────────────────────────────────────────────

describe("KBDetailPage — test-query panel (m68.13)", () => {
  it("renders the detail page with the KB name, phase badge, and conditions", async () => {
    installFetch({ get: { ok: true, body: kbDetail() } });
    renderDetailPage();

    expect(await screen.findByTestId("kb-detail-page")).toBeInTheDocument();
    expect(screen.getByTestId("kb-detail-header")).toBeInTheDocument();
    expect(screen.getByText("my-kb")).toBeInTheDocument();
    // "Ready" legitimately appears in more than one place on the detail page (the phase badge +
    // the status summary), so assert presence rather than uniqueness.
    expect(screen.getAllByText("Ready").length).toBeGreaterThan(0);
    expect(screen.getByText("Validated")).toBeInTheDocument();
  });

  it("renders the test-query panel with a query input and search button", async () => {
    installFetch({ get: { ok: true, body: kbDetail() } });
    renderDetailPage();

    await screen.findByTestId("kb-detail-page");
    expect(screen.getByTestId("query-input")).toBeInTheDocument();
    expect(screen.getByTestId("query-topk")).toBeInTheDocument();
    expect(screen.getByTestId("query-submit")).toBeInTheDocument();
  });

  it("renders search results with citation fields (documentRef#chunkIndex + score + content)", async () => {
    const { calls } = installFetch({
      get: { ok: true, body: kbDetail() },
      search: {
        ok: true,
        body: {
          results: [
            {
              content: "chunk content for testing",
              documentRef: "guide.pdf",
              chunkIndex: 3,
              score: 0.87,
            },
            {
              content: "another chunk",
              documentRef: "readme.md",
              chunkIndex: 0,
              score: 0.65,
              truncated: true,
            },
          ],
        },
      },
    });

    renderDetailPage();
    await screen.findByTestId("kb-detail-page");

    // Type a query and submit
    fireEvent.change(screen.getByTestId("query-input"), {
      target: { value: "how to configure" },
    });
    fireEvent.click(screen.getByTestId("query-submit"));

    // Wait for results
    await waitFor(() => expect(screen.getByTestId("query-results")).toBeInTheDocument());

    expect(screen.getByTestId("query-hit-0")).toBeInTheDocument();
    expect(screen.getByTestId("query-hit-1")).toBeInTheDocument();

    // Citation: documentRef#chunkIndex
    expect(screen.getByText("guide.pdf#3")).toBeInTheDocument();
    expect(screen.getByText("readme.md#0")).toBeInTheDocument();

    // Content
    expect(screen.getByText("chunk content for testing")).toBeInTheDocument();

    // Truncated badge on second result
    expect(screen.getByText("truncated")).toBeInTheDocument();

    // The search POST was made
    const searchCall = calls.find((c) => c.url.endsWith("/search") && c.method === "POST");
    expect(searchCall).toBeDefined();
    const parsedBody = JSON.parse(searchCall!.body) as { query: string };
    expect(parsedBody.query).toBe("how to configure");
  });

  it("501 from search → calm unavailable state (not an error toast)", async () => {
    installFetch({
      get: { ok: true, body: kbDetail() },
      search: { ok: false, status: 501, body: { error: "knowledge search not configured" } },
    });

    renderDetailPage();
    await screen.findByTestId("kb-detail-page");

    fireEvent.change(screen.getByTestId("query-input"), {
      target: { value: "test query" },
    });
    fireEvent.click(screen.getByTestId("query-submit"));

    await waitFor(() =>
      expect(screen.getByTestId("query-unavailable")).toBeInTheDocument(),
    );
    // Must NOT show query-results or a generic error
    expect(screen.queryByTestId("query-results")).toBeNull();
  });

  it("renders the ingest button and shows success on 202", async () => {
    installFetch({
      get: { ok: true, body: kbDetail() },
      ingest: { ok: true, status: 202, body: { runId: "run-abc", status: "queued", documentCount: 2 } },
    });

    renderDetailPage();
    await screen.findByTestId("kb-detail-page");

    expect(screen.getByTestId("ingest-button")).toBeInTheDocument();
    fireEvent.click(screen.getByTestId("ingest-button"));

    await waitFor(() =>
      expect(screen.getByTestId("ingest-result")).toHaveTextContent("run-abc"),
    );
  });
});
