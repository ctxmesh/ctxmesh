import { afterEach, describe, expect, it, vi } from "vitest";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";

import { SecretBindingsPage } from "@/pages/secret-bindings-page";
import { SecretBindingDetailPage, NewSecretBindingPage } from "@/pages/secret-binding-detail-page";
import { CapabilitiesProvider } from "@/lib/capabilities";
import { NamespaceProvider } from "@/lib/namespace";
import { ToastProvider } from "@/components/kit";

// Helpers --------------------------------------------------------------------

const DEFAULT_DETAIL = {
  name: "oai-key",
  namespace: "default",
  backend: "kubernetes",
  secretRef: { name: "my-oai-secret", key: "apiKey" },
  phase: "Resolved",
  ready: true,
};

function installFetch(opts: {
  caps?: Record<string, Record<string, boolean>>;
  bindings?: (qs: URLSearchParams) => { ok: boolean; status?: number; body: unknown };
  detail?: { ok: boolean; status?: number; body?: unknown };
  update?: { ok: boolean; status?: number; body?: unknown };
  remove?: { ok: boolean; status?: number };
  create?: { ok: boolean; status?: number; body?: unknown };
}) {
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
        return j({ namespace: "", allowed: opts.caps ?? { secretbindings: { create: true, update: true, delete: true } } });
      if (url.startsWith("/api/secretbindings") && method === "GET" && !url.includes("/default/")) {
        const qs = new URLSearchParams(url.split("?")[1] ?? "");
        const r = opts.bindings?.(qs) ?? { ok: true, body: { items: [], nextCursor: "" } };
        return j(r.body, r.ok, r.status ?? (r.ok ? 200 : 500));
      }
      if (url === "/api/secretbindings" && method === "POST") {
        const r = opts.create ?? { ok: true, body: { ...DEFAULT_DETAIL, name: "new-binding" } };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 201 : 422));
      }
      if (url.match(/\/api\/secretbindings\/[^/]+\/[^/]+$/) && method === "GET") {
        const r = opts.detail ?? { ok: true, body: DEFAULT_DETAIL };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 404));
      }
      if (url.match(/\/api\/secretbindings\/[^/]+\/[^/]+$/) && method === "PUT") {
        const r = opts.update ?? { ok: true, body: DEFAULT_DETAIL };
        return j(r.body ?? {}, r.ok, r.status ?? (r.ok ? 200 : 422));
      }
      if (url.match(/\/api\/secretbindings\/[^/]+\/[^/]+$/) && method === "DELETE") {
        const r = opts.remove ?? { ok: true };
        return Promise.resolve({ ok: r.ok, status: r.status ?? (r.ok ? 204 : 403), json: async () => ({}) } as Response);
      }
      return j({}, false, 404);
    }),
  );
  return calls;
}

function renderList() {
  return render(
    <MemoryRouter initialEntries={["/secrets"]}>
      <NamespaceProvider>
        <CapabilitiesProvider>
          <Routes>
            <Route path="secrets" element={<SecretBindingsPage />} />
          </Routes>
        </CapabilitiesProvider>
      </NamespaceProvider>
    </MemoryRouter>,
  );
}

