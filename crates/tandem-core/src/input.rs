use serde::{Deserialize, Serialize};

use crate::{RawJson, model::optional_raw};

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
pub struct RegisterProject {
    pub name: String,
    pub path: String,
    pub base_ref: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub instructions: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub instruction_files: Vec<String>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
pub struct CreateTask {
    pub project_id: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub parent_id: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pub dependencies: Vec<String>,
    pub title: String,
    pub instructions: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub role: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Followup {
    pub text: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct Control {
    pub action: String,
    pub cascade: bool,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields, rename_all = "camelCase")]
pub struct SubmitResult {
    pub attempt_id: String,
    pub generation: i64,
    pub summary: String,
    pub evidence: Vec<EvidenceInput>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct EvidenceInput {
    pub name: String,
    pub reference: String,
    pub passed: bool,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct ReplyDecision {
    // Missing answer and an explicitly supplied JSON null have different
    // admission semantics, just as Go's nil vs nonempty RawMessage does.
    #[serde(deserialize_with = "optional_raw")]
    pub answer: Option<RawJson>,
}
