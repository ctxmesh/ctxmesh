package ai.ctxmesh;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Drives the Java SDK's public API against a real launcher, one call per contract route, and
 * prints "PASS|FAIL|MISSING <route>[: reason]" for each. hack/sdk-conformance compiles it with the
 * SDK's sources, runs it, and checks every PASS against what the launcher actually received.
 *
 * <p>It sits in the SDK's package only to read the contract with the SDK's own JSON reader; every
 * call it makes goes through the public API.
 */
public final class ConformanceProbe {
  private ConformanceProbe() {}

  interface Check { Object run(Map<String, Object> golden) throws Exception; }

  /** The current route's sdkResult: the response fields every SDK's result must carry. */
  private static List<Object> required = List.of();

  public static void main(String[] args) throws Exception {
    Map<String, Object> contract = map(Json.read(Files.readString(Path.of(System.getenv("CTXMESH_PROBE_CONTRACT")))));
    String capability = System.getenv("CTXMESH_PROBE_CAPABILITY");
    Client c = null;
    String configError = null;
    try {
      // fromEnv, not an explicit Config: the env the launcher publishes is part of what is checked.
      c = Client.fromEnv();
    } catch (RuntimeException e) {
      configError = e.getMessage();
    }
    Map<String, Check> checks = c == null ? Map.of() : checks(c, capability);
    for (Object o : list(contract.get("routes"))) {
      Map<String, Object> r = map(o);
      String key = r.get("method") + " " + r.get("path");
      Check check = checks.get(key);
      if (configError != null) {
        System.out.println("FAIL " + key + ": the SDK does not configure from the launcher's env: " + configError);
      } else if (check == null) {
        System.out.println("MISSING " + key + ": the Java SDK has no call for it");
      } else {
        try {
          required = list(r.get("sdkResult"));
          check.run(map(list(r.get("goldens")).get(0)));
          System.out.println("PASS " + key);
        } catch (Exception | AssertionError e) {
          String msg = e.getMessage() == null ? e.toString() : e.getMessage();
          System.out.println("FAIL " + key + ": " + msg.replace('\n', ' '));
        }
      }
    }
  }