function renderDetail(path = "/secrets/default/oai-key") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="secrets/:ns/:name" element={<SecretBindingDetailPage />} />
              <Route path="secrets" element={<div>secrets list</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

function renderCreate() {
  return render(
    <MemoryRouter initialEntries={["/secrets/new"]}>
      <ToastProvider>
        <NamespaceProvider>
          <CapabilitiesProvider>
            <Routes>
              <Route path="secrets/new" element={<NewSecretBindingPage />} />
              <Route path="secrets/:ns/:name" element={<div>detail</div>} />
            </Routes>
          </CapabilitiesProvider>
        </NamespaceProvider>
      </ToastProvider>
    </MemoryRouter>,
  );
}

afterEach(() => vi.restoreAllMocks());

// List tests -----------------------------------------------------------------

describe("SecretBindingsPage — list", () => {
  it("renders teaching empty state when BFF returns no items", async () => {
    installFetch({ bindings: () => ({ ok: true, body: { items: [], nextCursor: "" } }) });
    renderList();
    expect(await screen.findByText("No secret bindings yet")).toBeInTheDocument();
  });

  it("renders secretRef.name/key — never a value field", async () => {
    installFetch({
      bindings: () => ({
        ok: true,
        body: {
          items: [DEFAULT_DETAIL],
          nextCursor: "",
        },
      }),
    });
    renderList();
    // The ref is shown
    expect(await screen.findByText("my-oai-secret/apiKey")).toBeInTheDocument();
    // There must be no input with type=password or any "value" label
    expect(document.querySelector("input[type=password]")).toBeNull();
    expect(screen.queryByLabelText(/secret value/i)).toBeNull();
    expect(screen.queryByText(/credential/i)).toBeNull();
  });

  it("paginates with cursor", async () => {
    const calls = installFetch({
      bindings: (qs) => {
        const cursor = qs.get("cursor") ?? "";
        if (!cursor) return { ok: true, body: { items: [{ ...DEFAULT_DETAIL, name: "sb-0" }], nextCursor: "c1" } };
        return { ok: true, body: { items: [{ ...DEFAULT_DETAIL, name: "sb-1" }], nextCursor: "" } };
      },
    });
    renderList();
    expect(await screen.findByText("sb-0")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: /Next page/ }));
    expect(await screen.findByText("sb-1")).toBeInTheDocument();
    expect(calls.some((c) => c.url.includes("cursor=c1"))).toBe(true);
  });

  it("renders 403 as forbidden variant, not empty list", async () => {
    installFetch({ bindings: () => ({ ok: false, status: 403, body: { error: "forbidden: cannot list secretbindings" } }) });
    renderList();
    expect(await screen.findByText("You don't have permission to view secret bindings")).toBeInTheDocument();
    // the raw RBAC string is never surfaced on a 403 (M100 UI99-403)
    expect(screen.queryByText(/forbidden: cannot/)).toBeNull();
  });
});

// RBAC tests -----------------------------------------------------------------

describe("SecretBindingsPage — RBAC-aware row actions", () => {
  const oneSb = () => ({
    ok: true,
    body: { items: [DEFAULT_DETAIL], nextCursor: "" },
  });

  it("viewer sees no edit or delete buttons", async () => {
    installFetch({ caps: { secretbindings: { create: false, update: false, delete: false } }, bindings: () => oneSb() });
    renderList();
    await screen.findByText("oai-key");
    expect(screen.queryByTestId("edit-oai-key")).toBeNull();
    expect(screen.queryByTestId("delete-oai-key")).toBeNull();
  });

  it("caller with update+delete sees both buttons", async () => {
    installFetch({ caps: { secretbindings: { create: true, update: true, delete: true } }, bindings: () => oneSb() });
    renderList();
    await screen.findByText("oai-key");
    expect(screen.getByTestId("edit-oai-key")).toBeInTheDocument();
    expect(screen.getByTestId("delete-oai-key")).toBeInTheDocument();
  });
});

// SECURITY: no-value invariant tests -----------------------------------------

