import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { App } from "@/App";
import { consumeOpenerlessMcpReturn, maybeCloseOAuthPopup } from "@/lib/oauth-popup";
import "@/index.css";

// If this window is an inline-consent popup that just returned from the MCP OAuth
// callback (ADR 0031), message the opener and close BEFORE rendering — so the run in
// the opener resumes in place even if the popup would otherwise bounce to /login.
if (maybeCloseOAuthPopup()) {
  // Popup handled; window.close() is in flight — do not mount the app.
} else {
  // Not a popup, but the SAME-TAB (popup-blocked) MCP redirect may have returned with
  // ?mcp_connected/?mcp_error and no opener (DX-6): stash the outcome + clean the URL BEFORE
  // render, so the mounting page can surface it instead of the connect ending in silence.
  consumeOpenerlessMcpReturn();
  const rootEl = document.getElementById("root");
  if (!rootEl) {
    throw new Error("root element not found");
  }
  createRoot(rootEl).render(
    <StrictMode>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </StrictMode>,
  );
}
