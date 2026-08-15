/**
 * The `agent` entry module: build a `Client`.
 * Parity with `sdk/python/src/ctxmesh/agent.py`.
 *
 *     import { agent } from "ctxmesh";
 *     const client = agent.fromEnv();     // in-pod: reads the launcher plane
 *
 * `fromEnv()` fails fast with `NotInPodError` when the launcher env is absent.
 * Tests and offline callers use `fromConfig()` with an explicit `PlaneConfig`
 * (e.g. one built by `PlaneConfig.forTest(...)`).
 */

import { Client } from "./client.js";
import { PlaneConfig, type Env } from "./config.js";

/**
 * Build a Client from the launcher-injected env (the in-pod entry point).
 *
 * Reads MEMORY_PORT / FEEDBACK_PORT / MODEL_GATEWAY_URL / AGENT_NAME / … and the
 * fixed discovery port :2999, resolving the localhost plane's base URLs and the run
 * context. Raises NotInPodError when no launcher env is present.
 */
export function fromEnv(env: Env = process.env): Client {
  const config = PlaneConfig.fromEnv(env, { requireLauncher: true });
  return new Client(config);
}

/**
 * Build a Client from an explicit PlaneConfig (tests / offline mode).
 *
 * `config` is typically `PlaneConfig.forTest({...})` pointing at the mock plane.
 */
export function fromConfig(config: PlaneConfig): Client {
  return new Client(config);
}
