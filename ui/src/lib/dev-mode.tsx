import * as React from "react";

// DevModeContext carries whether the console is running under `agentry dev --ui`
// (ADR 0021): a local, single-developer substrate with NO cluster and NO login wall.
// The reduced surface (fleet/providers/topology/RBAC) is served as honest 501s; only
// config-preview + the local run work. The SPA reads this to drop the login gate and
// render dev chrome instead of the cluster login/501-as-error experience.
//
// The value is resolved during SessionProvider's boot (in parallel with the session
// restore, behind the same splash) so it is KNOWN before any auth guard renders — a
// guard must never see a transient `false` and bounce a dev session to /login.
//
// Default false — the SAFE posture. The login wall stays ON unless GET /api/devmode
// explicitly confirms dev mode, so a probe failure on a real cluster keeps auth on.
export const DevModeContext = React.createContext(false);

export function useDevMode(): boolean {
  return React.useContext(DevModeContext);
}
