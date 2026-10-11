import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";

import { McpConnectPage } from "@/pages/mcp-connect-page";
import { MCP_CONSENT_READY, MCP_CONSENT_TICKET } from "@/lib/oauth-popup";

// /connect (ADR 0040 amendment): the console page a chat on an agent hostname opens to begin MCP
// consent on the console origin. It takes ONE ticket, by message, from window.opener only — never
// from the URL — reports the message's own origin as the opener, shows whose consent it is, and
// navigates only after the user continues, and only to an absolute http(s) authorization URL.

const agentOrigin = "https://helper.prod.agents.example.com";

const redeemed = {
  status: "authorization_required",
  authorizationURL: "https://as.example/authorize?state=s",
  minter: "alice@example.com",
  server: "github-mcp",
  agent: "helper",
  namespace: "prod",
};

interface Call {
  url: string;
  method: string;
  body: string;
  headers: Headers;
}

function stubRedeem(response: { status: number; json: unknown }) {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string | URL, init?: RequestInit) => {
      calls.push({
        url: typeof input === "string" ? input : input.toString(),
        method: init?.method ?? "GET",
        body: typeof init?.body === "string" ? init.body : "",
        headers: new Headers(init?.headers),
      });
      return Promise.resolve({
        ok: response.status < 400,
        status: response.status,
        json: async () => response.json,
        text: async () => JSON.stringify(response.json),
      } as Response);
    }),
  );
  return calls;
}

function fromWindow(source: unknown, data: unknown, origin = agentOrigin): MessageEvent {
  const e = new MessageEvent("message", { data, origin });
  Object.defineProperty(e, "source", { value: source });
  return e;
}

