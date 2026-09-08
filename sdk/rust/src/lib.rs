//! Rust SDK for agents running on [ctxmesh](https://ctxmesh.github.io), the Kubernetes-native
//! control plane for AI agents.
//!
//! An agent runs in a pod beside the platform's sidecars, and this crate is the typed way to
//! reach them over localhost: conversation memory, long-term memory, tools, knowledge bases,
//! feedback, agent-to-agent calls, delegation and handoff.
//!
//! It holds no credentials. Endpoints and identity arrive in the environment the platform
//! injects, so there is no API key to manage and no base URL to configure.
//!
//! Conformance tier: **plane-client** (ADR 0139). Every launcher route is reachable here; the
//! managed agent loop and model client are authoring-tier and live in the Python and TypeScript
//! SDKs. Every capability is also a plain HTTP endpoint, so this crate is convenience, never a
//! requirement.

mod config;
mod error;

pub use config::Config;
pub use error::Error;

use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::time::Duration;

/// Stamped from the product tag at release (ADR 0135).
pub const VERSION: &str = "0.1.0-beta.1";

const DEFAULT_TIMEOUT: Duration = Duration::from_secs(15);
/// Search may wait on an embedding call through the token-service.
const SEARCH_TIMEOUT: Duration = Duration::from_secs(60);

/// One conversation turn.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Entry {
    pub role: String,
    pub content: serde_json::Value,
}

/// One long-term memory, with the score its retrieval assigned.
#[derive(Debug, Clone, Deserialize)]
pub struct Fact {
    #[serde(default)]
    pub content: String,
    #[serde(default)]
    pub score: f64,
}

/// One retrieval hit, with the provenance a citation needs.
#[derive(Debug, Clone, Deserialize)]
pub struct Chunk {
    #[serde(default)]
    pub content: String,
    #[serde(rename = "documentRef", default)]
    pub document_ref: String,
    #[serde(rename = "knowledgeBase", default)]
    pub knowledge_base: String,
    #[serde(default)]
    pub score: f64,
}

/// One attached skill.
#[derive(Debug, Clone, Deserialize)]
pub struct Skill {
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub description: String,
}

/// The accepted sub-run.
#[derive(Debug, Clone, Deserialize)]
pub struct Delegation {
    #[serde(rename = "runId", default)]
    pub run_id: String,
    #[serde(default)]
    pub accepted: bool,
}

#[derive(Deserialize)]
struct Results<T> {
    #[serde(default = "Vec::new")]
    results: Vec<T>,
}

#[derive(Deserialize)]
struct SkillList {
    #[serde(default = "Vec::new")]
    skills: Vec<Skill>,
}

#[derive(Deserialize)]
struct SkillBody {
    #[serde(default)]
    content: String,
}

/// The entry point.
pub struct Client {
    cfg: Config,
    agent: ureq::Agent,
}

impl Client {
    /// Reads the launcher environment.
    ///
    /// Returns [`Error::NotInPod`] when it is absent, rather than guessing ports — running an
    /// agent binary on a laptop should say so plainly, not fail later with a connection refused.
    pub fn from_env() -> Result<Self, Error> {
        Ok(Self::with_config(Config::from_env()?))
    }

    /// Builds a client from an explicit config — for tests and offline work.
    pub fn with_config(cfg: Config) -> Self {
        let agent = ureq::AgentBuilder::new().timeout(DEFAULT_TIMEOUT).build();
        Self { cfg, agent }
    }

    pub fn config(&self) -> &Config {
        &self.cfg
    }

    fn send<T: serde::de::DeserializeOwned>(
        &self,
        method: &str,
        url: &str,
        body: Option<serde_json::Value>,
        timeout: Duration,
    ) -> Result<Option<T>, Error> {
        let req = self.agent.request(method, url).timeout(timeout);
        let resp = match body {
            Some(v) => req.send_json(v),
            None => req.call(),
        };
        let resp = match resp {
            Ok(r) => r,
            // ureq models a non-2xx as an error. A 403 is the plane refusing, which a caller
            // must be able to tell from a transport failure.
            Err(ureq::Error::Status(code, r)) => {
                let text = r.into_string().unwrap_or_default();
                let path = path_of(url);
                return Err(if code == 403 {
                    Error::Denied { path, body: text }
                } else {
                    Error::Api { status: code, path, body: text }
                });
            }
            Err(e) => return Err(Error::Transport { path: path_of(url), source: e.to_string() }),
        };
        let text = resp.into_string().map_err(|e| Error::Transport {
            path: path_of(url),
            source: e.to_string(),
        })?;
        if text.trim().is_empty() {
            return Ok(None);
        }
        serde_json::from_str(&text)
            .map(Some)
            .map_err(|e| Error::Decode { path: path_of(url), source: e.to_string() })
    }

