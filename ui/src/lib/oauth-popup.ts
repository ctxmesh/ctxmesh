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
//
// Cross-origin (ADR 0040): consent from a chatbox at an agent hostname runs its callback
// at the CANONICAL console origin, so the popup ends up cross-origin from its opener. The
// BFF carries the server-validated opener origin back as ?opener_origin, and we post to
// THAT origin (not the popup's own) so the message reaches the agent-origin opener. Absent
// ⇒ same-origin (post to the popup's own origin — the single-origin console default).
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
    const target = params.get("opener_origin")?.trim() || win.location.origin;
    opener.postMessage(message, target);
    win.close();
    return true;
  } catch {
    return false;
  }
}

// isValidHttpUrl guards a SAME-TAB OAuth redirect (DX-6): when a popup is blocked the connect
// falls back to `window.location.href = authorizationURL`, and an unvalidated value could be a
// relative path or a `javascript:`/`data:` URL that hijacks the tab. Only an absolute http(s)
// URL is a safe navigation target. (Mirrors the guard add-mcp-page.tsx already had.)
export function isValidHttpUrl(u: string | undefined | null): boolean {
  if (!u) return false;
  try {
    const parsed = new URL(u);
    return parsed.protocol === "http:" || parsed.protocol === "https:";
  } catch {
    return false;
  }
}

// The sessionStorage key a same-tab (opener-less) MCP OAuth return is stashed under so the page
// that mounts AFTER the redirect can surface the outcome (the popup path uses postMessage).
const MCP_RETURN_KEY = "ctxmesh:mcp-oauth-return";

// consumeOpenerlessMcpReturn: at boot, when the SPA loads with ?mcp_connected/?mcp_error and there
// is NO opener — a popup-blocked SAME-TAB redirect returned, not the popup case — the param would
// otherwise sit unconsumed and the UI end in SILENCE (DX-6: consent succeeded server-side but the
// user sees nothing and lost their run/chat state). Stash the outcome for the mounting page and
// strip the MCP params from the URL so a reload/bookmark can't replay it. Returns true when it
// acted. The popup case (opener set) is left to maybeCloseOAuthPopup.
export function consumeOpenerlessMcpReturn(win: Window = window): boolean {
  try {
    const params = new URLSearchParams(win.location.search);
    const server = params.get("mcp_connected");
    const error = params.get("mcp_error");
    const opener = win.opener as Window | null;
    if ((server === null && error === null) || (opener && opener !== win)) {
      return false; // no MCP return, or the popup case (handled by maybeCloseOAuthPopup)
    }
    win.sessionStorage?.setItem(
      MCP_RETURN_KEY,
      JSON.stringify({ server: server ?? "", error: error ?? "" }),
    );
    for (const k of ["mcp_connected", "mcp_error", "opener_origin", "state"]) params.delete(k);
    const qs = params.toString();
    win.history.replaceState(
      null,
      "",
      win.location.pathname + (qs ? `?${qs}` : "") + win.location.hash,
    );
    return true;
  } catch {
    return false;
  }
}

// readMcpOAuthReturn: one-shot read (clears it) of a stashed same-tab MCP OAuth outcome, so the
// mounting page (Playground / chat) can show "Connected <server> — run again" or the error, closing
// the DX-6 silent dead-end. Returns null when there is nothing stashed.
export function readMcpOAuthReturn(win: Window = window): McpOAuthPopupMessage | null {
  try {
    const raw = win.sessionStorage?.getItem(MCP_RETURN_KEY);
    if (!raw) return null;
    win.sessionStorage.removeItem(MCP_RETURN_KEY);
    const parsed = JSON.parse(raw) as { server?: string; error?: string };
    return { type: MCP_OAUTH_MESSAGE, server: parsed.server ?? "", error: parsed.error ?? "" };
  } catch {
    return null;
  }
}
