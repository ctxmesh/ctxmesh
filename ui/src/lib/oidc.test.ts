import { afterEach, describe, expect, it, vi } from "vitest";

import {
  buildAuthorizeUrl,
  completeLogin,
  pkceChallenge,
  randomUrlSafe,
  startEndUserLogin,
} from "@/lib/oidc";
import { api } from "@/lib/api";

// OIDC Auth-Code + PKCE (ADR 0020). These pin the security-load-bearing pieces: the
// S256 challenge (a known-answer vector), the authorize-URL shape, and the callback
// exchange incl. the anti-CSRF state check and the one-shot flow.

const FLOW_KEY = "agent-engine.oidc.flow";

afterEach(() => {
  sessionStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("PKCE", () => {
  it("computes the S256 challenge (RFC 7636 test vector)", async () => {
    // The canonical vector from RFC 7636 Appendix B.
    const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
    const challenge = await pkceChallenge(verifier);
    expect(challenge).toBe("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM");
  });

  it("randomUrlSafe is URL-safe and non-repeating", () => {
    const a = randomUrlSafe(32);
    const b = randomUrlSafe(32);
    expect(a).not.toBe(b);
    expect(a).toMatch(/^[A-Za-z0-9_-]+$/); // base64url: no +, /, or =
  });
});

describe("buildAuthorizeUrl", () => {
  it("assembles an Auth-Code + PKCE (S256) authorize URL", () => {
    const url = new URL(
      buildAuthorizeUrl("https://dex.example.com/auth", {
        clientId: "agent-engine-console",
        redirectUri: "https://console.example.com/auth/callback",
        state: "st4te",
        challenge: "chal1234",
      }),
    );
    expect(url.searchParams.get("response_type")).toBe("code");
    expect(url.searchParams.get("client_id")).toBe("agent-engine-console");
    expect(url.searchParams.get("redirect_uri")).toBe(
      "https://console.example.com/auth/callback",
    );
    expect(url.searchParams.get("code_challenge")).toBe("chal1234");
    expect(url.searchParams.get("code_challenge_method")).toBe("S256");
    expect(url.searchParams.get("state")).toBe("st4te");
    expect(url.searchParams.get("scope")).toContain("openid");
  });

  it("honors an explicit scope (end-user flow: no groups)", () => {
    const url = new URL(
      buildAuthorizeUrl("https://dex-eu.example.com/auth", {
        clientId: "agent-engine-enduser",
        redirectUri: "https://chatbot.ns1.example.com/auth/callback",
        state: "s",
        challenge: "c",
        scope: "openid email offline_access",
      }),
    );
    expect(url.searchParams.get("scope")).toBe("openid email offline_access");
    expect(url.searchParams.get("scope")).not.toContain("groups");
  });
});

describe("startEndUserLogin (M137/EU1b)", () => {
  it("throws when the agent's tenant has no end-user IdP (config null)", async () => {
    vi.spyOn(api, "endUserAuthConfig").mockResolvedValue(null);
    await expect(startEndUserLogin("/chat/ns1/chatbot")).rejects.toThrow(
      /no end-user identity provider/i,
    );
  });
});

describe("completeLogin", () => {
  function seedFlow(over: Record<string, unknown> = {}) {
    sessionStorage.setItem(
      FLOW_KEY,
      JSON.stringify({
        state: "st4te",
        codeVerifier: "the-verifier",
        tokenEndpoint: "https://dex.example.com/token",
        clientId: "agent-engine-console",
        redirectUri: "https://console.example.com/auth/callback",
        returnTo: "/agents",
        ...over,
      }),
    );
  }

  it("exchanges the code for an id_token and clears the one-shot flow", async () => {
    seedFlow();
    // Typed params so mock.calls[0] is inferred as [url, init] (not an empty tuple).
    const fetchMock = vi.fn(
      async (_url: string | URL, _init?: RequestInit) =>
        ({
          ok: true,
          status: 200,
          json: async () => ({ id_token: "ID.TOK.EN" }),
        }) as Response,
    );
    vi.stubGlobal("fetch", fetchMock);

    const res = await completeLogin(
      new URLSearchParams("code=abc&state=st4te"),
    );
    expect(res.idToken).toBe("ID.TOK.EN");
    expect(res.returnTo).toBe("/agents");
    // One-shot: the pending flow is consumed so a replay can't reuse it.
    expect(sessionStorage.getItem(FLOW_KEY)).toBeNull();
    // Exchanged at the token endpoint with the PKCE verifier + auth-code grant.
    const [u, init] = fetchMock.mock.calls[0];
    expect(String(u)).toBe("https://dex.example.com/token");
    expect(String(init?.body)).toContain("code_verifier=the-verifier");
    expect(String(init?.body)).toContain("grant_type=authorization_code");
  });

  it("rejects a state mismatch (anti-CSRF) without exchanging", async () => {
    seedFlow();
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    await expect(
      completeLogin(new URLSearchParams("code=abc&state=WRONG")),
    ).rejects.toThrow(/state mismatch/i);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("surfaces a provider error param", async () => {
    seedFlow();
    await expect(
      completeLogin(
        new URLSearchParams("error=access_denied&error_description=nope"),
      ),
    ).rejects.toThrow(/nope/);
  });

  it("throws when there is no pending flow (nothing to replay)", async () => {
    await expect(
      completeLogin(new URLSearchParams("code=abc&state=st4te")),
    ).rejects.toThrow(/no pending login/i);
  });
});
