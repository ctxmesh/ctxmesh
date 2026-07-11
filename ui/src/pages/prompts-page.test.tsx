import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { PromptsPage } from "@/pages/prompts-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// PromptsPage tests (m17.12):
//   1. Prompt diff: pick from/to → renders textual diff with resolveMode="textual" shown.
//   2. 501 → "not configured" calm state (NOT error toast, NOT fabricated diff).
//   3. 404 → not-found distinct state.
//   4. 502 → resolve-failed distinct state (NOT fabricated diff).
//   5. RBAC: a viewer sees no create/delete buttons.

// ---- fixtures ----------------------------------------------------------------

const EDITOR_CAPS = {
  promptversions: { create: true, delete: true, update: true },
};
const VIEWER_CAPS = {
  promptversions: { create: false, delete: false, update: false },
};

const VERSION_LIST = [
  {
    name: "v1",
    namespace: "default",
    ref: "abc1234",
    promptName: "my-prompt",
    createdAt: "2026-07-01T00:00:00Z",
  },
  {
    name: "v2",
    namespace: "default",
    ref: "def5678",
    promptName: "my-prompt",
    createdAt: "2026-07-10T00:00:00Z",
  },
];

const DIFF_RESPONSE = {
  resolveMode: "textual" as const,
  lines: [
    { op: " " as const, content: "You are a helpful assistant." },
    { op: "-" as const, content: "Please be brief." },
    { op: "+" as const, content: "Please be thorough and detailed." },
  ],
};

type FetchSetup = {
  caps?: Record<string, Record<string, boolean>>;
  versions?: unknown[];
  versionsStatus?: number;
  diffBody?: unknown;
  diffStatus?: number;
  createOk?: boolean;
  deleteOk?: boolean;
};