  private static Map<String, Check> checks(Client c, String capability) {
    Map<String, Check> m = new LinkedHashMap<>();
    m.put("POST /a2a/{targetAgent}", g -> expect("reply",
        c.mesh.callLegacy(segment(g, 1), req(g).get("body")), resp(g)));
    m.put("POST /amp/{targetAgent}", g -> expect("reply",
        c.mesh.call(segment(g, 1), req(g).get("body")), resp(g)));
    m.put("POST /delegate", g -> {
      Map<String, Object> b = body(g);
      Client.Delegation d = c.runs.delegate(str(b.get("subAgent")), str(b.get("step")), str(b.get("callId")),
          capability, b.get("input"));
      Map<String, Object> got = new LinkedHashMap<>();
      got.put("ok", d.ok());
      got.put("subAgent", d.subAgent());
      got.put("subRun", d.subRun());
      got.put("answer", d.answer());
      got.put("error", d.error());
      got.put("suspend", d.suspend());
      got.put("endpoint", d.endpoint());
      return expect("delegation", got, resp(g));
    });
    m.put("ANY /feedback", g -> {
      Map<String, Object> b = body(g);
      c.feedback.score(str(b.get("traceId")), str(b.get("name")), num(b.get("value")), str(b.get("comment")));
      return null;
    });
    m.put("POST /handoff", g -> {
      Map<String, Object> b = body(g);
      Client.Handoff h = c.runs.handoff(str(b.get("targetAgent")), capability, str(b.get("message")),
          !Boolean.FALSE.equals(b.get("includeHistory")));
      Map<String, Object> got = new LinkedHashMap<>();
      got.put("ok", h.ok());
      got.put("runId", h.runId());
      got.put("sourceRun", h.sourceRun());
      got.put("handedOffTo", h.handedOffTo());
      got.put("error", h.error());
      return expect("handoff", got, resp(g));
    });
    m.put("POST /knowledge/search", g -> {
      Map<String, Object> b = body(g);
      List<Object> results = new ArrayList<>();
      for (Client.Chunk ch : c.knowledge.search(str(b.get("query")), str(b.get("knowledgeBase")),
          (int) num(b.get("topK")))) {
        Map<String, Object> e = new LinkedHashMap<>();
        e.put("content", ch.content());
        e.put("documentRef", ch.documentRef());
        e.put("knowledgeBase", ch.knowledgeBase());
        e.put("score", ch.score());
        results.add(e);
      }
      return expect("chunks", Map.of("results", results), resp(g));
    });
    m.put("POST /memory/agent/remember", g -> {
      Map<String, Object> b = body(g);
      Map<String, String> tags = new LinkedHashMap<>();
      map(b.get("tags")).forEach((k, v) -> tags.put(k, str(v)));
      c.memory.remember(str(b.get("content")), tags);
      return null;
    });
    m.put("POST /memory/agent/search", g -> {
      Map<String, Object> b = body(g);
      List<Object> results = new ArrayList<>();
      for (Client.Fact f : c.memory.searchAgent(str(b.get("query")), (int) num(b.get("topK")), 0)) {
        Map<String, Object> e = new LinkedHashMap<>();
        e.put("content", f.content());
        e.put("score", f.score());
        results.add(e);
      }
      return expect("facts", Map.of("results", results), resp(g));
    });
    m.put("GET /memory/{conversationId}", g -> expect("entries", entries(c.memory.get(segment(g, 1))), resp(g)));
    m.put("PUT /memory/{conversationId}", g -> {
      List<Client.Entry> in = new ArrayList<>();
      for (Object o : list(req(g).get("body"))) {
        in.add(new Client.Entry(str(map(o).get("role")), map(o).get("content")));
      }
      c.memory.put(in, segment(g, 1));
      return null;
    });
    m.put("POST /memory/{conversationId}/append", g -> {
      Map<String, Object> b = body(g);
      c.memory.append(new Client.Entry(str(b.get("role")), b.get("content")), segment(g, 1));
      return null;
    });
    m.put("GET /memory/{conversationId}/search", g -> expect("entries",
        entries(c.memory.search(str(map(req(g).get("query")).get("q")), segment(g, 1), null)), resp(g)));
    m.put("GET /skills", g -> {
      List<Object> skills = new ArrayList<>();
      for (Client.Skill s : c.skills.list()) {
        Map<String, Object> e = new LinkedHashMap<>();
        e.put("name", s.name());
        e.put("description", s.description());
        skills.add(e);
      }
      return expect("skills", Map.of("skills", skills), resp(g));
    });
    m.put("POST /skills/load", g -> expect("skill",
        Map.of("body", c.skills.load(str(body(g).get("name")))), resp(g)));
    return m;
  }

  private static List<Object> entries(List<Client.Entry> in) {
    List<Object> out = new ArrayList<>();
    for (Client.Entry e : in) {
      Map<String, Object> m = new LinkedHashMap<>();
      m.put("role", e.role());
      m.put("content", e.content());
      out.add(m);
    }
    return out;
  }

  /**
   * Compares what the SDK decoded with the launcher's golden answer. Every field the SDK's type
   * carries must hold the launcher's value; a field the SDK does not model is not its concern.
   */
  private static Object expect(String what, Object got, Object want) {
    String at = within(got, want, "$");
    if (at != null) {
      throw new AssertionError(what + " decoded as " + Json.write(got) + ", but the launcher sent "
          + Json.write(want) + " (differs at " + at + ")");
    }
    // within compares only what the SDK kept, so it passes an SDK that dropped a field.
    for (Object path : required) {
      String p = String.valueOf(path);
      String missed = "$".equals(p) ? (same(got, want) ? null : "$") : carry(got, want, List.of(p.split("\\.")), "$");
      if (missed != null) {
        throw new AssertionError(what + " decoded as " + Json.write(got) + ", which does not carry " + p
            + " with the launcher's value (at " + missed + "); the launcher sent " + Json.write(want));
      }
    }
    return null;
  }

