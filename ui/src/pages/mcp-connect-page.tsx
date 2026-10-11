import * as React from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { AuthCard } from "@/pages/login-page";
import { api, type McpConsentRedeemResponse } from "@/lib/api";
import {
  isValidHttpUrl,
  MCP_CONSENT_READY,
  MCP_CONSENT_TICKET,
} from "@/lib/oauth-popup";

// McpConnectPage — /connect on the console origin, public (ADR 0040 amendment).
//
// A chat on an agent's own hostname cannot begin an MCP OAuth consent itself: the
// browser-binding cookie must be set on the console origin, where the one registered
// callback runs. So the chat opens this page in a popup, and this page:
//
//   1. tells its opener it is ready (no payload, so any target origin is safe);
//   2. accepts ONE ticket, by message only, and only from window.opener — never from
//      the URL, because a ticket in a link would let anyone who can send a link bind
//      their own consent to the reader's browser;
//   3. redeems it here, reporting the origin the message came from (which the BFF
//      accepts only as the ticket's agent hostname), so the binding cookie lands in
//      this browser;
//   4. says whose consent this is (the account that pressed Connect, the server, the
//      agent) and waits for the user: the tokens they are about to grant are stored as
//      that account's, so this is the check that holds when a page that is not the
//      agent's chat managed to open this window;
//   5. on Continue, replaces itself with the provider's authorization page, after
//      checking it is an absolute http(s) URL.
//
// The callback then lands in this same window and relays "connected" to the chat.
//
// data-testid contract (a harness spec clicks these; keep the names):
//   mcp-connect-working    — waiting for the ticket, redeeming it, or leaving for the provider
//   mcp-connect-confirm    — the confirmation, holding:
//     connect-summary      — "Connecting <server> for <agent>, as <minter>."
//     connect-continue     — the only way on to the provider
//   mcp-connect-error      — the redeem failed, the URL was not valid, or nothing arrived
//   mcp-connect-no-opener  — opened directly, not from a chat

/** How long to wait for the chat's ticket before saying nothing arrived. */
const TICKET_WAIT_MS = 20_000;

/**
 * How long Continue stays disabled after the confirmation appears. The window that opened
 * this one chose where it sits on screen, so it could put Continue under the pointer just as
 * it asks for a click of its own; a click that arrives this early was not aimed here.
 */
const CONTINUE_ARM_MS = 500;

const NOTHING_ARRIVED =
  "Nothing arrived from the chat that opened this window. Close it and press Connect in the chat again.";

const BAD_SIGN_IN_URL =
  "The connection could not start: the sign-in address it was given is not valid.";

type Phase =
  | { kind: "waiting" }
  | { kind: "redeeming" }
  | { kind: "confirm"; consent: McpConsentRedeemResponse }
  | { kind: "redirecting" }
  | { kind: "failed"; message: string }
  | { kind: "no-opener" };

function openerOf(win: Window): Window | null {
  const opener = win.opener as Window | null;
  return opener && opener !== win ? opener : null;
}

