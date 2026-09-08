package ai.ctxmesh;

/**
 * The launcher environment is absent: this process is not running as a ctxmesh agent.
 *
 * <p>Thrown by {@link Client#fromEnv()} rather than guessing ports, so running an agent on a
 * laptop says so plainly instead of failing later with a connection refused to localhost:2998.
 */
public class NotInPodException extends CtxmeshException {
  public NotInPodException(String message) { super(message); }
}
