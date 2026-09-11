package ai.ctxmesh;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

/**
 * A fake launcher built from sdk/launcher-routes.json — the SAME fixture generated from
 * cmd/launcher — that 404s anything it does not recognise.
 *
 * <p>The previous fake was a permissive catch-all written by the author of the client it tests, so
 * both encoded the same wrong assumption and every test passed while 8 of 10 routes failed against
 * a real launcher. A fake that answers whatever it is asked cannot catch a wrong path.
 */
final class FakePlane implements AutoCloseable {
  record Route(String method, String path) {
    /** {param} matches exactly ONE segment: /memory/{id} must NOT match /memory/agent/remember. */
    boolean matches(String m, String p) {
      if (!method.equals("ANY") && !method.equals(m)) return false;
      String[] want = path.replaceAll("^/|/$", "").split("/");
      String[] got = p.replaceAll("^/|/$", "").split("/");
      if (want.length != got.length) return false;
      for (int i = 0; i < want.length; i++) {
        if (want[i].startsWith("{")) {
          if (got[i].isEmpty()) return false;
        } else if (!want[i].equals(got[i])) {
          return false;
        }
      }
      return true;
    }
  }

  private final HttpServer server;
  private final List<Route> routes;
  private final Set<String> seen = Collections.synchronizedSet(new HashSet<>());
  private final List<String> requests = Collections.synchronizedList(new ArrayList<>());
  volatile int status = 200;

  FakePlane() throws IOException {
    this.routes = loadRoutes();
    this.server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
    server.createContext("/", this::handle);
    server.start();
  }

  /** The fixture is shared by all six SDKs, so it lives beside them; walk up to find it. */
  static List<Route> loadRoutes() throws IOException {
    Path dir = Path.of("").toAbsolutePath();
    for (int i = 0; i < 6 && dir != null; i++, dir = dir.getParent()) {
      Path f = dir.resolve("launcher-routes.json");
      if (Files.exists(f)) {
        Object o = TestJson.read(Files.readString(f));
        List<Route> out = new ArrayList<>();
        for (Object r : TestJson.arr(TestJson.obj(o).get("routes"))) {
          var m = TestJson.obj(r);
          out.add(new Route(String.valueOf(m.get("method")), String.valueOf(m.get("path"))));
        }
        if (out.isEmpty()) throw new IOException("launcher-routes.json is empty");
        return out;
      }
    }
    throw new IOException("launcher-routes.json not found — the fixture IS the contract");
  }

  private void handle(HttpExchange ex) throws IOException {
    String method = ex.getRequestMethod();
    String path = ex.getRequestURI().getPath();
    requests.add(method + " " + path);
    Route hit = routes.stream().filter(r -> r.matches(method, path)).findFirst().orElse(null);
    if (hit == null) {
      byte[] b = "404 page not found".getBytes(StandardCharsets.UTF_8);
      ex.sendResponseHeaders(404, b.length);
      try (OutputStream os = ex.getResponseBody()) { os.write(b); }
      return;
    }
    seen.add(hit.method() + " " + hit.path());
    byte[] b = bodyFor(hit.path()).getBytes(StandardCharsets.UTF_8);
    ex.getResponseHeaders().add("Content-Type", "application/json");
    ex.sendResponseHeaders(status, b.length);
    try (OutputStream os = ex.getResponseBody()) { os.write(b); }
  }

  /** Mirrors what the real handlers return — verified against cmd/launcher, not against the client. */
  private static String bodyFor(String pattern) {
    return switch (pattern) {
      case "/memory/agent/search" ->
          "{\"results\":[{\"content\":\"strong\",\"score\":0.9},{\"content\":\"weak\",\"score\":0.1}]}";
      case "/knowledge/search" -> "{\"results\":[{\"content\":\"c\",\"documentRef\":\"d\",\"score\":0.5}]}";
      case "/skills" -> "{\"skills\":[{\"name\":\"s\",\"description\":\"d\"}]}";
      case "/skills/load" -> "{\"body\":\"the skill body\"}";   // "body", not "content"
      case "/delegate" -> "{\"ok\":true,\"subAgent\":\"sub\",\"subRun\":\"r1\",\"answer\":\"42\"}";
      case "/handoff" -> "{\"ok\":true,\"runId\":\"r1\",\"handedOffTo\":\"other\"}";
      case "/memory/{conversationId}", "/memory/{conversationId}/search" ->
          "[{\"role\":\"user\",\"content\":\"hi\"}]";
      default -> "{}";
    };
  }

  int port() { return server.getAddress().getPort(); }
  List<Route> routes() { return routes; }
  boolean covered(Route r) { return seen.contains(r.method() + " " + r.path()); }
  List<String> requests() { return List.copyOf(requests); }

  @Override public void close() { server.stop(0); }
}
