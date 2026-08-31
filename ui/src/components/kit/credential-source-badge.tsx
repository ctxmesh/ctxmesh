import { Badge } from "@/components/ui/badge";

// CredentialSourceBadge (m76.2 T8) — ONE shared badge for the credentialSource
// field with human labels. Used on both mcp-servers-page and the Gallery MCP tab.
//
//   byo-oauth → "You connect your account"
//   shared    → "Uses a shared credential"
//   none / absent → hidden (render nothing)
//
// Always the `open` Tag variant (M151 §5.6): this chip DECLARES a capability, it does not
// report a state, so it may never carry a semantic hue (ok/warn/crit/hold) or the pine brand.

interface CredentialSourceBadgeProps {
  credentialSource: string | undefined;
  name?: string; // used for data-testid
}

export function CredentialSourceBadge({ credentialSource, name }: CredentialSourceBadgeProps) {
  if (!credentialSource || credentialSource === "none") return null;

  const label =
    credentialSource === "byo-oauth"
      ? "You connect your account"
      : credentialSource === "shared"
      ? "Uses a shared credential"
      : credentialSource; // fallback: raw value

  return (
    <Badge
      variant="open"
      data-testid={name ? `cred-source-${name}` : undefined}
    >
      {label}
    </Badge>
  );
}
