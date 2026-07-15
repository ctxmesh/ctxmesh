import type { CatalogTool } from "@/lib/api";

// CURATED_GROUP is the group label for catalog tools with no MCP-server registry
// (curated ToolRegistry entries). It sorts LAST after the named server groups.
export const CURATED_GROUP = "Curated tools";

// groupToolsByServer groups catalog tools by their MCP server (the `registry` field,
// e.g. "scalekit-mcp-server") — NOT `source`, which is only the origin class
// ("user-added" | "curated"). Returns [server, tools] pairs sorted alphabetically with
// curated tools (no registry) last. Shared by the Tool catalog (m25 S11) and the
// create-agent tool picker (m25 S13) so both group by server identically.
export function groupToolsByServer(tools: CatalogTool[]): [string, CatalogTool[]][] {
  const groups = new Map<string, CatalogTool[]>();
  for (const t of tools) {
    const key = t.registry && t.registry.trim() ? t.registry.trim() : CURATED_GROUP;
    const arr = groups.get(key);
    if (arr) {
      arr.push(t);
    } else {
      groups.set(key, [t]);
    }
  }
  return [...groups.entries()].sort((a, b) => {
    if (a[0] === CURATED_GROUP) return 1;
    if (b[0] === CURATED_GROUP) return -1;
    return a[0].localeCompare(b[0]);
  });
}
