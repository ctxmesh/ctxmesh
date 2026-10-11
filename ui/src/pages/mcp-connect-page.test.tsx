import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, waitFor } from "@testing-library/react";

import { McpConnectPage } from "@/pages/mcp-connect-page";
import { MCP_CONSENT_READY, MCP_CONSENT_TICKET } from "@/lib/oauth-popup";

// /connect (ADR 0040 amendment): the console page a chat on an agent hostname opens to begin MCP
// consent on the console origin. It takes ONE ticket, by message, from window.opener only — never
// from the URL — reports the message's own origin as the opener, and navigates only to an absolute
// http(s) authorization URL.

const agentOrigin = "https://helper.prod.agents.example.com";

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

  beforeEach(() => {
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
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it("announces itself to its opener with no payload", () => {
    stubRedeem({ status: 202, json: {} });
    render(<McpConnectPage />);
    expect(opener!.postMessage).toHaveBeenCalledWith({ type: MCP_CONSENT_READY }, "*");
    expect(screen.getByTestId("mcp-connect-working")).toBeInTheDocument();
  });

  it("redeems a ticket from its opener, reporting that message's origin, then goes to the provider", async () => {
    const calls = stubRedeem({
      status: 202,
      json: { status: "authorization_required", authorizationURL: "https://as.example/authorize?state=s" },
    });
    render(<McpConnectPage />);

    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    await waitFor(() => expect(replace).toHaveBeenCalledWith("https://as.example/authorize?state=s"));

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
    stubRedeem({
      status: 202,
      json: { status: "authorization_required", authorizationURL: "javascript:alert(1)" },
    });
    render(<McpConnectPage />);
    act(() => {
      window.dispatchEvent(fromWindow(opener, { type: MCP_CONSENT_TICKET, ticket: "tkt-1" }));
    });
    expect(await screen.findByTestId("mcp-connect-error")).toHaveTextContent("not valid");
    expect(replace).not.toHaveBeenCalled();
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
