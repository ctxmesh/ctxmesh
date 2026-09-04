import { describe, expect, it } from "vitest";
import { fromConfig } from "../src/agent.js";
import { EndpointError } from "../src/errors.js";
import { startPlane } from "../src/testing.js";

describe("MeshStub", () => {
  it("returns the peer response and refuses typed, via startPlane", async () => {
    // startPlane is the front door — every SDK test uses it rather than assembling stubs by
    // hand, so the mesh has to be reachable that way or the fake is shipped-but-unusable.
    const plane = await startPlane({ mesh: { deny: { nope: 403 } } });
    const stub = plane.mesh;
    try {
      const c = fromConfig(plane.config);
      expect(await c.mesh.call("research", { q: "hi" })).toEqual({
        ok: true,
        answer: "from the peer",
      });
      await expect(c.mesh.call("nope", {})).rejects.toBeInstanceOf(EndpointError);
      expect(stub.requests.map((r) => r.path)).toEqual(["/a2a/research", "/a2a/nope"]);
    } finally {
      await plane.stop();
    }
  });
});
