use std::sync::Arc;

use async_trait::async_trait;
use serde::{Deserialize, Serialize};

use crate::{
    Context, Control, CreateTask, Decision, Followup, Project, RawJson, RegisterProject,
    ReplyDecision, Result, State, SubmitResult, Task, TaskResult, Workspace,
};

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct RuntimeInfo {
    pub id: String,
    pub version: String,
    pub compatible: bool,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub detail: String,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SessionSpec {
    pub reference: String,
    pub task_id: String,
    pub attempt_id: String,
    pub instance_id: String,
    pub directory: String,
    pub role: String,
    pub title: String,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(default, rename_all = "camelCase")]
pub struct Observation {
    pub exists: bool,
    pub owned: bool,
    pub state: String,
    pub decisions: Vec<Decision>,
}

#[derive(Clone, Copy, Debug, Default)]
pub struct RuntimeCapabilities {
    pub reconcile_prompt: bool,
    pub cancel_pending: bool,
    pub quiescence: bool,
}

/// Concrete native writes must recover by the exact reference, positively prove
/// non-delivery, or return uncertainty. Implementations must honor Context.
#[async_trait]
pub trait Runtime: Send + Sync {
    /// Pure deterministic identity planning; must not allocate or contact a runtime.
    fn reference(&self, attempt_id: &str) -> String;
    fn capabilities(&self) -> RuntimeCapabilities {
        RuntimeCapabilities::default()
    }
    async fn info(&self, context: &Context) -> Result<RuntimeInfo>;
    async fn ensure(&self, context: &Context, spec: &SessionSpec) -> Result<()>;
    async fn inspect(&self, context: &Context, spec: &SessionSpec) -> Result<Observation>;
    async fn prompt(
        &self,
        context: &Context,
        spec: &SessionSpec,
        command_id: &str,
        text: &str,
    ) -> Result<()>;
    async fn interrupt(&self, context: &Context, spec: &SessionSpec) -> Result<()>;
    async fn reply(
        &self,
        context: &Context,
        spec: &SessionSpec,
        decision: &Decision,
        answer: &RawJson,
    ) -> Result<()>;
    async fn reconcile_prompt(
        &self,
        _context: &Context,
        _spec: &SessionSpec,
        _command_id: &str,
        _text: &str,
    ) -> Result<()> {
        Err(crate::Error::uncertain())
    }
    async fn cancel_pending(&self, _context: &Context, _spec: &SessionSpec) -> Result<()> {
        Err(crate::Error::invalid(
            "runtime cannot cancel owned pending inputs",
        ))
    }
    async fn check_quiescence(&self, _context: &Context, _spec: &SessionSpec) -> Result<()> {
        Err(crate::Error::invalid(
            "runtime cannot verify background-resource quiescence",
        ))
    }
    async fn events(&self, context: &Context, _notify: Arc<dyn Fn() + Send + Sync>) -> Result<()> {
        context.cancellation_requested().await;
        Ok(())
    }
}

#[async_trait]
pub trait Workspaces: Send + Sync {
    /// Pure plan only. The engine commits ownership before calling ensure().
    fn plan(&self, project: &Project, attempt_id: &str) -> Workspace;
    /// Read-only repository/policy validation, never checkout allocation.
    async fn register(&self, context: &Context, input: &RegisterProject) -> Result<Project>;
    async fn ensure(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<Workspace>;
    async fn inspect(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()>;
    async fn remove(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()>;
}

/// Common operation surface; neither an HTTP handler nor a client owns the
/// scheduler or directly invokes a native workspace/runtime mutation.
#[async_trait]
pub trait Service: Send + Sync {
    async fn snapshot(&self, context: &Context) -> Result<State>;
    async fn runtime_info(&self, context: &Context) -> Result<RuntimeInfo>;
    async fn register(
        &self,
        context: &Context,
        operation: &str,
        input: RegisterProject,
    ) -> Result<Project>;
    async fn create(&self, context: &Context, operation: &str, input: CreateTask) -> Result<Task>;
    async fn followup(
        &self,
        context: &Context,
        operation: &str,
        id: &str,
        input: Followup,
    ) -> Result<Task>;
    async fn control(
        &self,
        context: &Context,
        operation: &str,
        id: &str,
        input: Control,
    ) -> Result<Task>;
    async fn reply(
        &self,
        context: &Context,
        operation: &str,
        id: &str,
        input: ReplyDecision,
    ) -> Result<Decision>;
    async fn submit_result(
        &self,
        context: &Context,
        operation: &str,
        id: &str,
        input: SubmitResult,
    ) -> Result<TaskResult>;
    async fn accept(&self, context: &Context, operation: &str, id: &str) -> Result<Task>;
    async fn cleanup(
        &self,
        context: &Context,
        operation: &str,
        attempt_id: &str,
    ) -> Result<Workspace>;
}
