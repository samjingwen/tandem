use std::collections::BTreeMap;

use serde::{Deserialize, Deserializer, Serialize};
use serde_json::{Value, value::RawValue};

/// Retains JSON number precision, escapes, property order, and null outcomes.
/// Receipt replay must not replace this with a serialization of a newer record.
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(transparent)]
pub struct RawJson(Box<RawValue>);

impl RawJson {
    pub fn parse(text: String) -> serde_json::Result<Self> {
        RawValue::from_string(text).map(Self)
    }

    pub fn as_str(&self) -> &str {
        self.0.get()
    }
}

impl Default for RawJson {
    fn default() -> Self {
        Self(RawValue::NULL.to_owned())
    }
}

impl PartialEq for RawJson {
    fn eq(&self, other: &Self) -> bool {
        self.as_str() == other.as_str()
    }
}

impl Eq for RawJson {}

// Unlike Option's usual JSON handling, an explicitly supplied null is a retained
// value, not the absence of this optional json.RawMessage field.
fn optional_raw<'de, D: Deserializer<'de>>(
    decoder: D,
) -> std::result::Result<Option<RawJson>, D::Error> {
    RawJson::deserialize(decoder).map(Some)
}

/// Preserves the original RFC3339 spelling and subsecond precision on reopen.
/// Parsing and clock access belong to command admission, not storage rewrites.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(transparent)]
pub struct Timestamp(pub String);

impl Default for Timestamp {
    fn default() -> Self {
        Self("0001-01-01T00:00:00Z".into())
    }
}

pub type ExtraFields = BTreeMap<String, Value>;

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Project {
    pub id: String,
    pub name: String,
    pub path: String,
    pub base_ref: String,
    pub instructions: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub instruction_files: Vec<String>,
    pub created_at: Timestamp,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Task {
    pub id: String,
    pub project_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub parent_id: String,
    // Go's legacy null and non-null [] are distinct wire values.
    pub dependencies: Option<Vec<String>>,
    pub title: String,
    pub instructions: String,
    pub role: String,
    pub desired: String,
    pub state: String,
    pub generation: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub attempt_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    pub created_at: Timestamp,
    pub updated_at: Timestamp,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Workspace {
    pub allocator: String,
    pub owner: String,
    pub path: String,
    pub branch: String,
    pub base_ref: String,
    pub base_sha: String,
    pub allocated: bool,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Attempt {
    pub id: String,
    pub task_id: String,
    pub runtime_id: String,
    pub session_ref: String,
    pub workspace: Workspace,
    pub phase: String,
    pub observed: String,
    pub generation: i64,
    pub failure_count: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    pub created_at: Timestamp,
    pub updated_at: Timestamp,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Command {
    pub id: String,
    pub task_id: String,
    pub attempt_id: String,
    pub kind: String,
    pub text: String,
    pub status: String,
    pub generation: i64,
    pub sequence: i64,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Decision {
    pub id: String,
    pub task_id: String,
    pub attempt_id: String,
    pub runtime_ref: String,
    pub kind: String,
    pub prompt: String,
    #[serde(
        default,
        deserialize_with = "optional_raw",
        skip_serializing_if = "Option::is_none"
    )]
    pub details: Option<RawJson>,
    pub status: String,
    #[serde(
        default,
        deserialize_with = "optional_raw",
        skip_serializing_if = "Option::is_none"
    )]
    pub reply: Option<RawJson>,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Evidence {
    pub name: String,
    pub reference: String,
    pub passed: bool,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct TaskResult {
    pub task_id: String,
    pub attempt_id: String,
    pub generation: i64,
    pub summary: String,
    pub evidence: Option<Vec<Evidence>>,
    pub accepted: bool,
    pub late: bool,
    pub created_at: Timestamp,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct Operation {
    pub id: String,
    pub fingerprint: String,
    pub outcome: RawJson,
    pub created_at: Timestamp,
    #[serde(flatten)]
    pub extra: ExtraFields,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize, PartialEq, Eq)]
#[serde(default, rename_all = "camelCase")]
pub struct State {
    pub instance_id: String,
    pub revision: i64,
    pub projects: BTreeMap<String, Project>,
    pub tasks: BTreeMap<String, Task>,
    pub attempts: BTreeMap<String, Attempt>,
    pub commands: BTreeMap<String, Command>,
    pub decisions: BTreeMap<String, Decision>,
    pub results: BTreeMap<String, TaskResult>,
    // Private idempotency data is never exposed in the State HTTP shape.
    #[serde(skip)]
    pub operations: BTreeMap<String, Operation>,
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase")]
pub struct Revision {
    pub instance_id: String,
    pub revision: i64,
}
