//! Pinned OpenCode HTTP/SSE adapter. Never discovers or starts a runtime, reads
//! authentication files, retries a mutation, or treats idle as completion.

mod client;
mod policy;
mod runtime;
mod sse;

use serde::{Deserialize, Serialize};
use tandem_core::{Error, Result};

pub use client::OpenCode;

pub const VERSION: &str = "2.1.0";
pub const SOURCE_REVISION: &str = "60c68d145a14127ce633e26aa5fdfb882a7baa6d";

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct Model {
    pub id: String,
    #[serde(rename = "providerID")]
    pub provider_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub variant: String,
}

impl Model {
    /// Parses an explicit operator reference, not provider discovery or proof of
    /// model availability. An empty reference retains the native default.
    pub fn parse(reference: &str) -> Result<Option<Self>> {
        if reference.is_empty() {
            return Ok(None);
        }
        if reference
            .chars()
            .any(|c| c.is_whitespace() || c.is_control())
        {
            return Err(Error::invalid(
                "model reference cannot contain whitespace/control characters",
            ));
        }
        let (name, variant) = match reference.split_once('#') {
            Some((name, variant)) if !variant.is_empty() && !variant.contains('#') => {
                (name, variant)
            }
            Some(_) => {
                return Err(Error::invalid(
                    "model must be provider/model with optional #variant",
                ));
            }
            None => (reference, ""),
        };
        let (provider, model) = name
            .split_once('/')
            .ok_or_else(|| Error::invalid("model must be provider/model with optional #variant"))?;
        if provider.is_empty() || model.is_empty() {
            return Err(Error::invalid(
                "model must be provider/model with optional #variant",
            ));
        }
        Ok(Some(Self {
            id: model.into(),
            provider_id: provider.into(),
            variant: variant.into(),
        }))
    }
}

/// Explicit endpoint/configuration handoff. Deliberately not Debug: passwords
/// must not appear in diagnostic formatting.
pub struct Options {
    pub url: String,
    /// Empty uses the explicitly provided OPENCODE_PASSWORD environment value,
    /// matching the Go reference. No authentication file is ever discovered.
    pub password: String,
    pub model: Option<Model>,
}

fn ownership_error() -> Error {
    Error::new(
        "runtime_ownership",
        "OpenCode ownership or admission collision",
    )
}
