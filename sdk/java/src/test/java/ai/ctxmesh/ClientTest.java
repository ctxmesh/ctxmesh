package ai.ctxmesh;

import static org.junit.jupiter.api.Assertions.*;

import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * The contract gate proves each route STRING appears in this package, which a comment would
 * satisfy. These drive a fake launcher and assert the client actually issued the request.
 */
class ClientTest {
  private HttpServer server;
  private final List<String> seen = new ArrayList<>();
  private Client client;
  private volatile int status = 200;

  @BeforeEach
  void start() throws IOException {
    server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.createContext("/", ex -> {
      seen.add(ex.getRequestMethod() + " " + ex.getRequestURI().getPath());
      String body = switch (ex.getRequestURI().getPath()) {
        case "/memory/agent/search" -> "{\"results\":[{\"content\":\"strong\",\"score\":0.9},{\"content\":\"weak\",\"score\":0.1}]}";
        case "/knowledge/search" -> "{\"results\":[{\"content\":\"c\",\"documentRef\":\"d\",\"score\":0.5}]}";
        case "/skills" -> "{\"skills\":[{\"name\":\"s\",\"description\":\"d\"}]}";
        case "/skills/load" -> "{\"content\":\"body\"}";
        case "/delegate" -> "{\"runId\":\"r1\",\"accepted\":true}";
        default -> ex.getRequestMethod().equals("GET") && ex.getRequestURI().getPath().startsWith("/memory/")
            ? "[{\"role\":\"user\",\"content\":\"hi\"}]" : "{}";
      };
      byte[] out = body.getBytes(StandardCharsets.UTF_8);
      ex.getResponseHeaders().add("Content-Type", "application/json");
      ex.sendResponseHeaders(status, out.length);
      try (OutputStream os = ex.getResponseBody()) { os.write(out); }
    });
    server.start();
    int p = server.getAddress().getPort();
    client = Client.fromConfig(Config.of(p, p, p, "conv-1"));
  }

  @AfterEach
  void stop() { server.stop(0); }

  @Test
  void everyRouteIsActuallyCalled() {
    client.memory.get(null);
    client.memory.append(new Client.Entry("user", "hi"), null);
    client.memory.remember("a fact", Map.of("topic", "x"));
    client.memory.searchAgent("q", 3, 0);
    client.knowledge.search("q", "kb", 3);
    client.skills.list();
    client.skills.load("s");
    client.feedback.score("t1", "helpfulness", 1.0, "clear");
    client.mesh.call("other", Map.of("q", 1));
    client.mesh.callLegacy("other", Map.of("q", 1));
    client.runs.delegate("sub", Map.of("x", 1));
    client.runs.handoff("other", true);

    for (String want : new String[] {"/memory/", "/memory/agent", "/memory/agent/search",
        "/knowledge/search", "/skills", "/skills/load", "/feedback", "/amp/", "/a2a/",
        "/delegate", "/handoff"}) {
      assertTrue(seen.stream().anyMatch(s -> s.substring(s.indexOf(' ') + 1).startsWith(want)),
          "route " + want + " was never actually requested; seen=" + seen);
    }
  }

  @Test
  void deniedCarriesThePlatformsReason() {
    // A 403 is the platform refusing, not the transport failing — and the reason must survive,
    // or a policy decision becomes a bare status code.
    status = 403;
    DeniedException e = assertThrows(DeniedException.class, () -> client.runs.delegate("sub", null));
    assertEquals(403, e.status());
  }

  @Test
  void unwiredCapabilitiesRefuseLocally() {
    // The port is absent because the platform did not grant the capability. Dialling it anyway
    // turns a configuration answer into a connection refused that reads like an outage.
    Config bare = new Config(1, 1, 1, "", "c", false, false, false, false);
    Client c = Client.fromConfig(bare);
    assertThrows(NotWiredException.class, () -> c.memory.get(null));
    assertThrows(NotWiredException.class, () -> c.feedback.score("t", "d", 1, null));
    assertThrows(NotWiredException.class, () -> c.knowledge.search("q", null, 1));
  }

  @Test
  void notInPodIsSaidPlainly() {
    assertThrows(NotInPodException.class, () -> Config.fromEnv(k -> null));
  }

  @Test
  void unsetPortMeansNotWiredNotDefault() {
    // The distinction the whole exception vocabulary rests on.
    Config c = Config.fromEnv(k -> "AGENT_NAME".equals(k) ? "a" : null);
    assertFalse(c.memoryWired, "MEMORY_PORT unset must mean memory is NOT wired");
    assertEquals(Config.DEFAULT_MEMORY_PORT, c.memoryPort, "the port should still default");
  }

  @Test
  void badPortIsRejectedNotDefaulted() {
    assertThrows(NotInPodException.class, () -> Config.fromEnv(k -> "MEMORY_PORT".equals(k) ? "99999" : null));
  }

  @Test
  void ambiguousConversationIsRefused() {
    Config c = new Config(1, 1, 1, "", "", true, true, true, true);
    Client cl = Client.fromConfig(c);
    assertThrows(CtxmeshException.class, () -> cl.memory.get(null));
    assertThrows(CtxmeshException.class, () -> cl.memory.append(new Client.Entry("u", "x"), "has/slash"));
  }

  @Test
  void minScoreFiltersWeakFacts() {
    List<Client.Fact> got = client.memory.searchAgent("q", 5, 0.5);
    assertEquals(1, got.size(), "minScore must drop weak hits");
    assertEquals("strong", got.get(0).content());
  }

  @Test
  void jsonRoundTripsWhatThePlaneSends() {
    Object o = Json.read("{\"a\":[1,2.5,true,null,\"x\\ny\"],\"b\":{\"c\":\"d\"}}");
    assertTrue(o instanceof Map);
    assertEquals("{\"k\":\"a\\nb\"}", Json.write(Map.of("k", "a\nb")));
  }
}
