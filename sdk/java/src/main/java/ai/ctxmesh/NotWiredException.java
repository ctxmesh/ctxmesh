package ai.ctxmesh;

/**
 * The platform did not grant this capability to this agent.
 *
 * <p>A configuration answer, not a failure: the port is absent because nothing is listening.
 * Distinct from {@link DeniedException}, where the plane understood the call and refused it.
 */
public class NotWiredException extends CtxmeshException {
  public NotWiredException(String message) { super(message); }
}
