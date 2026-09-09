package ai.ctxmesh;

import java.util.List;
import java.util.Map;

/** Reuses the SDK's own package-private JSON reader so the fixture parses without a dependency. */
final class TestJson {
  private TestJson() {}
  static Object read(String s) { return Json.read(s); }
  @SuppressWarnings("unchecked")
  static Map<String, Object> obj(Object o) { return (Map<String, Object>) o; }
  @SuppressWarnings("unchecked")
  static List<Object> arr(Object o) { return (List<Object>) o; }
}
