package ai.ctxmesh;

import static org.junit.jupiter.api.Assertions.*;

import java.io.IOException;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

/**
 * The fake is generated from sdk/launcher-routes.json, whose goldens cmd/launcher proves against
 * the real handlers. It 404s anything the launcher does not serve and records every request that
 * breaks a route's schema, so a client calling the wrong path, or sending the wrong field, fails
 * here.
 */
class RoutesTest {
  private static final String CAP = "probe-run-capability";

  interface Call { void run() throws Exception; }

  private static Client clientFor(GeneratedFakeLauncher f) {
    int p = f.port();
    return Client.fromConfig(Config.of(p, p, p, p, "conv-1"));
  }

  @Test
  void everyLauncherRouteIsExercised() throws IOException {
    try (GeneratedFakeLauncher fake = new GeneratedFakeLauncher()) {
      Client c = clientFor(fake);
      // Every call the plane-client tier owes. One fake serves every listener, so a route sent to
      // the wrong PORT still reaches it; delegateUsesItsOwnListener covers ports.
      Map<String, Call> calls = new LinkedHashMap<>();
      calls.put("GET /memory/{conversationId}", () -> c.memory.get(null));
      calls.put("POST /memory/{conversationId}/append",
          () -> c.memory.append(new Client.Entry("user", "hi"), null));
      calls.put("PUT /memory/{conversationId}",
          () -> c.memory.put(List.of(new Client.Entry("user", "hi")), null));
      calls.put("GET /memory/{conversationId}/search", () -> c.memory.search("metric", null, CAP));
      calls.put("POST /memory/agent/remember",
          () -> c.memory.remember("The user prefers metric units.", Map.of("topic", "preferences")));
      calls.put("POST /memory/agent/search", () -> c.memory.searchAgent("units", 3, 0));
      calls.put("POST /knowledge/search",
          () -> c.knowledge.search("When do deploys go out?", "handbook", 3));
      calls.put("GET /skills", () -> c.skills.list());
      calls.put("POST /skills/load", () -> c.skills.load("deploy"));
      calls.put("ANY /feedback",
          () -> c.feedback.score("0af7651916cd43dd8448eb211c80319c", "helpfulness", 0.9, "clear answer"));
      calls.put("POST /amp/{targetAgent}", () -> c.mesh.call("peer-agent", Map.of("question", "ping")));
      calls.put("POST /a2a/{targetAgent}",
          () -> c.mesh.callLegacy("peer-agent", Map.of("question", "ping")));
      calls.put("POST /delegate",
          () -> c.runs.delegate("researcher", "1", "call-1", CAP, "Summarise the quarterly report"));
      calls.put("POST /handoff",
          () -> c.runs.handoff("billing", CAP, "The customer is asking about an invoice.", true));

      for (Map.Entry<String, Call> e : calls.entrySet()) {
        try {
          e.getValue().run();
        } catch (Exception ex) {
          fail(e.getKey() + ": " + ex);
        }
      }
      for (String key : fake.routeKeys()) {
        String why = fake.waived(key);
        if (!why.isEmpty()) {
          assertFalse(fake.isCovered(key),
              key + " is waived for Java (" + why + ") but the SDK reaches it now: delete the waiver");
        } else {
          assertTrue(fake.isCovered(key), "the launcher serves " + key + " and no SDK call reached it");
        }
      }
      fake.assertConforms();
    }
  }

  @Test
  void responsesDecodeAsTheLauncherSendsThem() throws IOException {
    try (GeneratedFakeLauncher fake = new GeneratedFakeLauncher()) {
      Client c = clientFor(fake);

      List<Client.Entry> entries = c.memory.get("conv-1");
      assertEquals(2, entries.size());
      assertEquals(new Client.Entry("assistant", "You prefer metric units."), entries.get(1));

      List<Client.Fact> facts = c.memory.searchAgent("units", 3, 0);
      assertEquals(List.of(new Client.Fact("The user prefers metric units.", 0.91)), facts);

      List<Client.Chunk> chunks = c.knowledge.search("When do deploys go out?", "handbook", 3);
      assertEquals(1, chunks.size());
      assertEquals("handbook/deploys.md", chunks.get(0).documentRef());
      assertEquals(0.88, chunks.get(0).score());

      assertEquals(List.of(new Client.Skill("deploy", "Deploy a service to the staging cluster.")),
          c.skills.list());

      assertEquals("pong", c.mesh.call("peer-agent", Map.of("question", "ping")).get("answer"));

      Client.Handoff h = c.runs.handoff("billing", CAP, "over to you", true);
      assertTrue(h.ok());
      assertEquals("run-billing-1", h.runId());
      assertEquals("run-supervisor-1", h.sourceRun());
      assertEquals("billing", h.handedOffTo());
      fake.assertConforms();
    }
  }

  @Test
  void skillsLoadReturnsTheBody() throws IOException {
    // The launcher answers {"body": …}. Reading "content" returned "" with NO error, so a skill
    // loaded as nothing and the model carried on without it.
    try (GeneratedFakeLauncher fake = new GeneratedFakeLauncher()) {
      Object want = GeneratedFakeLauncher.map(fake.golden("POST /skills/load", "").responseBody()).get("body");
      String got = clientFor(fake).skills.load("deploy");
      assertFalse(got.isEmpty(), "skills.load returned an empty body with no error");
      assertEquals(want, got);
    }
  }