    // ── memory: /memory and /memory/agent ────────────────────────────────────

    fn require_memory(&self) -> Result<(), Error> {
        if !self.cfg.memory_wired {
            return Err(Error::NotWired("memory (MEMORY_PORT is unset)".into()));
        }
        Ok(())
    }

    fn require_long_term(&self) -> Result<(), Error> {
        self.require_memory()?;
        if !self.cfg.long_term_enabled {
            return Err(Error::NotWired(
                "long-term memory (MEMORY_LONGTERM_ENABLED is not true)".into(),
            ));
        }
        Ok(())
    }

    /// Explicit id first, else the injected `CONVERSATION_ID`. Empty is an error rather than a
    /// silent write to a shared bucket.
    fn conv(&self, id: Option<&str>) -> Result<String, Error> {
        let v = id
            .filter(|s| !s.trim().is_empty())
            .map(str::to_string)
            .unwrap_or_else(|| self.cfg.conversation_id.clone());
        if v.trim().is_empty() {
            return Err(Error::Invalid(
                "no conversation id (pass one, or CONVERSATION_ID must be set)".into(),
            ));
        }
        if v.contains('/') || v.chars().any(char::is_whitespace) {
            return Err(Error::Invalid(format!(
                "conversation id {v:?} contains a separator or whitespace"
            )));
        }
        Ok(v)
    }

    /// Returns the conversation so far.
    pub fn memory_get(&self, conversation_id: Option<&str>) -> Result<Vec<Entry>, Error> {
        self.require_memory()?;
        let url = format!("{}/memory/{}", self.cfg.memory_base(), self.conv(conversation_id)?);
        Ok(self.send("GET", &url, None, DEFAULT_TIMEOUT)?.unwrap_or_default())
    }

