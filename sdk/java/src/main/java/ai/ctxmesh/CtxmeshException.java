package ai.ctxmesh;

/** Base for everything this SDK throws. */
public class CtxmeshException extends RuntimeException {
  public CtxmeshException(String message) { super(message); }
  public CtxmeshException(String message, Throwable cause) { super(message, cause); }
}
