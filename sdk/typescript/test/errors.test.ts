/**
 * The typed error hierarchy + instanceof, parity with `sdk/python/src/ctxmesh/errors.py`.
 * The critical property: every subclass is `instanceof` its base(s) and carries its
 * structured fields (status/body, key/summary, server, detector/scanPoint).
 */

import { describe, it, expect } from "vitest";

import {
  CtxmeshError,
  ConfigError,
  NotInPodError,
  EndpointError,
  ConsentRequiredError,
  GuardrailBlockedError,
  ApprovalRequiredError,
} from "../src/errors.js";

describe("the error hierarchy", () => {
  it("CtxmeshError is the base Error", () => {
    const e = new CtxmeshError("boom");
    expect(e).toBeInstanceOf(Error);
    expect(e).toBeInstanceOf(CtxmeshError);
    expect(e.name).toBe("CtxmeshError");
    expect(e.message).toBe("boom");
  });

  it("ConfigError and NotInPodError nest correctly", () => {
    const cfg = new ConfigError("bad");
    expect(cfg).toBeInstanceOf(CtxmeshError);
    expect(cfg).toBeInstanceOf(ConfigError);

    const nip = new NotInPodError("off plane");
    expect(nip).toBeInstanceOf(CtxmeshError);
    expect(nip).toBeInstanceOf(ConfigError);
    expect(nip).toBeInstanceOf(NotInPodError);
    expect(nip.name).toBe("NotInPodError");
  });

  it("EndpointError carries status + body", () => {
    const e = new EndpointError("upstream 502", { status: 502, body: "boom" });
    expect(e).toBeInstanceOf(CtxmeshError);
    expect(e).toBeInstanceOf(EndpointError);
    expect(e.status).toBe(502);
    expect(e.body).toBe("boom");

    // status/body are optional (transport-level failures have no HTTP response).
    const transport = new EndpointError("connection refused");
    expect(transport.status).toBeUndefined();
    expect(transport.body).toBeUndefined();
  });

  it("ConsentRequiredError extends EndpointError and carries server", () => {
    const e = new ConsentRequiredError("connect your account", {
      server: "github",
      status: 428,
      body: "consent_required",
    });
    expect(e).toBeInstanceOf(EndpointError);
    expect(e).toBeInstanceOf(ConsentRequiredError);
    expect(e.server).toBe("github");
    expect(e.status).toBe(428);
  });

  it("GuardrailBlockedError defaults to 403 and carries detector + scanPoint", () => {
    const e = new GuardrailBlockedError("blocked on policy", {
      detector: "pii",
      scanPoint: "output",
    });
    expect(e).toBeInstanceOf(EndpointError);
    expect(e).toBeInstanceOf(GuardrailBlockedError);
    expect(e.status).toBe(403);
    expect(e.detector).toBe("pii");
    expect(e.scanPoint).toBe("output");
  });

  it("ApprovalRequiredError is a CtxmeshError (not an EndpointError) with key + summary", () => {
    const e = new ApprovalRequiredError("needs approval", {
      key: "refund-approval",
      summary: "Refund $500 to customer",
    });
    expect(e).toBeInstanceOf(CtxmeshError);
    expect(e).toBeInstanceOf(ApprovalRequiredError);
    expect(e).not.toBeInstanceOf(EndpointError);
    expect(e.key).toBe("refund-approval");
    expect(e.summary).toBe("Refund $500 to customer");
  });

  it("subclass errors survive a catch as their concrete type (prototype restored)", () => {
    try {
      throw new NotInPodError("x");
    } catch (err) {
      expect(err).toBeInstanceOf(NotInPodError);
      expect(err).toBeInstanceOf(ConfigError);
    }
  });
});