  @Test
  void refusalsSurfaceTheLauncherStatus() throws IOException {
    try (GeneratedFakeLauncher fake = new GeneratedFakeLauncher()) {
      fake.useGolden("POST /knowledge/search", "refuses-a-knowledge-base-not-granted");
      fake.useGolden("POST /skills/load", "refuses-a-skill-not-attached");
      fake.useGolden("PUT /memory/{conversationId}", "refuses-a-stale-write");
      Client c = clientFor(fake);

      DeniedException kb = assertThrows(DeniedException.class, () -> c.knowledge.search("salaries", "payroll", 3));
      assertEquals(403, kb.status());
      assertTrue(kb.body().contains("not granted"), "the launcher's reason must survive: " + kb.body());
      assertEquals(403, assertThrows(DeniedException.class, () -> c.skills.load("payroll")).status());
      ApiException stale = assertThrows(ApiException.class,
          () -> c.memory.put(List.of(new Client.Entry("user", "x")), "conv-1"));
      assertEquals(412, stale.status());
    }
  }

  @Test
  void delegateRefusesWithoutItsRequiredFields() {
    // step, callId and the capability are hard-required by the handler. Refusing locally turns a
    // confusing remote refusal into a clear one.
    Client c = Client.fromConfig(Config.of(1, 1, 1, 1, "c"));
    assertThrows(CtxmeshException.class, () -> c.runs.delegate("sub", "", "call", CAP, null));
    assertThrows(CtxmeshException.class, () -> c.runs.delegate("sub", "step", "", CAP, null));
    assertThrows(NotWiredException.class, () -> c.runs.delegate("sub", "step", "call", "", null));
    assertThrows(NotWiredException.class, () -> c.runs.handoff("other", "", null, true));
  }

  @Test
  void delegateUsesItsOwnListener() throws IOException {
    // /delegate and /handoff are served by a DIFFERENT listener. Sending them to the memory port
    // was a 404 in all four plane-client SDKs.
    try (GeneratedFakeLauncher memory = new GeneratedFakeLauncher();
         GeneratedFakeLauncher delegate = new GeneratedFakeLauncher()) {
      Client c = Client.fromConfig(
          Config.of(memory.port(), memory.port(), memory.port(), delegate.port(), "c"));
      c.runs.delegate("researcher", "1", "call-1", CAP, "x");
      assertTrue(delegate.isCovered("POST /delegate"), "/delegate did not reach the delegate listener");
      assertFalse(memory.isCovered("POST /delegate"), "/delegate reached the MEMORY listener");
    }
  }

  @Test
  void delegateDecodesSuccessAndRefusal() throws IOException {
    // The launcher answers 200 for every outcome and signals success in "ok", so a refusal decoded
    // as a transport success would be silent data loss, and "answer" is the point of delegating.
    try (GeneratedFakeLauncher fake = new GeneratedFakeLauncher()) {
      Client c = clientFor(fake);
      Client.Delegation d = c.runs.delegate("researcher", "1", "call-1", CAP, "Summarise the quarterly report");
      assertTrue(d.ok(), "ok must be decoded");
      assertEquals("The quarterly report is ready.", d.answer(), "answer must survive");
      assertEquals("sub-run-1", d.subRun());

      fake.useGolden("POST /delegate", "refuses-without-a-run-capability");
      Client.Delegation refused = c.runs.delegate("researcher", "1", "call-1", CAP, "x");
      assertFalse(refused.ok(), "a refusal must decode as ok=false");
      assertFalse(refused.error().isEmpty(), "a refusal must carry its reason");

      fake.useGolden("POST /delegate", "suspends-without-spawning-when-asked");
      Client.Delegation suspended = c.runs.delegate("researcher", "1", "call-1", CAP, "x");
      assertTrue(suspended.suspend());
      assertEquals("http://researcher.conformance.svc.cluster.local", suspended.endpoint());
      fake.assertConforms();
    }
  }

  @Test
  void theFakeRefusesWhatTheLauncherRefuses() throws Exception {
    // The fake is only worth generating if it catches the defects hand-written fakes let through.
    try (GeneratedFakeLauncher fake = new GeneratedFakeLauncher()) {
      HttpClient http = HttpClient.newHttpClient();
      String base = "http://127.0.0.1:" + fake.port();
      HttpResponse<String> r = http.send(HttpRequest.newBuilder(URI.create(base + "/memory/agent/remember"))
          .POST(HttpRequest.BodyPublishers.ofString("{\"text\":\"a fact\"}")).build(),
          HttpResponse.BodyHandlers.ofString());
      assertEquals(202, r.statusCode());
      assertFalse(fake.violations().isEmpty(), "a remember body without content passed the fake");

      r = http.send(HttpRequest.newBuilder(URI.create(base + "/memory/agent/forget"))
          .POST(HttpRequest.BodyPublishers.ofString("{}")).build(), HttpResponse.BodyHandlers.ofString());
      assertEquals(404, r.statusCode(), "a path the launcher does not serve must 404");

      r = http.send(HttpRequest.newBuilder(URI.create(base + "/skills/load")).GET().build(),
          HttpResponse.BodyHandlers.ofString());
      assertEquals(405, r.statusCode(), "a served path under the wrong method must 405");

      assertThrows(AssertionError.class, () -> fake.respond("POST /skills/load", 418, ""),
          "a status the launcher never sends must be refused");
      assertThrows(AssertionError.class, () -> fake.respond("POST /skills/load", 200, "{\"content\":\"x\"}"),
          "a body outside the declared schema must be refused");
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
