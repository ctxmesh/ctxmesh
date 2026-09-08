package ai.ctxmesh;

import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * A minimal JSON reader/writer.
 *
 * <p>Deliberately hand-rolled rather than depending on Jackson or Gson. This is an SDK: it is
 * installed alongside an application that very likely already has one of those, at a version it
 * chose. A hard dependency here becomes a conflict there, and the plane's payloads are small
 * enough that the trade is not close.
 *
 * <p>Not a general-purpose parser — it covers what the launcher actually sends.
 */
final class Json {
  private Json() {}

  static String write(Object v) {
    StringBuilder sb = new StringBuilder();
    writeValue(sb, v);
    return sb.toString();
  }

  private static void writeValue(StringBuilder sb, Object v) {
    if (v == null) { sb.append("null"); return; }
    if (v instanceof String s) { writeString(sb, s); return; }
    if (v instanceof Number || v instanceof Boolean) { sb.append(v); return; }
    if (v instanceof Map<?, ?> m) {
      sb.append('{');
      boolean first = true;
      for (Map.Entry<?, ?> e : m.entrySet()) {
        if (!first) sb.append(',');
        first = false;
        writeString(sb, String.valueOf(e.getKey()));
        sb.append(':');
        writeValue(sb, e.getValue());
      }
      sb.append('}');
      return;
    }
    if (v instanceof Iterable<?> it) {
      sb.append('[');
      boolean first = true;
      for (Object o : it) {
        if (!first) sb.append(',');
        first = false;
        writeValue(sb, o);
      }
      sb.append(']');
      return;
    }
    writeString(sb, String.valueOf(v));
  }

  private static void writeString(StringBuilder sb, String s) {
    sb.append('"');
    for (int i = 0; i < s.length(); i++) {
      char c = s.charAt(i);
      switch (c) {
        case '"' -> sb.append("\\\"");
        case '\\' -> sb.append("\\\\");
        case '\n' -> sb.append("\\n");
        case '\r' -> sb.append("\\r");
        case '\t' -> sb.append("\\t");
        default -> {
          if (c < 0x20) sb.append(String.format("\\u%04x", (int) c));
          else sb.append(c);
        }
      }
    }
    sb.append('"');
  }

  static Object read(String s) {
    Parser p = new Parser(s);
    p.ws();
    Object v = p.value();
    p.ws();
    return v;
  }

  private static final class Parser {
    private final String s;
    private int i;

    Parser(String s) { this.s = s; }

    void ws() { while (i < s.length() && Character.isWhitespace(s.charAt(i))) i++; }

    Object value() {
      if (i >= s.length()) throw new IllegalArgumentException("ctxmesh: truncated JSON");
      char c = s.charAt(i);
      return switch (c) {
        case '{' -> object();
        case '[' -> array();
        case '"' -> string();
        case 't' -> literal("true", Boolean.TRUE);
        case 'f' -> literal("false", Boolean.FALSE);
        case 'n' -> literal("null", null);
        default -> number();
      };
    }

    Map<String, Object> object() {
      Map<String, Object> m = new LinkedHashMap<>();
      i++; ws();
      if (i < s.length() && s.charAt(i) == '}') { i++; return m; }
      while (true) {
        ws();
        String k = string();
        ws();
        expect(':');
        ws();
        m.put(k, value());
        ws();
        if (i < s.length() && s.charAt(i) == ',') { i++; continue; }
        expect('}');
        return m;
      }
    }

    List<Object> array() {
      List<Object> l = new ArrayList<>();
      i++; ws();
      if (i < s.length() && s.charAt(i) == ']') { i++; return l; }
      while (true) {
        ws();
        l.add(value());
        ws();
        if (i < s.length() && s.charAt(i) == ',') { i++; continue; }
        expect(']');
        return l;
      }
    }

    String string() {
      expect('"');
      StringBuilder sb = new StringBuilder();
      while (i < s.length()) {
        char c = s.charAt(i++);
        if (c == '"') return sb.toString();
        if (c != '\\') { sb.append(c); continue; }
        char e = s.charAt(i++);
        switch (e) {
          case 'n' -> sb.append('\n');
          case 't' -> sb.append('\t');
          case 'r' -> sb.append('\r');
          case 'b' -> sb.append('\b');
          case 'f' -> sb.append('\f');
          case 'u' -> { sb.append((char) Integer.parseInt(s.substring(i, i + 4), 16)); i += 4; }
          default -> sb.append(e);
        }
      }
      throw new IllegalArgumentException("ctxmesh: unterminated JSON string");
    }

    Object number() {
      int start = i;
      while (i < s.length() && "+-0123456789.eE".indexOf(s.charAt(i)) >= 0) i++;
      String n = s.substring(start, i);
      if (n.isEmpty()) throw new IllegalArgumentException("ctxmesh: not JSON at offset " + start);
      if (n.contains(".") || n.contains("e") || n.contains("E")) return Double.valueOf(n);
      return Long.valueOf(n);
    }

    Object literal(String want, Object v) {
      if (!s.startsWith(want, i)) throw new IllegalArgumentException("ctxmesh: bad JSON literal at " + i);
      i += want.length();
      return v;
    }

    void expect(char c) {
      if (i >= s.length() || s.charAt(i) != c) {
        throw new IllegalArgumentException("ctxmesh: expected '" + c + "' at offset " + i);
      }
      i++;
    }
  }
}
