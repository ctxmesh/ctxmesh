import { describe, expect, it, vi } from "vitest";

import {
  consumeOpenerlessMcpReturn,
  isValidHttpUrl,
  MCP_OAUTH_MESSAGE,
  maybeCloseOAuthPopup,
  readMcpOAuthReturn,
} from "@/lib/oauth-popup";

// A minimal fake popup window: a fixed origin + search, an opener carrying a
// postMessage spy (the bridge messages the OPENER, not itself), and a close spy.
function fakeSetup(search: string, withOpener: boolean) {
  const openerPost = vi.fn();
  const close = vi.fn();
  const win = {
    location: { search, origin: "https://console.example" },
    opener: withOpener ? { postMessage: openerPost } : null,
    close,
  } as unknown as Window;
  return { win, openerPost, close };
}

describe("maybeCloseOAuthPopup", () => {
  it("messages the opener with the connected server and closes, when a popup returns from the callback", () => {
    const { win, openerPost, close } = fakeSetup("?mcp_connected=scalekit-mcp-server", true);

    expect(maybeCloseOAuthPopup(win)).toBe(true);
    expect(openerPost).toHaveBeenCalledWith(
      { type: MCP_OAUTH_MESSAGE, server: "scalekit-mcp-server", error: "" },
      "https://console.example",
    );
    expect(close).toHaveBeenCalledOnce();
  });

  it("relays a callback error too", () => {
    const { win, openerPost } = fakeSetup("?mcp_error=denied", true);
    expect(maybeCloseOAuthPopup(win)).toBe(true);
    expect(openerPost).toHaveBeenCalledWith(
      { type: MCP_OAUTH_MESSAGE, server: "", error: "denied" },
      "https://console.example",
    );
  });

  it("is a no-op with no opener (the full-page register redirect, not a popup)", () => {
    const { win, close } = fakeSetup("?mcp_connected=some-server", false);
    expect(maybeCloseOAuthPopup(win)).toBe(false);
    expect(close).not.toHaveBeenCalled();
  });

  it("is a no-op on a normal page load (no callback query), even inside a popup", () => {
    const { win, openerPost, close } = fakeSetup("?foo=bar", true);
    expect(maybeCloseOAuthPopup(win)).toBe(false);
    expect(openerPost).not.toHaveBeenCalled();
    expect(close).not.toHaveBeenCalled();
  });

  it("posts to opener_origin cross-origin when the callback carries one (ADR 0040)", () => {
    // The callback runs at the console origin (win.location.origin) but the opener is an agent
    // hostname; the bridge must target the opener's origin, not the popup's own.
    const { win, openerPost } = fakeSetup(
      "?mcp_connected=scalekit-mcp-server&opener_origin=https://scalekit-agent.default.example",
      true,
    );
    expect(maybeCloseOAuthPopup(win)).toBe(true);
    expect(openerPost).toHaveBeenCalledWith(
      { type: MCP_OAUTH_MESSAGE, server: "scalekit-mcp-server", error: "" },
      "https://scalekit-agent.default.example",
    );
  });
});

// DX-6: a same-tab (popup-blocked) redirect must (a) only navigate to a safe URL, and (b) not
// dead-end in silence when it returns with ?mcp_connected and no opener.
describe("isValidHttpUrl (DX-6 same-tab redirect guard)", () => {
  it("accepts absolute http(s) URLs", () => {
    expect(isValidHttpUrl("https://as.example/authorize?x=1")).toBe(true);
    expect(isValidHttpUrl("http://localhost:9090/authorize")).toBe(true);
  });
  it("rejects empty, relative, and dangerous-scheme values", () => {
    expect(isValidHttpUrl("")).toBe(false);
    expect(isValidHttpUrl(undefined)).toBe(false);
    expect(isValidHttpUrl("/relative/path")).toBe(false);
    expect(isValidHttpUrl("javascript:alert(1)")).toBe(false);
    expect(isValidHttpUrl("data:text/html,<script>1</script>")).toBe(false);
  });
});

// A fake window with a sessionStorage + a replaceState spy, for the same-tab return path.
function fakeReturnWindow(search: string, withOpener: boolean) {
  const store = new Map<string, string>();
  const replaceState = vi.fn();
  const win = {
    location: { search, origin: "https://console.example", pathname: "/playground", hash: "" },
    opener: withOpener ? ({} as Window) : null,
    history: { replaceState },
    sessionStorage: {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => void store.set(k, v),
      removeItem: (k: string) => void store.delete(k),
    },
  } as unknown as Window;
  return { win, replaceState, store };
}

describe("consumeOpenerlessMcpReturn + readMcpOAuthReturn (DX-6)", () => {
  it("stashes the outcome + strips the MCP params from the URL when there is no opener", () => {
    const { win, replaceState } = fakeReturnWindow(
      "?mcp_connected=scalekit-mcp-server&state=abc&keep=1",
      false,
    );
    expect(consumeOpenerlessMcpReturn(win)).toBe(true);
    // URL cleaned: MCP params gone, unrelated params kept.
    expect(replaceState).toHaveBeenCalledWith(null, "", "/playground?keep=1");
    // The mounting page reads (and clears) the stashed outcome.
    expect(readMcpOAuthReturn(win)).toEqual({
      type: MCP_OAUTH_MESSAGE,
      server: "scalekit-mcp-server",
      error: "",
    });
    // One-shot: a second read is empty.
    expect(readMcpOAuthReturn(win)).toBeNull();
  });

  it("relays a same-tab error too", () => {
    const { win } = fakeReturnWindow("?mcp_error=denied", false);
    expect(consumeOpenerlessMcpReturn(win)).toBe(true);
    expect(readMcpOAuthReturn(win)).toEqual({
      type: MCP_OAUTH_MESSAGE,
      server: "",
      error: "denied",
    });
  });

  it("is a no-op for the popup case (opener set — maybeCloseOAuthPopup handles it)", () => {
    const { win, replaceState } = fakeReturnWindow("?mcp_connected=srv", true);
    expect(consumeOpenerlessMcpReturn(win)).toBe(false);
    expect(replaceState).not.toHaveBeenCalled();
  });

  it("is a no-op on a normal page load (no MCP params)", () => {
    const { win } = fakeReturnWindow("?foo=bar", false);
    expect(consumeOpenerlessMcpReturn(win)).toBe(false);
    expect(readMcpOAuthReturn(win)).toBeNull();
  });
});
