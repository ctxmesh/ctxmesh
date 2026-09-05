/**
 * Skills — the agent's attached procedural knowledge (ADR 0137).
 *
 * A SKILL is procedural knowledge with progressive disclosure. Its name and description sit in
 * the model's context permanently and cost almost nothing; the BODY is fetched only when the
 * model judges the task relevant. That is the whole reason skills exist rather than a longer
 * prompt: context is scarce, and ten attached skills should cost ten short lines until one of
 * them is actually needed.
 *
 * So this module deliberately offers two calls, not one:
 *
 *   list()        the always-affordable part — names and descriptions
 *   load(name)    the expensive part, fetched on demand
 *
 * Calling `load` for every skill up front would defeat the design. It exists because an agent
 * that KNOWS it needs a skill should not have to pretend otherwise.
 *
 * Gating:
 *   SKILL_REFS   comma-separated "<name>@sha256:…" — the agent's PINNED skills
 *
 * Every ref is a digest, never an alias. The controller resolves aliases once, at deploy time,
 * and records the digest (AgentDeployment.status.resolvedSkills), so a skill cannot change
 * underneath a running agent — which is what keeps a replay fixture honest.
 *
 * With SKILL_REFS unset every call throws ConfigError rather than returning an empty list:
 * "this agent has no skills" and "skills are not wired here" are different facts.
 */

import { PlaneConfig } from "./config.js";
import { ConfigError } from "./errors.js";

/** Launcher-local skill endpoints (the same :2998 listener as memory and knowledge). */
const SKILL_LIST_PATH = "/skills";
const SKILL_LOAD_PATH = "/skills/load";

/** Fetching a body may resolve a git ref or pull from the object store. */
const SKILL_TIMEOUT_MS = 30_000;

/**
 * One attached skill.
 *
 * `description` is the always-in-context line the model matches against to decide whether the
 * body is worth loading. `digest` is the version's identity — pinned, so two runs reporting the
 * same digest genuinely saw the same content.
 */
export interface Skill {
  name: string;
  digest: string;
  description: string;
}

function refs(): string[] {
  return (process.env.SKILL_REFS ?? "")
    .split(",")
    .map((r) => r.trim())
    .filter((r) => r.length > 0);
}

function requireEnabled(): void {
  if (refs().length === 0) {
    throw new ConfigError(
      "no skills are attached to this agent (SKILL_REFS is unset). " +
        "Attach them with AgentDeployment.spec.skillRefs.",
    );
  }
}

/**
 * The raw pinned refs, `["<name>@sha256:…", …]`.
 *
 * Cheap and offline — it reads the injected env and makes no call. Useful for a trace attribute
 * or an assertion that a run used the version you expected.
 */
export function attached(): string[] {
  return refs();
}

/** The client bound to a plane config. */
export class SkillsClient {
  constructor(private readonly config: PlaneConfig) {}

  /**
   * Names and descriptions of every attached skill — NOT their bodies.
   *
   * This is the call an agent makes on every run. It stays affordable no matter how many
   * skills are attached, which is the property that makes progressive disclosure work.
   */
  async list(): Promise<Skill[]> {
    requireEnabled();
    const res = await fetch(`${this.config.memoryBaseUrl}${SKILL_LIST_PATH}`, {
      method: "GET",
      signal: AbortSignal.timeout(SKILL_TIMEOUT_MS),
    });
    if (!res.ok) {
      throw new ConfigError(`skill list failed (${res.status})`);
    }
    const data = (await res.json()) as { skills?: Skill[] };
    return data.skills ?? [];
  }

  /**
   * Fetch one skill's BODY, by name.
   *
   * Call this when the model has decided a skill applies — not up front for everything. The
   * launcher resolves the name against the agent's pinned refs, so a body that is not attached
   * cannot be fetched however the name is spelled.
   */
  async load(name: string): Promise<string> {
    requireEnabled();
    if (!name.trim()) {
      throw new ConfigError("load() needs a skill name");
    }
    const res = await fetch(`${this.config.memoryBaseUrl}${SKILL_LOAD_PATH}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: name.trim() }),
      signal: AbortSignal.timeout(SKILL_TIMEOUT_MS),
    });
    if (!res.ok) {
      throw new ConfigError(`skill load failed (${res.status})`);
    }
    const data = (await res.json()) as { body?: string };
    return data.body ?? "";
  }
}
