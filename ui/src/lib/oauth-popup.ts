// oauth-popup.ts — the inline MCP-consent popup bridge (ADR 0031, m26.2).
//
// The per-user OAuth consent runs in a POPUP so the run stays on screen and can
// resume in place (the connector pattern). The BFF OAuth callback redirects the
// popup to a normal SPA route carrying ?mcp_connected=<server> (or ?mcp_error=<msg>).
// This module, run ONCE at SPA boot BEFORE React renders, detects that the current
// window is a popup that just completed a connect, messages the opener with the
// outcome, and closes — so the opener (the Playground) can auto-re-invoke the run.
//
// Running pre-render matters: it fires even if the popup would otherwise bounce to
// /login (the popup is a fresh window that may not inherit a per-tab session token).

export const MCP_OAUTH_MESSAGE = "ctxmesh:mcp-oauth";

export interface McpOAuthPopupMessage {
  type: typeof MCP_OAUTH_MESSAGE;
  // The connected server name (empty on error).
  server: string;
  // A callback error message (empty on success).
  error: string;
}

// maybeCloseOAuthPopup: when the current window is a popup that just returned from an
// MCP OAuth callback (?mcp_connected or ?mcp_error present AND window.opener set),
// message the opener with the outcome and close the popup. A no-op otherwise — a
// normal page load, or the full-page register redirect (which has no opener). Returns
// true when it acted (used by tests). Never throws: a cross-origin opener or a
// non-browser env falls through to a normal boot.
export function maybeCloseOAuthPopup(win: Window = window): boolean {
  try {
    const params = new URLSearchParams(win.location.search);
    const server = params.get("mcp_connected");
    const error = params.get("mcp_error");
    const opener = win.opener as Window | null;
    if ((server === null && error === null) || !opener || opener === win) {
      return false;
    }
    const message: McpOAuthPopupMessage = {
      type: MCP_OAUTH_MESSAGE,
      server: server ?? "",
      error: error ?? "",
    };
    opener.postMessage(message, win.location.origin);
    win.close();
    return true;
  } catch {
    return false;
  }
}
