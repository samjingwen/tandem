//! Runtime-independent contracts for the Rust migration.
//!
//! This crate has no SQLite, HTTP, executor, or model-provider dependency.
//! Persisted strings remain extensible and operation outcomes retain their raw
//! JSON representation. The Go engine remains the executable until parity is
//! established for scheduling, adapters, HTTP, and CLI as well as persistence.

mod context;
mod input;
mod model;
mod ports;
mod time_compat;

pub use context::Context;
pub use input::*;
pub use model::*;
pub use ports::*;

pub const VERSION: &str = "0.1.0";

/// Typed command/storage error. Internal transport and SQL diagnostics must not
/// be copied into a public error message.
#[derive(Clone, Debug, serde::Serialize, PartialEq, Eq)]
pub struct Error {
    pub code: String,
    pub message: String,
    #[serde(skip)]
    not_delivered: bool,
}

impl Error {
    pub fn new(code: impl Into<String>, message: impl Into<String>) -> Self {
        Self {
            code: code.into(),
            message: message.into(),
            not_delivered: false,
        }
    }

    pub fn invalid(message: impl Into<String>) -> Self {
        Self::new("invalid_request", message)
    }

    pub fn not_found(message: impl Into<String>) -> Self {
        Self::new("not_found", message)
    }

    pub fn uncertain() -> Self {
        Self::new(
            "uncertain",
            "external outcome cannot be established; reconciliation required",
        )
    }

    pub fn not_delivered(mut error: Self) -> Self {
        error.not_delivered = true;
        error
    }

    pub fn is_not_delivered(&self) -> bool {
        self.not_delivered
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.message)
    }
}

impl std::error::Error for Error {}

pub type Result<T> = std::result::Result<T, Error>;

/// A single serialized durable snapshot, including the private operation journal.
/// Updating state and recording an operation receipt must share one transaction.
pub trait Store: Send + Sync {
    fn snapshot(&self) -> Result<State>;
    fn read_revision(&self) -> Result<Revision>;
    fn update(&self, change: &mut dyn FnMut(&mut State) -> Result<()>) -> Result<()>;
}
