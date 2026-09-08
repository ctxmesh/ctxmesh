package ai.ctxmesh;

/** A non-2xx response from the plane. */
public class ApiException extends CtxmeshException {
  private final int status;
  private final String path;
  private final String body;

  public ApiException(int status, String path, String body) {
    super("ctxmesh: " + path + " returned " + status + ": " + body);
    this.status = status;
    this.path = path;
    this.body = body;
  }

  public int status() { return status; }
  public String path() { return path; }
  public String body() { return body; }
}
