import { describe, expect, it, vi } from "vitest";

import { MCP_OAUTH_MESSAGE, maybeCloseOAuthPopup } from "@/lib/oauth-popup";

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
