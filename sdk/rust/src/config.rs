use crate::Error;

const DEFAULT_MEMORY_PORT: u16 = 2998;
const DEFAULT_FEEDBACK_PORT: u16 = 2995;
const DEFAULT_AMP_PORT: u16 = 2997;
const DEFAULT_DELEGATE_PORT: u16 = 2994;

/// The resolved plane for one agent process.
#[derive(Debug, Clone)]
pub struct Config {
    pub memory_port: u16,
    pub feedback_port: u16,
    pub amp_port: u16,
    /// Its OWN listener. /delegate and /handoff on the memory port are a 404.
    pub delegate_port: u16,
    pub agent_name: String,
    pub conversation_id: String,
    /// False when `MEMORY_PORT` was absent: the platform did not grant memory to this agent.
    pub memory_wired: bool,
    pub feedback_wired: bool,
    pub long_term_enabled: bool,
    pub knowledge_enabled: bool,
}

impl Config {
    /// Reads the launcher environment.
    pub fn from_env() -> Result<Self, Error> {
        Self::from_lookup(&|k| std::env::var(k).ok())
    }

    /// Builds a config explicitly — for tests and offline work.
    pub fn for_test(
        memory_port: u16,
        feedback_port: u16,
        amp_port: u16,
        delegate_port: u16,
        conversation_id: &str,
    ) -> Self {
        Self {
            memory_port,
            feedback_port,
            amp_port,
            delegate_port,
            agent_name: String::new(),
            conversation_id: conversation_id.to_string(),
            memory_wired: true,
            feedback_wired: true,
            long_term_enabled: true,
            knowledge_enabled: true,
        }
    }

    pub(crate) fn from_lookup(look: &dyn Fn(&str) -> Option<String>) -> Result<Self, Error> {
        let in_pod = [
            "MEMORY_PORT",
            "FEEDBACK_PORT",
            "AGENT_NAME",
            "MODEL_GATEWAY_URL",
        ]
        .iter()
        .any(|k| look(k).is_some());
        if !in_pod {
            return Err(Error::NotInPod("no launcher environment".into()));
        }
        let (memory_port, memory_wired) = port(look, "MEMORY_PORT", DEFAULT_MEMORY_PORT)?;
        let (feedback_port, feedback_wired) = port(look, "FEEDBACK_PORT", DEFAULT_FEEDBACK_PORT)?;
        // A2A_PORT is what the launcher publishes; AMP_PORT was invented.
        let (amp_port, _) = port(look, "A2A_PORT", DEFAULT_AMP_PORT)?;
        let (delegate_port, _) = port(look, "DELEGATE_PORT", DEFAULT_DELEGATE_PORT)?;
        let s = |k: &str| look(k).unwrap_or_default().trim().to_string();
        Ok(Self {
            memory_port,
            feedback_port,
            amp_port,
            delegate_port,
            agent_name: s("AGENT_NAME"),
            conversation_id: s("CONVERSATION_ID"),
            memory_wired,
            feedback_wired,
            long_term_enabled: s("MEMORY_LONGTERM_ENABLED") == "true",
            knowledge_enabled: s("KNOWLEDGE_BASE_ENABLED") == "true",
        })
    }

    pub(crate) fn memory_base(&self) -> String {
        format!("http://127.0.0.1:{}", self.memory_port)
    }
    pub(crate) fn feedback_base(&self) -> String {
        format!("http://127.0.0.1:{}", self.feedback_port)
    }
    pub(crate) fn amp_base(&self) -> String {
        format!("http://127.0.0.1:{}", self.amp_port)
    }
    pub(crate) fn delegate_base(&self) -> String {
        format!("http://127.0.0.1:{}", self.delegate_port)
    }
}

/// Returns `(value, was_explicitly_set)`. The caller needs the difference: an unset port means
/// the capability is not wired, not that it is on the default.
fn port(
    look: &dyn Fn(&str) -> Option<String>,
    name: &str,
    default: u16,
) -> Result<(u16, bool), Error> {
    match look(name) {
        None => Ok((default, false)),
        Some(raw) if raw.trim().is_empty() => Ok((default, false)),
        Some(raw) => raw
            .trim()
            .parse::<u16>()
            .ok()
            .filter(|p| *p >= 1)
            .map(|p| (p, true))
            .ok_or_else(|| Error::NotInPod(format!("{name}={raw:?} is not a valid port"))),
    }
}