function installFetch(opts: FetchSetup = {}) {
  const calls: { url: string; method: string; body: string }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({ url, method, body: typeof init?.body === "string" ? init.body : "" });
      const j = (body: unknown, ok = true, status = ok ? 200 : 500) =>
        Promise.resolve({ ok, status, json: async () => body } as Response);

      if (url.startsWith("/api/namespaces")) return j({ namespaces: [] });
      if (url.startsWith("/api/capabilities"))
        return j({ namespace: "", allowed: opts.caps ?? EDITOR_CAPS });

      // List
      if (url.startsWith("/api/promptversions") && !url.includes("/diff") && method === "GET") {
        const status = opts.versionsStatus ?? 200;
        const ok = status < 400;
        return Promise.resolve({
          ok,
          status,
          json: async () =>
            ok
              ? { items: opts.versions ?? VERSION_LIST, nextCursor: "" }
              : { error: "forbidden" },
        } as Response);
      }

      // Diff
      if (url.match(/\/api\/promptversions\/[^/]+\/[^/]+\/diff/) && method === "GET") {
        const status = opts.diffStatus ?? 200;
        const ok = status === 200;
        return Promise.resolve({
          ok: status < 400,
          status,
          json: async () =>
            ok ? (opts.diffBody ?? DIFF_RESPONSE) : { error: "diff error" },
        } as Response);
      }

      // Create
      if (url === "/api/promptversions" && method === "POST") {
        const ok = opts.createOk ?? true;
        return Promise.resolve({
          ok,
          status: ok ? 201 : 403,
          json: async () =>
            ok
              ? {
                  name: "v3",
                  namespace: "default",
                  ref: "ghi9012",
                  promptName: "my-prompt",
                }
              : { error: "not allowed" },
        } as Response);
      }

      // Delete
      if (url.match(/\/api\/promptversions\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const ok = opts.deleteOk ?? true;
        return Promise.resolve({
          ok,
          status: ok ? 204 : 403,
          json: async () => (ok ? {} : { error: "forbidden" }),
        } as Response);
      }

      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderPage(caps?: Record<string, Record<string, boolean>>, setup: FetchSetup = {}) {
  installFetch({ ...setup, caps: caps ?? setup.caps });
  return render(
    <MemoryRouter>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <PromptsPage />
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
  localStorage.clear();
  sessionStorage.clear();
});

describe("PromptsPage", () => {
  it("renders prompt version list", async () => {
    renderPage(EDITOR_CAPS);
    expect(await screen.findByTestId("prompt-version-v1")).toBeInTheDocument();
    expect(screen.getByTestId("prompt-version-v2")).toBeInTheDocument();
    expect(screen.getByText("v1")).toBeInTheDocument();
    expect(screen.getByText("v2")).toBeInTheDocument();
  });

  it("diff: pick from/to → renders textual diff with resolveMode shown", async () => {
    renderPage(EDITOR_CAPS, { diffBody: DIFF_RESPONSE });
    await screen.findByTestId("prompt-version-v1");

    // Fill in from/to
    fireEvent.change(screen.getByTestId("diff-from-input"), {
      target: { value: "v1" },
    });
    fireEvent.change(screen.getByTestId("diff-to-input"), {
      target: { value: "v2" },
    });
    fireEvent.click(screen.getByTestId("diff-compare-btn"));

    // Wait for diff output
    await waitFor(() => {
      expect(screen.getByTestId("prompt-diff")).toBeInTheDocument();
    });

    // resolveMode="textual" ALWAYS shown explicitly
    expect(screen.getByTestId("prompt-diff-resolve-mode")).toBeInTheDocument();
    expect(screen.getByText(/resolve mode: textual/i)).toBeInTheDocument();

    // Diff lines rendered
    expect(screen.getByTestId("prompt-diff-lines")).toBeInTheDocument();
    expect(screen.getByText("You are a helpful assistant.")).toBeInTheDocument();
    expect(screen.getByText("Please be brief.")).toBeInTheDocument();
    expect(screen.getByText("Please be thorough and detailed.")).toBeInTheDocument();
  });

  it("501 → calm 'not configured' state — no error toast, no fabricated diff", async () => {
    renderPage(EDITOR_CAPS, { diffStatus: 501 });
    await screen.findByTestId("prompt-version-v1");

    fireEvent.change(screen.getByTestId("diff-from-input"), {
      target: { value: "v1" },
    });
    fireEvent.change(screen.getByTestId("diff-to-input"), {
      target: { value: "v2" },
    });
    fireEvent.click(screen.getByTestId("diff-compare-btn"));

    // Calm "not configured" state
    await waitFor(() => {
      expect(screen.getByTestId("prompt-diff-not-configured")).toBeInTheDocument();
    });
    expect(screen.getByText(/not configured/i)).toBeInTheDocument();

    // No diff content — no fabricated diff
    expect(screen.queryByTestId("prompt-diff")).not.toBeInTheDocument();
    expect(screen.queryByTestId("prompt-diff-lines")).not.toBeInTheDocument();
    expect(screen.queryByText("Please be brief.")).not.toBeInTheDocument();
  });

  it("404 → not-found state — distinct from 502 and other errors", async () => {
    renderPage(EDITOR_CAPS, { diffStatus: 404 });
    await screen.findByTestId("prompt-version-v1");

    fireEvent.change(screen.getByTestId("diff-from-input"), {
      target: { value: "v1" },
    });
    fireEvent.change(screen.getByTestId("diff-to-input"), {
      target: { value: "v2" },
    });
    fireEvent.click(screen.getByTestId("diff-compare-btn"));

    await waitFor(() => {
      expect(screen.getByTestId("prompt-diff-not-found")).toBeInTheDocument();
    });
    expect(screen.getByText(/not found/i)).toBeInTheDocument();

    // No fabricated diff
    expect(screen.queryByTestId("prompt-diff")).not.toBeInTheDocument();
    // The not-found state is distinct from the resolve-failed state
    expect(screen.queryByTestId("prompt-diff-resolve-failed")).not.toBeInTheDocument();
  });

  it("502 → resolve-failed state — distinct from 404, shows retry button", async () => {
    renderPage(EDITOR_CAPS, { diffStatus: 502 });
    await screen.findByTestId("prompt-version-v1");

    fireEvent.change(screen.getByTestId("diff-from-input"), {
      target: { value: "v1" },
    });
    fireEvent.change(screen.getByTestId("diff-to-input"), {
      target: { value: "v2" },
    });
    fireEvent.click(screen.getByTestId("diff-compare-btn"));

    await waitFor(() => {
      expect(screen.getByTestId("prompt-diff-resolve-failed")).toBeInTheDocument();
    });
    expect(screen.getByText(/resolve failed/i)).toBeInTheDocument();
    // Retry button present
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();

    // No fabricated diff
    expect(screen.queryByTestId("prompt-diff")).not.toBeInTheDocument();
    // Distinct from not-found
    expect(screen.queryByTestId("prompt-diff-not-found")).not.toBeInTheDocument();
  });

  it("RBAC: viewer sees no create or delete buttons", async () => {
    renderPage(VIEWER_CAPS);
    await screen.findByTestId("prompt-version-v1");

    expect(screen.queryByTestId("prompts-new-btn")).not.toBeInTheDocument();
    expect(screen.queryByTestId("prompt-delete-v1")).not.toBeInTheDocument();
    expect(screen.queryByTestId("prompt-delete-v2")).not.toBeInTheDocument();
  });
});
