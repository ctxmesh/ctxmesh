import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { App } from "@/App";
import { maybeCloseOAuthPopup } from "@/lib/oauth-popup";
import "@/index.css";

// If this window is an inline-consent popup that just returned from the MCP OAuth
// callback (ADR 0031), message the opener and close BEFORE rendering — so the run in
// the opener resumes in place even if the popup would otherwise bounce to /login.
if (maybeCloseOAuthPopup()) {
  // Popup handled; window.close() is in flight — do not mount the app.
} else {
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