  private static boolean same(Object got, Object want) {
    return within(got, want, "$") == null && within(want, got, "$") == null;
  }

  /** Where got fails to hold want's value at a sdkResult path ("[]" descends into each element). */
  private static String carry(Object got, Object want, List<String> segs, String at) {
    if (segs.isEmpty()) return same(got, want) ? null : at;
    boolean each = segs.get(0).endsWith("[]");
    String key = each ? segs.get(0).substring(0, segs.get(0).length() - 2) : segs.get(0);
    if (!key.isEmpty()) {
      if (!(got instanceof Map<?, ?> g) || !g.containsKey(key)) return at + "." + key;
      got = g.get(key);
      want = map(want).get(key);
      at = at + "." + key;
    }
    List<String> rest = segs.subList(1, segs.size());
    if (!each) return carry(got, want, rest, at);
    if (!(got instanceof List<?> gl) || !(want instanceof List<?> wl) || gl.size() != wl.size()) return at;
    for (int i = 0; i < wl.size(); i++) {
      String p = carry(gl.get(i), wl.get(i), rest, at + "[" + i + "]");
      if (p != null) return p;
    }
    return null;
  }

  /** Where got first leaves want, or null when every value in got appears, equal, in want. */
  private static String within(Object got, Object want, String at) {
    if (got instanceof Map<?, ?> g) {
      if (!(want instanceof Map<?, ?> w)) return at;
      for (Map.Entry<?, ?> e : g.entrySet()) {
        if (isZero(e.getValue()) && !w.containsKey(e.getKey())) continue;
        String p = within(e.getValue(), w.get(e.getKey()), at + "." + e.getKey());
        if (p != null) return p;
      }
      return null;
    }
    if (got instanceof List<?> g) {
      if (!(want instanceof List<?> w) || w.size() != g.size()) return at;
      for (int i = 0; i < g.size(); i++) {
        String p = within(g.get(i), w.get(i), at + "[" + i + "]");
        if (p != null) return p;
      }
      return null;
    }
    if (got instanceof Number a && want instanceof Number b) {
      return a.doubleValue() == b.doubleValue() ? null : at;
    }
    return got == null ? (want == null ? null : at) : (got.equals(want) ? null : at);
  }

  private static boolean isZero(Object v) {
    return v == null || "".equals(v) || Boolean.FALSE.equals(v)
        || (v instanceof Number n && n.doubleValue() == 0) || (v instanceof Map<?, ?> m && m.isEmpty());
  }

  private static Map<String, Object> req(Map<String, Object> g) { return map(g.get("request")); }

  private static Map<String, Object> body(Map<String, Object> g) { return map(req(g).get("body")); }

  private static Object resp(Map<String, Object> g) { return map(g.get("response")).get("body"); }

  private static String segment(Map<String, Object> g, int i) {
    String p = str(req(g).get("path")).replaceAll("^/+|/+$", "");
    String[] parts = p.split("/");
    return i < parts.length ? parts[i] : "";
  }

  @SuppressWarnings("unchecked")
  private static Map<String, Object> map(Object o) {
    return o instanceof Map<?, ?> m ? (Map<String, Object>) m : Collections.emptyMap();
  }

  @SuppressWarnings("unchecked")
  private static List<Object> list(Object o) {
    return o instanceof List<?> l ? (List<Object>) l : Collections.emptyList();
  }

  private static String str(Object o) { return o == null ? "" : String.valueOf(o); }

  private static double num(Object o) { return o instanceof Number n ? n.doubleValue() : 0; }
}
