package ai.ctxmesh;

import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * The Java SDK for agents running on ctxmesh.
 *
 * <p>An agent runs in a pod beside the platform's sidecars; this is the typed way to reach them
 * over localhost. It holds no credentials — endpoints and identity arrive in the environment the
 * platform injects.
 *
 * <p>Conformance tier: plane-client (ADR 0139). The managed agent loop and model client are
 * authoring-tier and live in the Python and TypeScript SDKs.
 */
public final class Client {
  /** Stamped from the product tag at release (ADR 0135). */
  public static final String VERSION = "0.1.0-beta.3";

  /**
   * Carries the run capability. Delegation, handoff, per-user session memory, per-user long-term
   * memory and per-user knowledge bases all key on it. Session memory fails SAFE without it —
   * every user silently shares the agent-wide bucket instead of their own — so omitting it
   * defeats an isolation control with no error to notice.
   */
  public static final String CAPABILITY_HEADER = "X-Ctxmesh-Run-Capability";

  private final Config cfg;
  private final HttpClient http;

  public final Memory memory = new Memory();
  public final Knowledge knowledge = new Knowledge();
  public final Skills skills = new Skills();
  public final Feedback feedback = new Feedback();
  public final Mesh mesh = new Mesh();
  public final Runs runs = new Runs();

