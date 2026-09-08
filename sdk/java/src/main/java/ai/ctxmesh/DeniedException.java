package ai.ctxmesh;

/**
 * The plane refused the call — a 403. Guardrails, budgets and the delegate fence surface here.
 *
 * <p>Carries the platform's reason: a refusal that says only "403" turns a policy decision into
 * a bare status code.
 */
public class DeniedException extends ApiException {
  public DeniedException(String path, String body) { super(403, path, body); }
}