describe("SecretBinding — NO VALUE field in UI (security invariant)", () => {
  it("detail page shows ref + status but NO value/data input or display", async () => {
    installFetch({});
    renderDetail();
    await screen.findByTestId("secret-detail-page");
    // Shows the ref
    expect(screen.getByTestId("secret-ref-name")).toHaveTextContent("my-oai-secret");
    expect(screen.getByTestId("secret-ref-key")).toHaveTextContent("apiKey");
    // No-value note is shown
    expect(screen.getByTestId("no-value-note")).toBeInTheDocument();
    // No password input or value field
    expect(document.querySelector("input[type=password]")).toBeNull();
    expect(screen.queryByLabelText(/value/i)).toBeNull();
    expect(screen.queryByLabelText(/credential/i)).toBeNull();
    // The detail panel exists but does not contain "value" fields
    const panel = screen.getByTestId("secret-detail-panel");
    expect(panel.querySelector("[data-testid=secret-ref-value]")).toBeNull();
  });

  it("edit wizard form shows only secretRef.name and key — no value field", async () => {
    installFetch({});
    renderDetail("/secrets/default/oai-key?edit=1");
    await screen.findByTestId("no-value-form-note");
    // The no-value notice is present
    expect(screen.getByTestId("no-value-form-note")).toBeInTheDocument();
    // secretRef name and key fields
    expect(screen.getByTestId("edit-sb-secret-name")).toBeInTheDocument();
    expect(screen.getByTestId("edit-sb-secret-key")).toBeInTheDocument();
    // NO password input or credential-value field
    expect(document.querySelector("input[type=password]")).toBeNull();
    // No input dedicated to a credential value (distinct from labels that say "not the value")
    expect(screen.queryByLabelText(/^(secret )?value$/i)).toBeNull();
    expect(screen.queryByLabelText(/credential/i)).toBeNull();
    // No input element with a "value" or "secret-data" testid
    expect(document.querySelector("input[data-testid*=value]")).toBeNull();
    expect(document.querySelector("input[data-testid*=secret-data]")).toBeNull();
  });

  it("create wizard shows secretRef fields but no value input", async () => {
    installFetch({});
    renderCreate();
    // Navigate to the ref step (wizard Next button is labeled "Continue")
    fireEvent.change(screen.getByTestId("new-sb-name"), { target: { value: "my-binding" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("no-value-create-note");
    // Notice is shown
    expect(screen.getByTestId("no-value-create-note")).toBeInTheDocument();
    // Name and key inputs exist
    expect(screen.getByTestId("new-sb-secret-name")).toBeInTheDocument();
    expect(screen.getByTestId("new-sb-secret-key")).toBeInTheDocument();
    // No value input
    expect(document.querySelector("input[type=password]")).toBeNull();
    expect(document.querySelector("[data-testid*=secret-value]")).toBeNull();
  });

  it("POST body from create carries only secretRef (no value field)", async () => {
    const calls = installFetch({ create: { ok: true, body: { ...DEFAULT_DETAIL, name: "new-binding" } } });
    renderCreate();
    // Step 1: identity
    fireEvent.change(screen.getByTestId("new-sb-name"), { target: { value: "new-binding" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Step 2: ref
    await screen.findByTestId("new-sb-secret-name");
    fireEvent.change(screen.getByTestId("new-sb-secret-name"), { target: { value: "my-secret" } });
    fireEvent.change(screen.getByTestId("new-sb-secret-key"), { target: { value: "apiKey" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    // Review
    await screen.findByTestId("new-secret-review");
    fireEvent.click(screen.getByRole("button", { name: /Create binding/i }));
    await waitFor(() => {
      const postCall = calls.find((c) => c.method === "POST" && c.url === "/api/secretbindings");
      expect(postCall).toBeDefined();
      const body = JSON.parse(postCall!.body);
      // Has ref — never a value
      expect(body.secretRef.name).toBe("my-secret");
      expect(body.secretRef.key).toBe("apiKey");
      expect(body.value).toBeUndefined();
      expect(body.secretRef.value).toBeUndefined();
      expect(body.data).toBeUndefined();
      expect(body.credential).toBeUndefined();
    });
  });

  it("PUT body from edit carries only secretRef (no value field)", async () => {
    const calls = installFetch({ update: { ok: true, body: DEFAULT_DETAIL } });
    renderDetail("/secrets/default/oai-key?edit=1");
    await screen.findByTestId("edit-sb-secret-name");
    fireEvent.change(screen.getByTestId("edit-sb-secret-name"), { target: { value: "updated-secret" } });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("secret-edit-review");
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));
    await waitFor(() => {
      const putCall = calls.find((c) => c.method === "PUT" && c.url.includes("/api/secretbindings/"));
      expect(putCall).toBeDefined();
      const body = JSON.parse(putCall!.body);
      expect(body.secretRef.name).toBe("updated-secret");
      expect(body.value).toBeUndefined();
      expect(body.secretRef.value).toBeUndefined();
    });
  });
});

// Delete tests ---------------------------------------------------------------

describe("SecretBindingDetailPage — delete", () => {
  it("typed-name delete calls DELETE on confirm", async () => {
    const calls = installFetch({ remove: { ok: true } });
    renderDetail("/secrets/default/oai-key?delete=1");
    await screen.findByTestId("secret-detail-page");
    // ConfirmDialog uses placeholder matching the confirmText
    fireEvent.change(screen.getByPlaceholderText("oai-key"), { target: { value: "oai-key" } });
    fireEvent.click(screen.getByRole("button", { name: /Delete binding/i }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === "DELETE" && c.url.includes("/api/secretbindings/default/oai-key"))).toBe(true);
    });
  });
});

// ── Archetype A1 (M151 spec §6.1) ───────────────────────────────────────────
// The redesign's contract for this page: sorted by what is blocking, a Next
// step column that speaks the USER's next action, the §4.4 column budget, four
// distinct degraded states, a closing note whose numbers come from the data —
// and, unchanged and non-negotiable, no secret value anywhere on the surface.

const READY_A = { ...DEFAULT_DETAIL, name: "aaa-ready" };
const READY_Z = { ...DEFAULT_DETAIL, name: "zzz-ready" };
const BROKEN = {
  ...DEFAULT_DETAIL,
  name: "mmm-broken",
  backend: "vault",
  phase: "NotReady",
  ready: false,
};

/** Row identity in DOM order, read off each row's Next step cell test id. */
function rowOrder(): string[] {
  return Array.from(screen.getByRole("table").querySelectorAll("tbody tr"))
    .map(
      (tr) =>
        tr.querySelector("[data-testid^='next-step-']")?.getAttribute("data-testid") ?? "",
    )
    .filter(Boolean)
    .map((id) => id.replace("next-step-", ""));
}

describe("SecretBindingsPage — archetype A1 (M151)", () => {
  it("renders the §4.4 resource-list column budget, in visual order", async () => {
    installFetch({ bindings: () => ({ ok: true, body: { items: [READY_A], nextCursor: "" } }) });
    renderList();
    await screen.findByText("aaa-ready");

    const heads = Array.from(
      screen.getByRole("table").querySelectorAll("thead th"),
    ).map((th) => th.textContent?.trim());
    expect(heads).toEqual([
      "Binding",
      "Backend",
      "K8s Secret / Key",
      "State",
      "Next step",
      "Actions",
    ]);
  });

  it("sorts by what is blocking, not alphabetically", async () => {
    installFetch({
      bindings: () => ({
        ok: true,
        body: { items: [READY_A, READY_Z, BROKEN], nextCursor: "" },
      }),
    });
    renderList();
    await screen.findByText("aaa-ready");

    expect(rowOrder()).toEqual(["mmm-broken", "aaa-ready", "zzz-ready"]);
  });

  it("the Next step column says what the user should do, and 'Nothing needed' when nothing is", async () => {
    installFetch({
      bindings: () => ({
        ok: true,
        body: {
          items: [
            BROKEN,
            READY_A,
            { ...DEFAULT_DETAIL, name: "coming-up", phase: "Pending", ready: false },
          ],
          nextCursor: "",
        },
      }),
    });
    renderList();
    await screen.findByText("aaa-ready");

    expect(screen.getByTestId("next-step-mmm-broken")).toHaveTextContent("Fix the binding");
    // Resolved, and converging-on-its-own, both ask nothing of a person.
    expect(screen.getByTestId("next-step-aaa-ready")).toHaveTextContent("Nothing needed");
    expect(screen.getByTestId("next-step-coming-up")).toHaveTextContent("Nothing needed");

    const label = screen
      .getByTestId("next-step-mmm-broken")
      .textContent!.replace(" →", "")
      .trim();
    expect(label.length).toBeLessThanOrEqual(22); // the §7.2 copy budget
    expect(label).toMatch(/^[A-Z][a-z]+ /); // verb-first
    // Crit only because the target genuinely is a failure (§2.3).
    expect(screen.getByTestId("next-step-mmm-broken").className).toMatch(/text-destructive/);
    expect(screen.getByTestId("next-step-aaa-ready").tagName).toBe("SPAN");
  });

  it("chip views carry counts only when the loaded window is provably the whole set", async () => {
    installFetch({
      bindings: (qs) =>
        qs.get("cursor")
          ? { ok: true, body: { items: [READY_Z], nextCursor: "" } }
          : { ok: true, body: { items: [READY_A, BROKEN], nextCursor: "c1" } },
    });
    renderList();
    await screen.findByText("aaa-ready");

    // Page 1 of more: a count of the rows in hand would look like a total, so
    // the chips carry NO number at all, and the closing line says "on this
    // page" out loud instead of implying it counted the namespace.
    const group = screen.getByRole("radiogroup", { name: "Filter secret bindings" });
    expect(
      within(group)
        .getAllByRole("radio")
        .map((r) => r.textContent),
    ).toEqual(["Needs you", "Resolved", "Everything"]);
    expect(
      await screen.findByText(
        "1 of the 2 bindings on this page needs a person. One of them won’t resolve until it is fixed. The other one needs nothing from you. More pages follow.",
      ),
    ).toBeInTheDocument();
  });

  it("a single complete page IS the whole set, so its chip counts are facts", async () => {
    installFetch({
      bindings: () => ({ ok: true, body: { items: [READY_A, BROKEN], nextCursor: "" } }),
    });
    renderList();
    await screen.findByText("aaa-ready");

    const group = screen.getByRole("radiogroup", { name: "Filter secret bindings" });
    expect(
      within(group)
        .getAllByRole("radio")
        .map((r) => r.textContent),
    ).toEqual(["Needs you1", "Resolved1", "Everything2"]);
    // No page precedes or follows, so nothing is hedged.
    expect(screen.queryByText(/on this page/)).toBeNull();
    expect(screen.queryByText(/More pages follow/)).toBeNull();
  });

  it("an emptied chip view is the filtered state, and offers the way back", async () => {
    installFetch({
      bindings: () => ({ ok: true, body: { items: [READY_A, READY_Z], nextCursor: "" } }),
    });
    renderList();
    await screen.findByText("aaa-ready");

    fireEvent.click(screen.getByRole("radio", { name: /Needs you/ }));
    expect(await screen.findByText("Nothing needs a person")).toBeInTheDocument();
    // NOT the first-run teaching state — bindings exist, the view excluded them.
    expect(screen.queryByText("No secret bindings yet")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Show everything" }));
    expect(await screen.findByText("aaa-ready")).toBeInTheDocument();
  });

  it("says once, calmly, what this response cannot answer (§7.1)", async () => {
    installFetch({ bindings: () => ({ ok: true, body: { items: [READY_A], nextCursor: "" } }) });
    renderList();
    await screen.findByText("aaa-ready");

    const notes = screen.getAllByRole("note");
    expect(notes).toHaveLength(1);
    expect(notes[0]).toHaveTextContent("Usage and rotation aren’t in the binding list.");
    expect(notes[0]).toHaveTextContent("Nothing is estimated");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("the closing note states the ratio from the data, and is grammatical at n=1", async () => {
    installFetch({ bindings: () => ({ ok: true, body: { items: [READY_A], nextCursor: "" } }) });
    const { unmount } = renderList();
    expect(
      await screen.findByText("The one binding needs nothing from you."),
    ).toBeInTheDocument();
    unmount();

    installFetch({
      bindings: () => ({
        ok: true,
        body: { items: [READY_A, READY_Z, BROKEN], nextCursor: "" },
      }),
    });
    renderList();
    expect(
      await screen.findByText(
        "1 of the 3 bindings needs a person. One of them won’t resolve until it is fixed. The other 2 need nothing from you.",
      ),
    ).toBeInTheDocument();
  });

  it("an absent backend renders the dash with a reason, never a default (§7.1)", async () => {
    installFetch({
      bindings: () => ({
        ok: true,
        body: { items: [{ ...READY_A, backend: "" }], nextCursor: "" },
      }),
    });
    renderList();
    await screen.findByText("aaa-ready");

    // Unknown and a real answer never share a glyph.
    const dash = screen.getByTitle(/unknown, not a default/);
    expect(dash).toHaveTextContent("—");
    expect(screen.queryByTestId("backend-aaa-ready")).toBeNull();
  });

  it("a 63-character name truncates on one line; a deep namespace middle-truncates (§4.5)", async () => {
    const LONG_NAME = `sb-${"x".repeat(60)}`; // the K8s 63-character limit
    expect(LONG_NAME).toHaveLength(63);
    const DEEP_NS = "acme-platform-eu-west-1-team-d-shared-ingest";

    installFetch({
      bindings: () => ({
        ok: true,
        body: {
          items: [{ ...DEFAULT_DETAIL, name: LONG_NAME, namespace: DEEP_NS }],
          nextCursor: "",
        },
      }),
    });
    renderList();

    const nameCell = await screen.findByTitle(LONG_NAME);
    expect(nameCell.className).toMatch(/truncate/);
    expect(nameCell.className).not.toMatch(/break-all/);

    const ns = screen.getByTestId(`namespace-${LONG_NAME}`);
    expect(ns).toHaveAttribute("title", DEEP_NS);
    expect(ns).toHaveTextContent("acme…shared-ingest");
    expect(ns.closest("div")!.className).toMatch(/font-mono/);
  });

  it("SECURITY: the redesigned columns render the ref and nothing else", async () => {
    // Even if a backend ever leaked a value onto the DTO, no column, tooltip,
    // or closing sentence on this page has anywhere to put it.
    installFetch({
      bindings: () => ({
        ok: true,
        body: {
          items: [
            {
              ...DEFAULT_DETAIL,
              value: "sk-live-must-never-render",
              data: { apiKey: "sk-live-must-never-render" },
              secretRef: { name: "my-oai-secret", key: "apiKey", value: "sk-live-must-never-render" },
            },
          ],
          nextCursor: "",
        },
      }),
    });
    renderList();
    await screen.findByText("oai-key");

    expect(screen.getByTestId("secret-ref-oai-key")).toHaveTextContent("my-oai-secret/apiKey");
    expect(document.body.textContent).not.toContain("sk-live-must-never-render");
    expect(document.body.innerHTML).not.toContain("sk-live-must-never-render");
    expect(document.querySelector("input[type=password]")).toBeNull();
  });
});


// ── Archetype A2 (M151 spec §6.1) — the detail surface ──────────────────────
// The redesign's contract for the detail page: the §4.7 hub grid, a rail that
// states the bound facts, an edit form whose divergence from the record is
// VISIBLE — and, unchanged and non-negotiable, no secret value anywhere, ever.

/** The secret a correct page can never put on screen — in text, in an
 *  attribute (title / placeholder / value), or in a live input's value. */
function expectNoSecretAnywhere(secret: string) {
  expect(document.body.textContent ?? "").not.toContain(secret);
  for (const el of Array.from(document.querySelectorAll("*"))) {
    for (const attr of Array.from(el.attributes)) {
      expect(`${attr.name}=${attr.value}`).not.toContain(secret);
    }
    const live = el as HTMLInputElement;
    if (typeof live.value === "string") expect(live.value).not.toContain(secret);
  }
}

describe("SecretBindingDetailPage — archetype A2 (M151)", () => {
  // The DTO carries no value field at all; this is the hostile case — if one
  // ever appeared in a payload, the page still may not render it.
  const SECRET_VALUE = "sk-live-MUST-NEVER-RENDER-0000";
  const LEAKY = {
    ...DEFAULT_DETAIL,
    value: SECRET_VALUE,
    data: { apiKey: SECRET_VALUE },
    credential: SECRET_VALUE,
  };

  it("states the value's absence in the rail, in words, where a value would be", async () => {
    installFetch({ detail: { ok: true, body: LEAKY } });
    renderDetail();
    await screen.findByTestId("secret-detail-page");

    // The rail's last fact is the deliberate absence — not a blank row.
    expect(screen.getByText("never read here")).toBeInTheDocument();
    // And the surface says why, once, beside the reference it DOES show.
    expect(screen.getByTestId("no-value-note")).toBeInTheDocument();
    // The reference itself is still fully readable.
    expect(screen.getByTestId("secret-ref-name")).toHaveTextContent("my-oai-secret");
    expect(screen.getByTestId("secret-ref-key")).toHaveTextContent("apiKey");

    expectNoSecretAnywhere(SECRET_VALUE);
  });

  it("renders no secret value in the edit form either", async () => {
    installFetch({ detail: { ok: true, body: LEAKY } });
    renderDetail("/secrets/default/oai-key?edit=1");
    await screen.findByTestId("edit-sb-secret-name");
    expectNoSecretAnywhere(SECRET_VALUE);
  });

  it("renders no secret value after a successful save", async () => {
    installFetch({
      detail: { ok: true, body: LEAKY },
      update: { ok: true, body: LEAKY },
    });
    renderDetail("/secrets/default/oai-key?edit=1");
    await screen.findByTestId("edit-sb-secret-name");
    fireEvent.change(screen.getByTestId("edit-sb-secret-name"), {
      target: { value: "rotated-secret" },
    });
    fireEvent.click(screen.getByRole("button", { name: /continue/i }));
    await screen.findByTestId("secret-edit-review");
    fireEvent.click(screen.getByRole("button", { name: /Save changes/i }));

    // The drawer closes and the page re-reads the binding.
    await waitFor(() =>
      expect(screen.queryByTestId("secret-edit-review")).toBeNull(),
    );
    await screen.findByTestId("secret-detail-page");
    expectNoSecretAnywhere(SECRET_VALUE);
  });

  it("makes an unsaved change visibly unsaved — and un-marks one put back", async () => {
    installFetch({});
    renderDetail("/secrets/default/oai-key?edit=1");
    const field = await screen.findByTestId("edit-sb-secret-name");

    // Nothing diverges yet, so nothing nags.
    expect(screen.queryByTestId("secret-edit-unsaved")).toBeNull();

    fireEvent.change(field, { target: { value: "rotated-secret" } });
    expect(screen.getByTestId("secret-edit-unsaved")).toBeInTheDocument();
    expect(screen.getByText("Changed — not saved yet")).toBeInTheDocument();

    // Edited BACK to what the record says: that is not a change, and a page
    // that still claimed it was would train people to click through the guard.
    fireEvent.change(field, { target: { value: "my-oai-secret" } });
    expect(screen.queryByTestId("secret-edit-unsaved")).toBeNull();
  });

  it("shows the 404 as an honest absence with a way back, not an error", async () => {
    installFetch({ detail: { ok: false, status: 404, body: { error: "not found" } } });
    renderDetail();
    expect(await screen.findByTestId("secret-not-found")).toBeInTheDocument();
    // Calm: a note, never an alert.
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("link", { name: /Back to bindings/ })).toHaveAttribute(
      "href",
      "/secrets",
    );
  });
});
