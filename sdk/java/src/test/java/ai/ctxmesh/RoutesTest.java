package ai.ctxmesh;

import static org.junit.jupiter.api.Assertions.*;

import java.io.IOException;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * Coverage is asserted against sdk/launcher-routes.json — generated from cmd/launcher — with a fake
 * that 404s anything unregistered. The gate can no longer assert this: it grepped the SDK source
 * for route substrings, which a COMMENT satisfied.
 */
class RoutesTest {
  private static final String CAP = "cap-token";

  @Test
  void everyLauncherRouteIsExercised() throws IOException {
    try (FakePlane fake = new FakePlane()) {
      int p = fake.port();
      Client c = Client.fromConfig(Config.of(p, p, p, p, "conv-1"));

      c.memory.get(null);
      c.memory.append(new Client.Entry("user", "hi"), null);
      c.memory.put(java.util.List.of(new Client.Entry("user", "hi")), null);
      c.memory.search("hi", null, CAP);
      c.memory.remember("a fact", Map.of("topic", "x"));
      c.memory.searchAgent("q", 3, 0);
      c.knowledge.search("q", "kb", 3);
      c.skills.list();
      c.skills.load("s");
      c.feedback.score("t1", "helpfulness", 1.0, "clear");
      c.mesh.call("other", Map.of("q", 1));
      c.mesh.callLegacy("other", Map.of("q", 1));
      c.runs.delegate("sub", "step-1", "call-1", CAP, Map.of("x", 1));
      c.runs.handoff("other", CAP, "over to you", true);

      for (FakePlane.Route r : fake.routes()) {
        assertTrue(fake.covered(r),
            "launcher serves " + r.method() + " " + r.path()
                + " and no SDK call reached it; requests=" + fake.requests());
      }
    }
  }

  @Test
  void skillsLoadReturnsTheBody() throws IOException {
    // The launcher answers {"body": …}. Reading "content" returned "" with NO error, so a skill
    // loaded as nothing and the model carried on without it.
    try (FakePlane fake = new FakePlane()) {
      int p = fake.port();
      Client c = Client.fromConfig(Config.of(p, p, p, p, "c"));
      assertEquals("the skill body", c.skills.load("s"));
    }
  }

  @Test
  void delegateRefusesWithoutItsRequiredFields() {
    Client c = Client.fromConfig(Config.of(1, 1, 1, 1, "c"));
    assertThrows(CtxmeshException.class, () -> c.runs.delegate("sub", "", "call", CAP, null));
    assertThrows(CtxmeshException.class, () -> c.runs.delegate("sub", "step", "", CAP, null));
    assertThrows(NotWiredException.class, () -> c.runs.delegate("sub", "step", "call", "", null));
    assertThrows(NotWiredException.class, () -> c.runs.handoff("other", "", null, true));
  }

  @Test
  void delegateUsesItsOwnListener() throws IOException {
    // /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port
    // was a 404 in all four SDKs, and the old gate had no port model to notice.
    try (FakePlane memory = new FakePlane(); FakePlane delegate = new FakePlane()) {
      Client c = Client.fromConfig(
          Config.of(memory.port(), memory.port(), memory.port(), delegate.port(), "c"));
      c.runs.delegate("sub", "s", "c", CAP, null);

      FakePlane.Route d = new FakePlane.Route("POST", "/delegate");
      assertTrue(delegate.covered(d), "/delegate did not reach the delegate listener");
      assertFalse(memory.covered(d), "/delegate reached the MEMORY listener — the 404 bug is back");
    }
  }

  @Test
  void delegateRefusalIsNotSuccess() throws IOException {
    // The launcher answers 200 for every outcome and signals success in "ok". Decoding
    // {runId, accepted} made refusal, failure and success indistinguishable — and dropped
    // "answer", which is the entire point of delegating.
    try (FakePlane fake = new FakePlane()) {
      int p = fake.port();
      Client c = Client.fromConfig(Config.of(p, p, p, p, "c"));
      Client.Delegation d = c.runs.delegate("sub", "s", "c", CAP, null);
      assertTrue(d.ok(), "ok must be decoded");
      assertEquals("42", d.answer(), "answer must survive");
    }
  }

  @Test
  void unwiredCapabilitiesRefuseLocally() {
    Config bare = new Config(1, 1, 1, 1, "", "c", false, false, false, false);
    Client c = Client.fromConfig(bare);
    assertThrows(NotWiredException.class, () -> c.memory.get(null));
    assertThrows(NotWiredException.class, () -> c.feedback.score("t", "d", 1, null));
    assertThrows(NotWiredException.class, () -> c.knowledge.search("q", "kb", 1));
  }

  @Test
  void configReadsTheEnvTheLauncherPublishes() {
    // A2A_PORT and DELEGATE_PORT are what the launcher injects; AMP_PORT was invented.
    Config c = Config.fromEnv(k -> switch (k) {
      case "AGENT_NAME" -> "a";
      case "A2A_PORT" -> "3997";
      case "DELEGATE_PORT" -> "3994";
      default -> null;
    });
    assertEquals(3997, c.ampPort);
    assertEquals(3994, c.delegatePort);
    assertFalse(c.memoryWired, "MEMORY_PORT unset must mean memory is NOT wired");
  }

  @Test
  void badPortIsRejectedNotDefaulted() {
    assertThrows(NotInPodException.class,
        () -> Config.fromEnv(k -> "MEMORY_PORT".equals(k) ? "99999" : null));
  }
}
