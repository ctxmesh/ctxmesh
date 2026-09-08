use std::fmt;

/// The SDK's error vocabulary.
///
/// The variants exist to carry a distinction the platform actually makes: a capability that was
/// never granted is not the same as one that refused, and neither is a transport failure. An
/// agent that must behave differently in each case cannot get that from a single error type.
#[derive(Debug)]
pub enum Error {
    /// The launcher environment is absent: this process is not running as a ctxmesh agent.
    NotInPod(String),
    /// The platform did not grant this capability. The port is absent because nothing is
    /// listening — a configuration answer, not a failure.
    NotWired(String),
    /// A 403. The plane understood the call and refused it; `body` carries its reason.
    Denied { path: String, body: String },
    /// Any other non-2xx.
    Api {
        status: u16,
        path: String,
        body: String,
    },
    /// The request never completed.
    Transport { path: String, source: String },
    /// The response was not what the contract says.
    Decode { path: String, source: String },
    /// The caller's arguments cannot form a valid request.
    Invalid(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Error::NotInPod(m) => write!(f, "ctxmesh: not running in a ctxmesh pod ({m})"),
            Error::NotWired(m) => write!(f, "ctxmesh: capability not wired for this agent: {m}"),
            Error::Denied { path, body } => {
                write!(f, "ctxmesh: {path} denied by the platform: {body}")
            }
            Error::Api { status, path, body } => {
                write!(f, "ctxmesh: {path} returned {status}: {body}")
            }
            Error::Transport { path, source } => write!(f, "ctxmesh: {path}: {source}"),
            Error::Decode { path, source } => {
                write!(f, "ctxmesh: decode response from {path}: {source}")
            }
            Error::Invalid(m) => write!(f, "ctxmesh: {m}"),
        }
    }
}

impl std::error::Error for Error {}