export function McpConnectPage() {
  const [phase, setPhase] = React.useState<Phase>(() =>
    openerOf(window) ? { kind: "waiting" } : { kind: "no-opener" },
  );

  React.useEffect(() => {
    const opener = openerOf(window);
    if (!opener) return;
    let alive = true;
    let received = false;

    async function redeem(ticket: string, openerOrigin: string) {
      setPhase({ kind: "redeeming" });
      try {
        const res = await api.redeemMcpConsentTicket({ ticket, openerOrigin });
        if (!alive) return;
        if (!isValidHttpUrl(res.authorizationURL)) {
          setPhase({ kind: "failed", message: BAD_SIGN_IN_URL });
          return;
        }
        if (!res.minter || !res.server || !res.agent) {
          // Without a name to show, the user cannot tell whose account this connects.
          setPhase({
            kind: "failed",
            message: "This connection did not say whose account it is for, so it was not started. Close this window and press Connect in the chat again.",
          });
          return;
        }
        setPhase({ kind: "confirm", consent: res });
      } catch (err) {
        if (!alive) return;
        setPhase({
          kind: "failed",
          message: err instanceof Error ? err.message : "The connection could not start.",
        });
      }
    }

    function onMessage(e: MessageEvent) {
      if (received || e.source !== opener || !e.origin || e.origin === "null") return;
      const data = e.data as { type?: unknown; ticket?: unknown } | null;
      if (data?.type !== MCP_CONSENT_TICKET || typeof data.ticket !== "string" || !data.ticket) {
        return;
      }
      received = true;
      window.removeEventListener("message", onMessage);
      window.clearTimeout(timer);
      void redeem(data.ticket, e.origin);
    }

    const timer = window.setTimeout(() => {
      if (!received && alive) setPhase({ kind: "failed", message: NOTHING_ARRIVED });
    }, TICKET_WAIT_MS);
    window.addEventListener("message", onMessage);
    try {
      opener.postMessage({ type: MCP_CONSENT_READY }, "*");
    } catch {
      // The opener closed or navigated away; the wait above times out with an honest message.
    }
    return () => {
      alive = false;
      window.removeEventListener("message", onMessage);
      window.clearTimeout(timer);
    };
  }, []);

  const closeWindow = (
    <Button variant="outline" className="h-11 w-full" onClick={() => window.close()}>
      Close this window
    </Button>
  );

  const confirmed = phase.kind === "confirm" ? phase.consent : null;
  const [armedFor, setArmedFor] = React.useState<McpConsentRedeemResponse | null>(null);
  React.useEffect(() => {
    if (!confirmed) return;
    const t = window.setTimeout(() => setArmedFor(confirmed), CONTINUE_ARM_MS);
    return () => window.clearTimeout(t);
  }, [confirmed]);

  function onContinue(consent: McpConsentRedeemResponse) {
    if (!isValidHttpUrl(consent.authorizationURL)) {
      setPhase({ kind: "failed", message: BAD_SIGN_IN_URL });
      return;
    }
    setPhase({ kind: "redirecting" });
    window.location.replace(consent.authorizationURL);
  }

  if (phase.kind === "no-opener") {
    return (
      <AuthCard>
        <div data-testid="mcp-connect-no-opener">
          <h1 className="font-serif text-2xl font-medium tracking-snug">Nothing to connect here</h1>
          <p className="mt-1.5 text-md text-secondary-foreground">
            This page finishes a connection started from an agent&rsquo;s chat, and opens
            when you press Connect there. To connect an account without a chat, open the
            console and connect the server from there.
          </p>
          <div className="mt-6">
            <Button asChild className="h-11 w-full">
              <a href="/">Open the console</a>
            </Button>
          </div>
        </div>
      </AuthCard>
    );
  }

  if (phase.kind === "failed") {
    return (
      <AuthCard>
        <div data-testid="mcp-connect-error">
          <h1 className="font-serif text-2xl font-medium tracking-snug">
            The connection didn&rsquo;t start
          </h1>
          <div role="alert" className="mt-4 border-l-2 border-l-destructive pl-4">
            <p className="text-md text-secondary-foreground">{phase.message}</p>
          </div>
          <div className="mt-6">{closeWindow}</div>
        </div>
      </AuthCard>
    );
  }

  if (phase.kind === "confirm") {
    const { consent } = phase;
    return (
      <AuthCard>
        <div data-testid="mcp-connect-confirm">
          <h1 className="font-serif text-2xl font-medium tracking-snug">
            Connect {consent.server}?
          </h1>
          <p data-testid="connect-summary" className="mt-1.5 text-md text-secondary-foreground">
            Connecting <span className="font-medium text-foreground">{consent.server}</span> for{" "}
            <span className="font-medium text-foreground">{consent.agent}</span>, as{" "}
            <span className="font-medium text-foreground">{consent.minter}</span>. Continue only
            if that is you.
          </p>
          <p className="mt-3 text-md text-secondary-foreground">
            The account you sign in to next will be stored for {consent.minter}, and{" "}
            {consent.agent} will act with it. If you did not just press Connect in that
            agent&rsquo;s chat, close this window.
          </p>
          <div className="mt-6 flex flex-col gap-2">
            {/* Never autofocused: a key press meant for the window that opened this one must not continue. */}
            <Button
              data-testid="connect-continue"
              className="h-11 w-full"
              disabled={armedFor !== consent}
              onClick={() => onContinue(consent)}
            >
              Continue to sign in
            </Button>
            {closeWindow}
          </div>
        </div>
      </AuthCard>
    );
  }

  return (
    <AuthCard>
      <div data-testid="mcp-connect-working">
        <Badge variant="progressing">Working</Badge>
        <h1 className="mt-3 font-serif text-2xl font-medium tracking-snug">
          Connecting your account
        </h1>
        <p role="status" className="mt-1.5 text-md text-secondary-foreground">
          {phase.kind === "redirecting"
            ? "Opening your provider's sign-in page."
            : phase.kind === "redeeming"
              ? "Checking the connection with the console."
              : "Picking up the connection from your chat. This takes a moment."}
        </p>
        <div className="mt-6">{closeWindow}</div>
      </div>
    </AuthCard>
  );
}