    /// Adds one entry to the conversation.
    pub fn memory_append(&self, entry: &Entry, conversation_id: Option<&str>) -> Result<(), Error> {
        self.require_memory()?;
        let url = format!("{}/memory/{}", self.cfg.memory_base(), self.conv(conversation_id)?);
        let body = serde_json::to_value(entry).map_err(|e| Error::Invalid(e.to_string()))?;
        self.send::<serde_json::Value>("POST", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(())
    }

    /// Replaces the conversation wholesale.
    pub fn memory_put(&self, entries: &[Entry], conversation_id: Option<&str>) -> Result<(), Error> {
        self.require_memory()?;
        let url = format!("{}/memory/{}", self.cfg.memory_base(), self.conv(conversation_id)?);
        let body = serde_json::to_value(entries).map_err(|e| Error::Invalid(e.to_string()))?;
        self.send::<serde_json::Value>("PUT", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(())
    }

    /// Writes a fact to this agent's long-term memory.
    pub fn remember(&self, content: &str, tags: &BTreeMap<String, String>) -> Result<(), Error> {
        self.require_long_term()?;
        let mut body = serde_json::json!({ "content": content });
        if !tags.is_empty() {
            body["tags"] = serde_json::to_value(tags).map_err(|e| Error::Invalid(e.to_string()))?;
        }
        let url = format!("{}/memory/agent", self.cfg.memory_base());
        self.send::<serde_json::Value>("POST", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(())
    }

    /// Retrieves facts from long-term memory; `min_score` drops weak matches.
    pub fn search_agent(&self, query: &str, top_k: usize, min_score: f64) -> Result<Vec<Fact>, Error> {
        self.require_long_term()?;
        let body = serde_json::json!({ "query": query, "topK": if top_k == 0 { 5 } else { top_k } });
        let url = format!("{}/memory/agent/search", self.cfg.memory_base());
        let out: Option<Results<Fact>> = self.send("POST", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(out
            .map(|r| r.results)
            .unwrap_or_default()
            .into_iter()
            .filter(|f| f.score >= min_score)
            .collect())
    }

    // ── knowledge: /knowledge/search ─────────────────────────────────────────

    /// Retrieval over the granted knowledge bases. `knowledge_base` may be `None` for all.
    pub fn knowledge_search(
        &self,
        query: &str,
        knowledge_base: Option<&str>,
        top_k: usize,
    ) -> Result<Vec<Chunk>, Error> {
        if !self.cfg.knowledge_enabled {
            return Err(Error::NotWired(
                "knowledge (KNOWLEDGE_BASE_ENABLED is not true)".into(),
            ));
        }
        let mut body = serde_json::json!({ "query": query, "topK": if top_k == 0 { 5 } else { top_k } });
        if let Some(kb) = knowledge_base.filter(|s| !s.is_empty()) {
            body["knowledgeBase"] = serde_json::Value::String(kb.to_string());
        }
        let url = format!("{}/knowledge/search", self.cfg.memory_base());
        let out: Option<Results<Chunk>> = self.send("POST", &url, Some(body), SEARCH_TIMEOUT)?;
        Ok(out.map(|r| r.results).unwrap_or_default())
    }

    // ── skills: /skills and /skills/load ─────────────────────────────────────

    /// The skills the platform attached to this agent.
    pub fn skills(&self) -> Result<Vec<Skill>, Error> {
        let url = format!("{}/skills", self.cfg.memory_base());
        let out: Option<SkillList> = self.send("GET", &url, None, DEFAULT_TIMEOUT)?;
        Ok(out.map(|s| s.skills).unwrap_or_default())
    }

    /// Fetches a skill's body by name.
    pub fn skill_load(&self, name: &str) -> Result<String, Error> {
        let url = format!("{}/skills/load", self.cfg.memory_base());
        let body = serde_json::json!({ "name": name });
        let out: Option<SkillBody> = self.send("POST", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(out.map(|b| b.content).unwrap_or_default())
    }

    // ── feedback: /feedback ──────────────────────────────────────────────────

    /// Records a score against a trace — the signal that drives evals and canary promotion.
    pub fn feedback(
        &self,
        trace_id: &str,
        dimension: &str,
        score: f64,
        comment: Option<&str>,
    ) -> Result<(), Error> {
        if !self.cfg.feedback_wired {
            return Err(Error::NotWired("feedback (FEEDBACK_PORT is unset)".into()));
        }
        let mut body =
            serde_json::json!({ "traceId": trace_id, "dimension": dimension, "score": score });
        if let Some(c) = comment.filter(|s| !s.is_empty()) {
            body["comment"] = serde_json::Value::String(c.to_string());
        }
        let url = format!("{}/feedback", self.cfg.feedback_base());
        self.send::<serde_json::Value>("POST", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(())
    }

    // ── mesh: /amp and /a2a ──────────────────────────────────────────────────

    /// Invokes another agent through AMP.
    pub fn call_agent(
        &self,
        target_agent: &str,
        payload: serde_json::Value,
    ) -> Result<serde_json::Value, Error> {
        if target_agent.trim().is_empty() {
            return Err(Error::Invalid("target agent is required".into()));
        }
        let url = format!("{}/amp/{}", self.cfg.amp_base(), target_agent);
        Ok(self.send("POST", &url, Some(payload), DEFAULT_TIMEOUT)?.unwrap_or(serde_json::Value::Null))
    }

    /// Invokes another agent through the retired `/a2a` path. Prefer [`Client::call_agent`].
    ///
    /// Kept because the launcher still serves it (ADR 0138), and an SDK that pretends a served
    /// route does not exist is the drift this project's contract gate exists to prevent.
    pub fn call_agent_legacy(
        &self,
        target_agent: &str,
        payload: serde_json::Value,
    ) -> Result<serde_json::Value, Error> {
        if target_agent.trim().is_empty() {
            return Err(Error::Invalid("target agent is required".into()));
        }
        let url = format!("{}/a2a/{}", self.cfg.amp_base(), target_agent);
        Ok(self.send("POST", &url, Some(payload), DEFAULT_TIMEOUT)?.unwrap_or(serde_json::Value::Null))
    }

    // ── runs: /delegate and /handoff ─────────────────────────────────────────

    /// Spawns a sub-run on another agent.
    ///
    /// The platform fences this — spawn depth, total spawns and budget are enforced on its side,
    /// so a refusal arrives as [`Error::Denied`] rather than a silently dropped call.
    pub fn delegate(&self, sub_agent: &str, input: serde_json::Value) -> Result<Delegation, Error> {
        if sub_agent.trim().is_empty() {
            return Err(Error::Invalid("sub-agent is required".into()));
        }
        let body = serde_json::json!({ "subAgent": sub_agent, "input": input });
        let url = format!("{}/delegate", self.cfg.memory_base());
        self.send("POST", &url, Some(body), DEFAULT_TIMEOUT)?
            .ok_or_else(|| Error::Decode { path: "/delegate".into(), source: "empty body".into() })
    }

    /// Transfers the conversation to another agent.
    pub fn handoff(&self, target_agent: &str, include_history: bool) -> Result<(), Error> {
        if target_agent.trim().is_empty() {
            return Err(Error::Invalid("target agent is required".into()));
        }
        let body =
            serde_json::json!({ "targetAgent": target_agent, "includeHistory": include_history });
        let url = format!("{}/handoff", self.cfg.memory_base());
        self.send::<serde_json::Value>("POST", &url, Some(body), DEFAULT_TIMEOUT)?;
        Ok(())
    }
}

/// Keeps the port out of error text: it is an implementation detail, and it makes two otherwise
/// identical failures look different.
fn path_of(url: &str) -> String {
    url.find("//")
        .and_then(|i| url[i + 2..].find('/').map(|j| url[i + 2 + j..].to_string()))
        .unwrap_or_else(|| url.to_string())
}