  private Client(Config cfg) {
    this.cfg = cfg;
    this.http = HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(5)).build();
  }

  /** Reads the launcher environment. Throws NotInPodException outside a ctxmesh pod. */
  public static Client fromEnv() { return new Client(Config.fromSystemEnv()); }

  /** Builds a Client from an explicit Config — for tests. */
  public static Client fromConfig(Config cfg) { return new Client(cfg); }

  public Config config() { return cfg; }

  // ── transport ──────────────────────────────────────────────────────────────

  private Object send(String method, String url, Object body, Duration timeout) {
    return send(method, url, body, timeout, Map.of());
  }

  private Object send(String method, String url, Object body, Duration timeout,
                      Map<String, String> headers) {
    HttpRequest.BodyPublisher pub = body == null
        ? HttpRequest.BodyPublishers.noBody()
        : HttpRequest.BodyPublishers.ofString(Json.write(body), StandardCharsets.UTF_8);
    HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(url))
        .timeout(timeout)
        .header("Accept", "application/json")
        .method(method, pub);
    if (body != null) b.header("Content-Type", "application/json");
    headers.forEach((k, v) -> { if (v != null && !v.isBlank()) b.header(k, v); });

    HttpResponse<String> resp;
    try {
      resp = http.send(b.build(), HttpResponse.BodyHandlers.ofString());
    } catch (IOException e) {
      throw new CtxmeshException("ctxmesh: " + path(url) + ": " + e.getMessage(), e);
    } catch (InterruptedException e) {
      Thread.currentThread().interrupt();
      throw new CtxmeshException("ctxmesh: interrupted calling " + path(url), e);
    }
    String text = resp.body() == null ? "" : resp.body().trim();
    if (resp.statusCode() == 403) throw new DeniedException(path(url), text);
    if (resp.statusCode() < 200 || resp.statusCode() > 299) {
      throw new ApiException(resp.statusCode(), path(url), text);
    }
    if (text.isEmpty()) return null;
    return Json.read(text);
  }

  private Object send(String method, String url, Object body) {
    return send(method, url, body, Duration.ofSeconds(15));
  }

  /** Keeps the port out of error text: it is an implementation detail. */
  private static String path(String url) {
    int i = url.indexOf("//");
    if (i >= 0) {
      int j = url.indexOf('/', i + 2);
      if (j >= 0) return url.substring(j);
    }
    return url;
  }

  private static String enc(String s) { return URLEncoder.encode(s, StandardCharsets.UTF_8); }

  @SuppressWarnings("unchecked")
  private static List<Object> jsonArray(Object o, String key) {
    if (o instanceof Map<?, ?> m && m.get(key) instanceof List<?> l) return (List<Object>) l;
    if (o instanceof List<?> l) return (List<Object>) l;
    return List.of();
  }

  @SuppressWarnings("unchecked")
  private static Map<String, Object> map(Object o) {
    return o instanceof Map<?, ?> m ? (Map<String, Object>) m : Map.of();
  }

  private static String s(Map<String, Object> m, String k) {
    Object v = m.get(k);
    return v == null ? "" : String.valueOf(v);
  }

  private static double d(Map<String, Object> m, String k) {
    Object v = m.get(k);
    return v instanceof Number n ? n.doubleValue() : 0.0;
  }

  // ── memory: /memory and /memory/agent ─────────────────────────────────────

  /** One conversation turn. */
  public record Entry(String role, Object content) {}

  /** One long-term memory, with the score its retrieval assigned. */
  public record Fact(String content, double score) {}

  public final class Memory {
    private void require() {
      if (!cfg.memoryWired) throw new NotWiredException("ctxmesh: memory is not wired (MEMORY_PORT unset)");
    }

    private void requireLongTerm() {
      require();
      if (!cfg.longTermEnabled) {
        throw new NotWiredException("ctxmesh: long-term memory is not enabled (MEMORY_LONGTERM_ENABLED)");
      }
    }

    /** Explicit id first, else the injected CONVERSATION_ID. Empty is an error, never a guess. */
    private String conv(String id) {
      String v = (id == null || id.isBlank()) ? cfg.conversationId : id;
      if (v == null || v.isBlank()) {
        throw new CtxmeshException("ctxmesh: no conversation id (pass one, or set CONVERSATION_ID)");
      }
      if (v.chars().anyMatch(c -> c == '/' || Character.isWhitespace(c))) {
        throw new CtxmeshException("ctxmesh: conversation id '" + v + "' contains a separator or whitespace");
      }
      return v;
    }

    /** Returns the conversation so far. */
    public List<Entry> get(String conversationId) {
      require();
      Object o = send("GET", cfg.memoryBase() + "/memory/" + enc(conv(conversationId)), null);
      List<Entry> out = new ArrayList<>();
      for (Object e : jsonArray(o, "entries")) {
        Map<String, Object> m = map(e);
        out.add(new Entry(s(m, "role"), m.get("content")));
      }
      return out;
    }

    /** Adds one entry to the conversation. */
    public void append(Entry entry, String conversationId) {
      require();
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("role", entry.role());
      body.put("content", entry.content());
      send("POST", cfg.memoryBase() + "/memory/" + enc(conv(conversationId)) + "/append", body);
    }

    /**
     * Searches this conversation's memory. {@code capability} is optional: without it a per-user
     * agent silently reads the agent-wide bucket rather than the caller's own.
     */
    public List<Entry> search(String query, String conversationId, String capability) {
      require();
      String url = cfg.memoryBase() + "/memory/" + enc(conv(conversationId))
          + "/search?q=" + enc(query == null ? "" : query);
      Map<String, String> h = capability == null || capability.isBlank()
          ? Map.of() : Map.of(CAPABILITY_HEADER, capability);
      Object o = send("GET", url, null, Duration.ofSeconds(15), h);
      List<Entry> out = new ArrayList<>();
      for (Object e : jsonArray(o, "entries")) {
        Map<String, Object> m = map(e);
        out.add(new Entry(s(m, "role"), m.get("content")));
      }
      return out;
    }

    /** Replaces the conversation wholesale. */
    public void put(List<Entry> entries, String conversationId) {
      require();
      List<Object> body = new ArrayList<>();
      for (Entry e : entries) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("role", e.role());
        m.put("content", e.content());
        body.add(m);
      }
      send("PUT", cfg.memoryBase() + "/memory/" + enc(conv(conversationId)), body);
    }

    /** Writes a fact to this agent's long-term memory. */
    public void remember(String content, Map<String, String> tags) {
      requireLongTerm();
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("content", content);
      if (tags != null && !tags.isEmpty()) body.put("tags", tags);
      send("POST", cfg.memoryBase() + "/memory/agent/remember", body);
    }

    /** Retrieves facts from long-term memory; minScore drops weak matches. */
    public List<Fact> searchAgent(String query, int topK, double minScore) {
      requireLongTerm();
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("query", query);
      body.put("topK", topK <= 0 ? 5 : topK);
      Object o = send("POST", cfg.memoryBase() + "/memory/agent/search", body);
      List<Fact> out = new ArrayList<>();
      for (Object e : jsonArray(o, "results")) {
        Map<String, Object> m = map(e);
        double sc = d(m, "score");
        if (sc >= minScore) out.add(new Fact(s(m, "content"), sc));
      }
      return out;
    }
  }

  // ── knowledge: /knowledge/search ──────────────────────────────────────────

  /** One retrieval hit, with the provenance a citation needs. */
  public record Chunk(String content, String documentRef, String knowledgeBase, double score) {}

  public final class Knowledge {
    /** Retrieval over ONE knowledge base. Required — the launcher 400s without it. */
    public List<Chunk> search(String query, String knowledgeBase, int topK) {
      if (!cfg.knowledgeEnabled) {
        throw new NotWiredException("ctxmesh: knowledge is not enabled (KNOWLEDGE_BASE_ENABLED)");
      }
      if (knowledgeBase == null || knowledgeBase.isBlank()) {
        throw new CtxmeshException("ctxmesh: knowledgeBase is required");
      }
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("query", query);
      body.put("topK", topK <= 0 ? 5 : topK);
      body.put("knowledgeBase", knowledgeBase);
      // Longer: this may wait on an embedding call through the token-service.
      Object o = send("POST", cfg.memoryBase() + "/knowledge/search", body, Duration.ofSeconds(60));
      List<Chunk> out = new ArrayList<>();
      for (Object e : jsonArray(o, "results")) {
        Map<String, Object> m = map(e);
        out.add(new Chunk(s(m, "content"), s(m, "documentRef"), s(m, "knowledgeBase"), d(m, "score")));
      }
      return out;
    }
  }

  // ── skills: /skills and /skills/load ──────────────────────────────────────

  /** One attached skill. */
  public record Skill(String name, String description) {}

  public final class Skills {
    /** The skills the platform attached to this agent. */
    public List<Skill> list() {
      Object o = send("GET", cfg.memoryBase() + "/skills", null);
      List<Skill> out = new ArrayList<>();
      for (Object e : jsonArray(o, "skills")) {
        Map<String, Object> m = map(e);
        out.add(new Skill(s(m, "name"), s(m, "description")));
      }
      return out;
    }

    /** Fetches a skill's body by name. */
    public String load(String name) {
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("name", name);
      // The launcher answers {"body": "..."}. Reading "content" yielded an empty string with NO
      // error, so a skill loaded as nothing and the model carried on without it.
      return s(map(send("POST", cfg.memoryBase() + "/skills/load", body)), "body");
    }
  }

  // ── feedback: /feedback ───────────────────────────────────────────────────

  public final class Feedback {
    /** Records a score against a trace — the signal that drives evals and canary promotion. */
    public void score(String traceId, String dimension, double score, String comment) {
      if (!cfg.feedbackWired) {
        throw new NotWiredException("ctxmesh: feedback is not wired (FEEDBACK_PORT unset)");
      }
      Map<String, Object> body = new LinkedHashMap<>();
      // name/value, not dimension/score: the handler decodes those and relays to Langfuse.
      // The wrong keys returned 202 while writing a nameless zero score.
      body.put("traceId", traceId);
      body.put("name", dimension);
      body.put("value", score);
      if (comment != null && !comment.isBlank()) body.put("comment", comment);
      send("POST", cfg.feedbackBase() + "/feedback", body);
    }
  }

  // ── mesh: /amp and /a2a ───────────────────────────────────────────────────

  /**
   * Agent-to-agent calls. /amp is the current surface; /a2a is the retired spelling, still
   * served so agents built against it keep working (ADR 0138).
   */
  public final class Mesh {
    /** Invokes another agent through AMP. */
    public Map<String, Object> call(String targetAgent, Object payload) {
      requireTarget(targetAgent);
      return map(send("POST", cfg.ampBase() + "/amp/" + enc(targetAgent), payload));
    }

    /** Invokes another agent through the retired /a2a path. Prefer {@link #call}. */
    public Map<String, Object> callLegacy(String targetAgent, Object payload) {
      requireTarget(targetAgent);
      return map(send("POST", cfg.ampBase() + "/a2a/" + enc(targetAgent), payload));
    }

    private void requireTarget(String t) {
      if (t == null || t.isBlank()) throw new CtxmeshException("ctxmesh: target agent is required");
    }
  }

  // ── runs: /delegate and /handoff ──────────────────────────────────────────

  /**
   * What /delegate answers. The launcher returns HTTP 200 for EVERY outcome and signals success
   * in {@code ok}, so a refusal decoded as a transport success is silent data loss — {@code
   * answer} is the entire point of delegating.
   */
  public record Delegation(boolean ok, String subAgent, String subRun, String answer,
                           String error, boolean suspend, String endpoint) {}

  /** What /handoff answers, with the same ok-not-status convention. */
  public record Handoff(boolean ok, String runId, String sourceRun, String handedOffTo,
                        String error) {}

  public final class Runs {
    /**
     * Spawns a sub-run on another agent. The platform fences this — spawn depth, total spawns
     * and budget are enforced on its side, so a refusal arrives as DeniedException.
     */
    /**
     * Spawns a sub-run on another agent.
     *
     * <p>{@code step} and {@code callId} are the idempotency key the launcher hard-requires — the
     * supervisor's loop iteration and the model's tool-call id — so a reclaimed supervisor resolves
     * to the SAME sub-run rather than spawning a second. {@code capability} is the run capability;
     * delegation is refused without an authenticated run.
     *
     * <p>The launcher answers 200 for every outcome: check {@link Delegation#ok()}.
     */
    public Delegation delegate(String subAgent, String step, String callId, String capability,
                               Object input) {
      if (subAgent == null || subAgent.isBlank()) {
        throw new CtxmeshException("ctxmesh: sub-agent is required");
      }
      if (step == null || step.isBlank() || callId == null || callId.isBlank()) {
        throw new CtxmeshException("ctxmesh: step and callId are required (the idempotency key)");
      }
      if (capability == null || capability.isBlank()) {
        throw new NotWiredException("ctxmesh: delegation needs the run capability (" + CAPABILITY_HEADER + ")");
      }
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("subAgent", subAgent);
      body.put("input", input);
      body.put("step", step);
      body.put("callId", callId);
      Map<String, Object> m = map(send("POST", cfg.delegateBase() + "/delegate", body,
          Duration.ofSeconds(15), Map.of(CAPABILITY_HEADER, capability)));
      return new Delegation(Boolean.TRUE.equals(m.get("ok")), s(m, "subAgent"), s(m, "subRun"),
          s(m, "answer"), s(m, "error"), Boolean.TRUE.equals(m.get("suspend")), s(m, "endpoint"));
    }

    /**
     * Transfers the conversation to another agent.
     *
     * <p>{@code includeHistory} carries the transcript; the launcher treats an ABSENT field as
     * true, so this always sends it explicitly. {@code message} is the receiver's opening note —
     * without history and without a message it is handed nothing.
     */
    public Handoff handoff(String targetAgent, String capability, String message,
                           boolean includeHistory) {
      if (targetAgent == null || targetAgent.isBlank()) {
        throw new CtxmeshException("ctxmesh: target agent is required");
      }
      if (capability == null || capability.isBlank()) {
        throw new NotWiredException("ctxmesh: handoff needs the run capability (" + CAPABILITY_HEADER + ")");
      }
      Map<String, Object> body = new LinkedHashMap<>();
      body.put("targetAgent", targetAgent);
      body.put("includeHistory", includeHistory);
      if (message != null && !message.isBlank()) body.put("message", message);
      Map<String, Object> m = map(send("POST", cfg.delegateBase() + "/handoff", body,
          Duration.ofSeconds(15), Map.of(CAPABILITY_HEADER, capability)));
      return new Handoff(Boolean.TRUE.equals(m.get("ok")), s(m, "runId"), s(m, "sourceRun"),
          s(m, "handedOffTo"), s(m, "error"));
    }

  }
}
