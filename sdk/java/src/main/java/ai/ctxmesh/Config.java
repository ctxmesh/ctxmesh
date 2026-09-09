package ai.ctxmesh;

import java.util.Map;
import java.util.function.Function;

/** The resolved plane for one agent process. */
public final class Config {
  static final int DEFAULT_MEMORY_PORT = 2998;
  static final int DEFAULT_FEEDBACK_PORT = 2995;
  static final int DEFAULT_AMP_PORT = 2997;
  static final int DEFAULT_DELEGATE_PORT = 2994;

  public final int memoryPort;
  public final int feedbackPort;
  public final int ampPort;
  /** Its OWN listener. /delegate and /handoff on the memory port are a 404. */
  public final int delegatePort;
  public final String agentName;
  public final String conversationId;

  /**
   * False when MEMORY_PORT was absent. Calling a memory method then throws NotWiredException
   * rather than dialling a port nothing is listening on.
   */
  public final boolean memoryWired;
  public final boolean feedbackWired;
  public final boolean longTermEnabled;
  public final boolean knowledgeEnabled;

  Config(int memoryPort, int feedbackPort, int ampPort, int delegatePort, String agentName,
         String conversationId, boolean memoryWired, boolean feedbackWired,
         boolean longTermEnabled, boolean knowledgeEnabled) {
    this.memoryPort = memoryPort;
    this.feedbackPort = feedbackPort;
    this.ampPort = ampPort;
    this.delegatePort = delegatePort;
    this.agentName = agentName;
    this.conversationId = conversationId;
    this.memoryWired = memoryWired;
    this.feedbackWired = feedbackWired;
    this.longTermEnabled = longTermEnabled;
    this.knowledgeEnabled = knowledgeEnabled;
  }

  /** Builds a Config explicitly — for tests and offline work. */
  public static Config of(int memoryPort, int feedbackPort, int ampPort, int delegatePort,
                          String conversationId) {
    return new Config(memoryPort, feedbackPort, ampPort, delegatePort, "", conversationId,
        true, true, true, true);
  }

  static Config fromEnv(Function<String, String> look) {
    boolean inPod = false;
    for (String marker : new String[] {"MEMORY_PORT", "FEEDBACK_PORT", "AGENT_NAME", "MODEL_GATEWAY_URL"}) {
      if (look.apply(marker) != null) { inPod = true; break; }
    }
    if (!inPod) {
      throw new NotInPodException("ctxmesh: not running in a ctxmesh pod (no launcher environment)");
    }
    int[] mem = port(look, "MEMORY_PORT", DEFAULT_MEMORY_PORT);
    int[] fb = port(look, "FEEDBACK_PORT", DEFAULT_FEEDBACK_PORT);
    // A2A_PORT is what the launcher publishes; AMP_PORT was invented.
    int[] amp = port(look, "A2A_PORT", DEFAULT_AMP_PORT);
    int[] del = port(look, "DELEGATE_PORT", DEFAULT_DELEGATE_PORT);
    return new Config(mem[0], fb[0], amp[0], del[0],
        str(look, "AGENT_NAME"), str(look, "CONVERSATION_ID"),
        mem[1] == 1, fb[1] == 1,
        "true".equals(str(look, "MEMORY_LONGTERM_ENABLED")),
        "true".equals(str(look, "KNOWLEDGE_BASE_ENABLED")));
  }

  private static String str(Function<String, String> look, String k) {
    String v = look.apply(k);
    return v == null ? "" : v.trim();
  }

  /**
   * Returns {value, wasExplicitlySet}. The caller needs the difference: an unset port means the
   * capability is not wired, not that it is on the default.
   */
  private static int[] port(Function<String, String> look, String name, int def) {
    String raw = look.apply(name);
    if (raw == null || raw.isBlank()) return new int[] {def, 0};
    int n;
    try {
      n = Integer.parseInt(raw.trim());
    } catch (NumberFormatException e) {
      throw new NotInPodException("ctxmesh: " + name + "=" + raw + " is not a port");
    }
    if (n < 1 || n > 65535) {
      throw new NotInPodException("ctxmesh: " + name + "=" + n + " out of range (1..65535)");
    }
    return new int[] {n, 1};
  }

  String memoryBase() { return "http://127.0.0.1:" + memoryPort; }
  String feedbackBase() { return "http://127.0.0.1:" + feedbackPort; }
  String ampBase() { return "http://127.0.0.1:" + ampPort; }
  String delegateBase() { return "http://127.0.0.1:" + delegatePort; }

  static Config fromSystemEnv() {
    Map<String, String> env = System.getenv();
    return fromEnv(env::get);
  }
}