describe("McpConnectPage", () => {
  const originalLocation = window.location;
  let replace: ReturnType<typeof vi.fn>;
  let opener: { postMessage: ReturnType<typeof vi.fn> } | null;
  // Whether this window has focus and is visible; jsdom has neither notion of its own.
  let focused: boolean;
  let visibility: DocumentVisibilityState;

  beforeEach(() => {
    focused = true;
    visibility = "visible";
    vi.spyOn(document, "hasFocus").mockImplementation(() => focused);
    Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility });
    replace = vi.fn();
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, origin: "https://console.example.com", replace },
    });
    opener = { postMessage: vi.fn() };
    Object.defineProperty(window, "opener", { configurable: true, writable: true, value: opener });
  });
  afterEach(() => {
    Object.defineProperty(window, "location", { configurable: true, value: originalLocation });
    Object.defineProperty(window, "opener", { configurable: true, writable: true, value: null });
    delete (document as { visibilityState?: unknown }).visibilityState;
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  async function confirmation() {
    stubRedeem({ status: 202, json: redeemed });
    render(<McpConnectPage />);
    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    return screen.findByTestId("connect-continue");
  }

  const pause = (ms: number) => act(() => new Promise<void>((r) => setTimeout(r, ms)));

  it("announces itself to its opener with no payload", () => {
    stubRedeem({ status: 202, json: {} });
    render(<McpConnectPage />);
    expect(opener!.postMessage).toHaveBeenCalledWith({ type: MCP_CONSENT_READY }, "*");
    expect(screen.getByTestId("mcp-connect-working")).toBeInTheDocument();
  });

  it("redeems a ticket from its opener, reporting that message's origin, and says whose consent it is", async () => {
    const calls = stubRedeem({ status: 202, json: redeemed });
    render(<McpConnectPage />);

    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    expect(await screen.findByTestId("connect-summary")).toHaveTextContent(
      "Connecting github-mcp for helper, as alice@example.com. Continue only if that is you.",
    );

    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe("/api/mcp/oauth/grant/redeem");
    expect(calls[0].method).toBe("POST");
    expect(JSON.parse(calls[0].body)).toEqual({ ticket: "tkt-1", openerOrigin: agentOrigin });
    expect(calls[0].headers.get("Authorization")).toBeNull();

    // One ticket per page: a second message is not redeemed.
    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-2" }));
    });
    expect(calls).toHaveLength(1);
  });

  it("goes to the provider only when the user continues", async () => {
    stubRedeem({ status: 202, json: redeemed });
    render(<McpConnectPage />);
    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    const cont = await screen.findByTestId("connect-continue");

    // Shown is not consented: nothing navigates on its own, and an early click is not taken.
    expect(cont).toBeDisabled();
    expect(cont).not.toHaveFocus();
    fireEvent.click(cont);
    await new Promise((r) => setTimeout(r, 50));
    expect(replace).not.toHaveBeenCalled();

    await waitFor(() => expect(cont).toBeEnabled());
    expect(replace).not.toHaveBeenCalled();
    fireEvent.click(cont);
    expect(replace).toHaveBeenCalledTimes(1);
    expect(replace).toHaveBeenCalledWith("https://as.example/authorize?state=s");
    expect(screen.getByTestId("mcp-connect-working")).toHaveTextContent("Opening your provider");
  });

  it("disarms Continue while the window is blurred or hidden, and re-arms only after it is back", async () => {
    const cont = await confirmation();
    await waitFor(() => expect(cont).toBeEnabled());

    // Covered by another window: disarmed at once, and a click then is not taken.
    focused = false;
    act(() => {
      window.dispatchEvent(new Event("blur"));
    });
    expect(cont).toBeDisabled();
    fireEvent.click(cont);
    expect(replace).not.toHaveBeenCalled();

    // Revealed again: still disarmed until it has been attended for a while.
    focused = true;
    act(() => {
      window.dispatchEvent(new Event("focus"));
    });
    expect(cont).toBeDisabled();
    await waitFor(() => expect(cont).toBeEnabled());

    // Hidden (another tab, minimised): the same.
    visibility = "hidden";
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(cont).toBeDisabled();
    visibility = "visible";
    act(() => {
      document.dispatchEvent(new Event("visibilitychange"));
    });
    expect(cont).toBeDisabled();
    await waitFor(() => expect(cont).toBeEnabled());

    fireEvent.click(cont);
    expect(replace).toHaveBeenCalledWith("https://as.example/authorize?state=s");
  });

  it("does not arm Continue in a window that was already covered when it rendered", async () => {
    focused = false;
    const cont = await confirmation();
    await pause(800);
    expect(cont).toBeDisabled();

    focused = true;
    act(() => {
      window.dispatchEvent(new Event("focus"));
    });
    expect(cont).toBeDisabled();
    await waitFor(() => expect(cont).toBeEnabled());
  });

  it("refuses a consent that does not say whose it is", async () => {
    stubRedeem({ status: 202, json: { ...redeemed, minter: "" } });
    render(<McpConnectPage />);
    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    expect(await screen.findByTestId("mcp-connect-error")).toHaveTextContent("whose account");
    expect(screen.queryByTestId("connect-continue")).toBeNull();
    expect(replace).not.toHaveBeenCalled();
  });

  it("ignores a ticket in its URL", async () => {
    const calls = stubRedeem({ status: 202, json: {} });
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...originalLocation, search: "?ticket=from-a-link", href: "https://console.example.com/connect?ticket=from-a-link", replace },
    });
    render(<McpConnectPage />);
    await new Promise((r) => setTimeout(r, 20));
    expect(calls).toHaveLength(0);
    expect(replace).not.toHaveBeenCalled();
  });

  it("ignores a ticket from any window but its opener, and from an opaque origin", async () => {
    const calls = stubRedeem({ status: 202, json: {} });
    render(<McpConnectPage />);

    act(() => {
      window.dispatchEvent(fromWindow({ postMessage: vi.fn() }, { type: MCP_CONSENT_TICKET, ticket: "tkt-x" }));
      window.dispatchEvent(fromWindow(window, { type: MCP_CONSENT_TICKET, ticket: "tkt-x" }));
      window.dispatchEvent(fromWindow(null, { type: MCP_CONSENT_TICKET, ticket: "tkt-x" }));
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-x" }, "null"));
      window.dispatchEvent(fromWindow(opener, { type: "something-else", ticket: "tkt-x" }));
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: 42 }));
    });
    await new Promise((r) => setTimeout(r, 20));
    expect(calls).toHaveLength(0);
    expect(screen.getByTestId("mcp-connect-working")).toBeInTheDocument();
  });

  it("navigates only to an absolute http(s) authorization URL", async () => {
    for (const authorizationURL of ["javascript:alert(1)", "data:text/html,x", "/relative"]) {
      stubRedeem({ status: 202, json: { ...redeemed, authorizationURL } });
      const { unmount } = render(<McpConnectPage />);
      act(() => {
        window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
      });
      expect(await screen.findByTestId("mcp-connect-error")).toHaveTextContent("not valid");
      expect(screen.queryByTestId("connect-continue")).toBeNull();
      expect(replace).not.toHaveBeenCalled();
      unmount();
    }
  });

  it("shows the BFF's refusal and does not navigate", async () => {
    stubRedeem({ status: 403, json: { error: "this connect request did not come from the chat of the agent it was started for" } });
    render(<McpConnectPage />);
    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    expect(await screen.findByTestId("mcp-connect-error")).toHaveTextContent("did not come from the chat");
    expect(replace).not.toHaveBeenCalled();
  });

  it("says so when nothing arrives", async () => {
    vi.useFakeTimers();
    try {
      stubRedeem({ status: 202, json: {} });
      render(<McpConnectPage />);
      act(() => {
        vi.advanceTimersByTime(20_000);
      });
      expect(screen.getByTestId("mcp-connect-error")).toHaveTextContent("Nothing arrived");
    } finally {
      vi.useRealTimers();
    }
  });

  it("without an opener, explains itself and points to the console", () => {
    Object.defineProperty(window, "opener", { configurable: true, writable: true, value: null });
    const calls = stubRedeem({ status: 202, json: {} });
    render(<McpConnectPage />);
    expect(screen.getByTestId("mcp-connect-no-opener")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open the console" })).toHaveAttribute("href", "/");
    expect(calls).toHaveLength(0);
  });
});
