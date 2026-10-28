use std::collections::{BTreeSet, VecDeque};
use std::sync::atomic::Ordering;

use async_trait::async_trait;
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use tandem_core::{
    Context, Control, CreateTask, Decision, Error, Evidence, Followup, Project, RegisterProject,
    ReplyDecision, Result, RuntimeInfo, Service, State, SubmitResult, Task, TaskResult, Workspace,
};

use crate::{Engine, id, journal, operation_conflict, read, state::*, workspace_read};

#[derive(Serialize)]
pub(crate) struct Empty {}

fn replay<T: DeserializeOwned, I: Serialize>(
    state: &State,
    operation: &str,
    kind: &str,
    input: &I,
) -> Result<Option<T>> {
    let Some(old) = state.operations.get(operation) else {
        return Ok(None);
    };
    if old.fingerprint != journal::fingerprint(kind, input)? {
        return Err(operation_conflict());
    }
    serde_json::from_str(old.outcome.as_str())
        .map(Some)
        .map_err(|_| Error::new("corrupt_state", "stored operation outcome is invalid"))
}

#[async_trait]
impl Service for Engine {
    async fn snapshot(&self, context: &Context) -> Result<State> {
        Engine::snapshot(self, context).await
    }

    async fn runtime_info(&self, context: &Context) -> Result<RuntimeInfo> {
        let mut info = read(context, self.inner.runtime.info(context)).await.map_err(|_| Error::new("runtime_unavailable", "execution runtime metadata unavailable; check the configured endpoint, authorization and supported version"))?;
        if !self.inner.reconciled.load(Ordering::Acquire) {
            info.compatible = false;
            info.detail = "startup/runtime reconciliation is pending".into();
        }
        Ok(info)
    }

    async fn register(
        &self,
        context: &Context,
        operation: &str,
        input: RegisterProject,
    ) -> Result<Project> {
        let operation = operation.to_owned();
        self.owned(context, move |engine, context| async move {
            engine.register_command(&context, operation, input).await
        })
        .await
    }

    async fn create(&self, context: &Context, operation: &str, input: CreateTask) -> Result<Task> {
        let operation = operation.to_owned();
        self.owned(context, move |engine, context| async move {
            let owner = engine.clone();
            let request = input.clone();
            engine
                .mutate(
                    &context,
                    operation,
                    "task.create".into(),
                    input,
                    move |state| owner.create_task(state, request),
                )
                .await
        })
        .await
    }

    async fn followup(
        &self,
        context: &Context,
        operation: &str,
        task_id: &str,
        input: Followup,
    ) -> Result<Task> {
        let (operation, task_id) = (operation.to_owned(), task_id.to_owned());
        self.owned(context, move |engine, context| async move {
            engine
                .followup_command(&context, operation, task_id, input)
                .await
        })
        .await
    }

    async fn control(
        &self,
        context: &Context,
        operation: &str,
        task_id: &str,
        input: Control,
    ) -> Result<Task> {
        let (operation, task_id) = (operation.to_owned(), task_id.to_owned());
        self.owned(context, move |engine, context| async move {
            engine
                .control_command(&context, operation, task_id, input)
                .await
        })
        .await
    }

    async fn reply(
        &self,
        context: &Context,
        operation: &str,
        decision_id: &str,
        input: ReplyDecision,
    ) -> Result<Decision> {
        let (operation, decision_id) = (operation.to_owned(), decision_id.to_owned());
        self.owned(context, move |engine, context| async move {
            engine
                .reply_command(&context, operation, decision_id, input)
                .await
        })
        .await
    }

    async fn submit_result(
        &self,
        context: &Context,
        operation: &str,
        task_id: &str,
        input: SubmitResult,
    ) -> Result<TaskResult> {
        let (operation, task_id) = (operation.to_owned(), task_id.to_owned());
        self.owned(context, move |engine, context| async move {
            engine
                .result_command(&context, operation, task_id, input)
                .await
        })
        .await
    }

    async fn accept(&self, context: &Context, operation: &str, task_id: &str) -> Result<Task> {
        let (operation, task_id) = (operation.to_owned(), task_id.to_owned());
        self.owned(context, move |engine, context| async move {
            engine.accept_command(&context, operation, task_id).await
        })
        .await
    }

    async fn cleanup(
        &self,
        context: &Context,
        operation: &str,
        attempt_id: &str,
    ) -> Result<Workspace> {
        let (operation, attempt_id) = (operation.to_owned(), attempt_id.to_owned());
        self.owned(context, move |engine, context| async move {
            engine
                .cleanup_command(&context, operation, attempt_id)
                .await
        })
        .await
    }
}

impl Engine {
    async fn register_command(
        &self,
        context: &Context,
        operation: String,
        input: RegisterProject,
    ) -> Result<Project> {
        let snapshot = self.snapshot(context).await?;
        if let Some(previous) = replay(&snapshot, &operation, "project.register", &input)? {
            return Ok(previous);
        }
        if input.name.trim().is_empty() || input.path.is_empty() || input.base_ref.is_empty() {
            return Err(Error::invalid("name, path and baseRef are required"));
        }
        let mut project =
            workspace_read(context, self.inner.workspaces.register(context, &input)).await?;
        let engine = self.clone();
        self.mutate(
            context,
            operation,
            "project.register".into(),
            input,
            move |state| {
                if let Some(previous) = state.projects.values().find(|old| old.path == project.path)
                {
                    project.id = previous.id.clone();
                    project.created_at = previous.created_at.clone();
                } else {
                    project.id = id("prj_")?;
                    project.created_at = engine.now()?;
                }
                state.projects.insert(project.id.clone(), project.clone());
                Ok(project)
            },
        )
        .await
    }

    fn create_task(&self, state: &mut State, mut input: CreateTask) -> Result<Task> {
        if !state.projects.contains_key(&input.project_id) {
            return Err(Error::not_found("project not found"));
        }
        if input.title.trim().is_empty() || input.instructions.trim().is_empty() {
            return Err(Error::invalid("title and instructions are required"));
        }
        if input.instructions.len() > 128 * 1024 || input.title.len() > 500 {
            return Err(Error::invalid("task exceeds input budget"));
        }
        if state.tasks.len() >= self.inner.options.max_tasks {
            return Err(Error::invalid("instance task bound reached"));
        }
        if input.role.is_empty() {
            input.role = "worker".into();
        }
        if input.role != "worker" && input.role != "coordinator" {
            return Err(Error::invalid("role must be worker or coordinator"));
        }
        if !input.parent_id.is_empty() {
            let parent = state
                .tasks
                .get(&input.parent_id)
                .ok_or_else(|| Error::not_found("parent task not found"))?;
            if parent.project_id != input.project_id {
                return Err(Error::invalid(
                    "subtasks must belong to the parent's project",
                ));
            }
            if parent.desired != "active" || parent.state == "completed" {
                return Err(Error::invalid("parent does not permit new work"));
            }
            if ancestors(state, &input.parent_id)?.len() > self.inner.options.max_depth {
                return Err(Error::invalid("decomposition depth bound reached"));
            }
        }
        for dependency in &input.dependencies {
            if state
                .tasks
                .get(dependency)
                .is_none_or(|task| task.project_id != input.project_id)
            {
                return Err(Error::invalid("dependency must exist in this project"));
            }
        }
        if !input.parent_id.is_empty() {
            let mut seen = BTreeSet::new();
            let mut queue = VecDeque::from(input.dependencies.clone());
            while let Some(dependency) = queue.pop_front() {
                if dependency == input.parent_id {
                    return Err(Error::invalid(
                        "task completion dependencies must be acyclic",
                    ));
                }
                if !seen.insert(dependency.clone()) {
                    continue;
                }
                if let Some(task) = state.tasks.get(&dependency) {
                    queue.extend(task.dependencies.clone().unwrap_or_default());
                }
                queue.extend(
                    state
                        .tasks
                        .values()
                        .filter(|task| task.parent_id == dependency)
                        .map(|task| task.id.clone()),
                );
            }
        }
        let task = Task {
            id: id("tsk_")?,
            project_id: input.project_id,
            parent_id: input.parent_id,
            dependencies: Some(input.dependencies),
            title: input.title,
            instructions: input.instructions,
            role: input.role,
            desired: "active".into(),
            state: "queued".into(),
            generation: 1,
            created_at: self.now()?,
            updated_at: self.now()?,
            ..Default::default()
        };
        state.tasks.insert(task.id.clone(), task.clone());
        Ok(task)
    }

    async fn followup_command(
        &self,
        context: &Context,
        operation: String,
        task_id: String,
        input: Followup,
    ) -> Result<Task> {
        if input.text.trim().is_empty() || input.text.len() > 128 * 1024 {
            return Err(Error::invalid(
                "follow-up text is required and must fit the input budget",
            ));
        }
        let _effects = self.effects_lock(context).await?;
        let text = input.text.clone();
        let engine = self.clone();
        self.mutate(
            context,
            operation,
            format!("task.followup/{task_id}"),
            input,
            move |state| {
                let mut task = task(state, &task_id)?;
                if task.desired != "active" || task.state == "completed" {
                    return Err(Error::invalid(
                        "resume or explicitly reopen the task before following up",
                    ));
                }
                if task.state == "recovering" {
                    return Err(Error::uncertain());
                }
                if cleanup_barrier(state.attempts.get(&task.attempt_id)) {
                    return Err(Error::invalid(
                        "cleanup prevents continuation of this attempt",
                    ));
                }
                task.generation = generation(task.generation)?;
                task.updated_at = engine.now()?;
                task.error.clear();
                task.instructions
                    .push_str(&format!("\n\nFollow-up:\n{text}"));
                if !task.attempt_id.is_empty() {
                    let attempt = state
                        .attempts
                        .get_mut(&task.attempt_id)
                        .ok_or_else(missing_attempt)?;
                    attempt.generation = task.generation;
                    attempt.failure_count = 0;
                    if state
                        .commands
                        .contains_key(&format!("initial_{}", attempt.id))
                    {
                        let command = tandem_core::Command {
                            id: id("cmd_")?,
                            task_id: task.id.clone(),
                            attempt_id: attempt.id.clone(),
                            kind: "prompt".into(),
                            text: format!("Task generation {}.\n{text}", task.generation),
                            status: "pending".into(),
                            generation: task.generation,
                            sequence: sequence(state.revision)?,
                            ..Default::default()
                        };
                        state.commands.insert(command.id.clone(), command);
                        task.state = "running".into();
                    }
                }
                state.tasks.insert(task_id, task.clone());
                Ok(task)
            },
        )
        .await
    }

    async fn control_command(
        &self,
        context: &Context,
        operation: String,
        task_id: String,
        input: Control,
    ) -> Result<Task> {
        let _effects = self.effects_lock(context).await?;
        if input.action == "retry" {
            let snapshot = self.snapshot(context).await?;
            if !snapshot.operations.contains_key(&operation) {
                task(&snapshot, &task_id)?;
                for target in descendants(&snapshot, &task_id, input.cascade)? {
                    let task = task(&snapshot, &target)?;
                    if target != task_id
                        && (task.state == "completed" || task.desired == "cancelled")
                    {
                        continue;
                    }
                    let attempt = snapshot.attempts.get(&task.attempt_id).ok_or_else(|| {
                        Error::invalid("retry requires a conclusively failed, quiescent attempt")
                    })?;
                    if task.state != "failed"
                        || attempt.phase != "failed"
                        || attempt.observed != "failed"
                    {
                        return Err(Error::invalid(
                            "retry requires a conclusively failed, quiescent attempt",
                        ));
                    }
                    if snapshot
                        .commands
                        .values()
                        .any(|command| command.attempt_id == attempt.id && unsettled(command))
                    {
                        return Err(Error::invalid(
                            "pending or uncertain dispatch must reconcile before retry",
                        ));
                    }
                    if snapshot.decisions.values().any(|decision| {
                        decision.attempt_id == attempt.id
                            && matches!(decision.status.as_str(), "reply_pending" | "uncertain")
                    }) {
                        return Err(Error::invalid(
                            "uncertain decision replies must reconcile before retry",
                        ));
                    }
                    let spec = spec(&snapshot, attempt)?;
                    let observation =
                        read(context, self.inner.runtime.inspect(context, &spec)).await?;
                    if !observation.exists
                        || !observation.owned
                        || observation.state != "failed"
                        || !observation.decisions.is_empty()
                    {
                        return Err(Error::invalid(
                            "runtime does not confirm a failed, quiescent attempt without pending decisions",
                        ));
                    }
                    if !self.inner.runtime.capabilities().quiescence {
                        return Err(Error::invalid(
                            "runtime cannot verify failed-attempt quiescence",
                        ));
                    }
                    read(context, self.inner.runtime.check_quiescence(context, &spec)).await?;
                }
            }
        }
        let request = input.clone();
        let engine = self.clone();
        self.mutate(context, operation, format!("task.control/{task_id}"), input, move |state| {
            task(state, &task_id)?;
            if !matches!(request.action.as_str(), "pause" | "resume" | "cancel" | "retry") { return Err(Error::invalid("action must be pause, resume, cancel or retry")); }
            for target in descendants(state, &task_id, request.cascade)? {
                let mut task = task(state, &target)?;
                if matches!(task.state.as_str(), "completed" | "cancelled") || (task.desired == "cancelled" && request.action != "cancel") {
                    if target == task_id { return Err(Error::invalid("terminal task cannot be controlled")); } else { continue; }
                }
                if cleanup_barrier(state.attempts.get(&task.attempt_id)) { return Err(Error::invalid("cleanup prevents controls for this attempt")); }
                task.generation = generation(task.generation)?; task.updated_at = engine.now()?; task.error.clear();
                match request.action.as_str() {
                    "pause" | "cancel" => {
                        task.desired = if request.action == "pause" { "paused" } else { "cancelled" }.into();
                        task.state = if task.attempt_id.is_empty() { task.desired.clone() } else { "recovering".into() };
                    }
                    "resume" => {
                        task.desired = "active".into(); task.state = if task.attempt_id.is_empty() { "queued" } else { "recovering" }.into();
                        if let Some(attempt) = state.attempts.get(&task.attempt_id)
                            && matches!(attempt.phase.as_str(), "executing" | "session_bound" | "failed") {
                                let command = tandem_core::Command { id: id("cmd_")?, task_id: task.id.clone(), attempt_id: attempt.id.clone(), kind: "prompt".into(), text: format!("Resume task {} at generation {}. Reconcile prior artifacts and continue the existing instructions; do not start replacement workers.", task.id, task.generation), status: "pending".into(), generation: task.generation, sequence: sequence(state.revision)?, ..Default::default() };
                                state.commands.insert(command.id.clone(), command);
                        }
                    }
                    "retry" => {
                        let previous = state.attempts.get(&task.attempt_id).ok_or_else(|| Error::invalid("retry requires a conclusively failed attempt"))?;
                        if task.state != "failed" || previous.phase != "failed" || previous.observed != "failed" { return Err(Error::invalid("retry requires a conclusively failed attempt")); }
                        for decision in state.decisions.values_mut().filter(|decision| decision.attempt_id == previous.id && decision.status == "pending") { decision.status = "expired".into(); }
                        task.desired = "active".into(); task.attempt_id.clear(); task.state = "queued".into();
                    }
                    _ => unreachable!("validated control action"),
                }
                if !task.attempt_id.is_empty() {
                    let attempt = state.attempts.get_mut(&task.attempt_id).ok_or_else(missing_attempt)?;
                    attempt.generation = task.generation; attempt.failure_count = 0;
                }
                state.tasks.insert(target, task);
            }
            task(state, &task_id)
        }).await
    }

    async fn reply_command(
        &self,
        context: &Context,
        operation: String,
        decision_id: String,
        input: ReplyDecision,
    ) -> Result<Decision> {
        let _effects = self.effects_lock(context).await?;
        let request = input.clone();
        self.mutate(context, operation, format!("decision.reply/{decision_id}"), input, move |state| {
            let mut decision = state.decisions.get(&decision_id).cloned().ok_or_else(|| Error::not_found("decision not found"))?;
            let mut task = task(state, &decision.task_id)?;
            if task.attempt_id != decision.attempt_id || task.desired != "active" { return Err(Error::invalid("decision is not owned by an active task attempt")); }
            if cleanup_barrier(state.attempts.get(&decision.attempt_id)) { return Err(Error::invalid("cleanup prevents replies for this attempt")); }
            if decision.status != "pending" { return Err(Error::new("conflict", "decision is already answered or being reconciled")); }
            let answer = request.answer.ok_or_else(|| Error::invalid("answer must be valid JSON"))?;
            if decision.kind == "permission" {
                #[derive(Default, Deserialize)]
                #[serde(default, deny_unknown_fields)]
                struct Permission { decision: String, message: Option<String> }
                let valid = serde_json::from_str::<Permission>(answer.as_str()).is_ok_and(|permission| { let _ = permission.message; matches!(permission.decision.as_str(), "once" | "always" | "reject") });
                if !valid { return Err(Error::invalid("permission answer requires decision: once, always or reject, with optional message")); }
            }
            decision.status = "reply_pending".into(); decision.reply = Some(answer); decision.error.clear();
            state.decisions.insert(decision_id, decision.clone()); task.state = "waiting_for_input".into(); state.tasks.insert(task.id.clone(), task);
            Ok(decision)
        }).await
    }

    async fn result_command(
        &self,
        context: &Context,
        operation: String,
        task_id: String,
        input: SubmitResult,
    ) -> Result<TaskResult> {
        let request = input.clone();
        let engine = self.clone();
        self.mutate(context, operation, format!("task.result/{task_id}"), input, move |state| {
            let mut task = task(state, &task_id)?;
            let attempt = state.attempts.get(&request.attempt_id).filter(|attempt| attempt.task_id == task_id).ok_or_else(|| Error::invalid("result attempt does not belong to this task"))?;
            if request.summary.trim().is_empty() || request.evidence.is_empty() { return Err(Error::invalid("summary and explicit evidence are required; idleness is not evidence")); }
            if request.generation < 1 || request.generation > attempt.generation { return Err(Error::invalid("result must identify an issued instruction generation for this attempt")); }
            if request.evidence.iter().any(|evidence| evidence.name.is_empty() || evidence.reference.is_empty()) { return Err(Error::invalid("evidence needs a name and reference")); }
            let result = TaskResult { task_id: task_id.clone(), attempt_id: attempt.id.clone(), generation: request.generation, summary: request.summary, evidence: Some(request.evidence.into_iter().map(|evidence| Evidence { name: evidence.name, reference: evidence.reference, passed: evidence.passed, ..Default::default() }).collect()), late: task.desired != "active" || task.attempt_id != attempt.id || task.generation != request.generation || task.state == "completed" || cleanup_barrier(Some(attempt)), created_at: engine.now()?, ..Default::default() };
            let key = format!("{}/{}", attempt.id, request.generation);
            if state.results.contains_key(&key) { return Err(Error::new("conflict", "this attempt generation already has a result; use its original operation ID to retry")); }
            state.results.insert(key, result.clone());
            if !result.late { task.state = "ready_for_review".into(); task.updated_at = engine.now()?; state.tasks.insert(task_id, task); }
            Ok(result)
        }).await
    }

    async fn accept_command(
        &self,
        context: &Context,
        operation: String,
        task_id: String,
    ) -> Result<Task> {
        let _effects = self.effects_lock(context).await?;
        let snapshot = self.snapshot(context).await?;
        if !snapshot.operations.contains_key(&operation) {
            let task = task(&snapshot, &task_id)?;
            let attempt = snapshot
                .attempts
                .get(&task.attempt_id)
                .ok_or_else(|| Error::invalid("task has no execution attempt"))?;
            let observation = read(
                context,
                self.inner
                    .runtime
                    .inspect(context, &spec(&snapshot, attempt)?),
            )
            .await?;
            if !observation.exists || !observation.owned || observation.state != "idle" {
                return Err(Error::invalid(
                    "runtime must currently confirm owned quiescence before acceptance",
                ));
            }
            let project = snapshot
                .projects
                .get(&task.project_id)
                .ok_or_else(missing_project)?;
            read(
                context,
                self.inner
                    .workspaces
                    .inspect(context, project, &attempt.workspace),
            )
            .await?;
        }
        let engine = self.clone();
        self.mutate(
            context,
            operation,
            format!("task.accept/{task_id}"),
            Empty {},
            move |state| {
                let mut task = task(state, &task_id)?;
                let key = format!("{}/{}", task.attempt_id, task.generation);
                let mut result = state
                    .results
                    .get(&key)
                    .filter(|result| {
                        !result.late && task.state == "ready_for_review" && task.desired == "active"
                    })
                    .cloned()
                    .ok_or_else(|| Error::invalid("task has no current reviewable result"))?;
                let attempt = state
                    .attempts
                    .get(&task.attempt_id)
                    .ok_or_else(missing_attempt)?;
                if attempt.observed != "idle" {
                    return Err(Error::invalid(
                        "execution must be confirmed quiescent before accepting",
                    ));
                }
                if state
                    .commands
                    .values()
                    .any(|command| command.attempt_id == attempt.id && unsettled(command))
                {
                    return Err(Error::invalid(
                        "pending dispatch must settle before acceptance",
                    ));
                }
                if result
                    .evidence
                    .iter()
                    .flatten()
                    .any(|evidence| !evidence.passed)
                {
                    return Err(Error::invalid("failed evidence cannot be accepted"));
                }
                if state.decisions.values().any(|decision| {
                    decision.task_id == task_id
                        && matches!(
                            decision.status.as_str(),
                            "pending" | "reply_pending" | "uncertain"
                        )
                }) {
                    return Err(Error::invalid("resolve pending decisions before accepting"));
                }
                if state
                    .tasks
                    .values()
                    .any(|child| child.parent_id == task_id && child.state != "completed")
                {
                    return Err(Error::invalid(
                        "all subtasks must be completed before parent acceptance",
                    ));
                }
                result.accepted = true;
                state.results.insert(key, result);
                task.state = "completed".into();
                task.updated_at = engine.now()?;
                state.tasks.insert(task_id, task.clone());
                Ok(task)
            },
        )
        .await
    }

    async fn cleanup_command(
        &self,
        context: &Context,
        operation: String,
        attempt_id: String,
    ) -> Result<Workspace> {
        let _effects = self.effects_lock(context).await?;
        let snapshot = self.snapshot(context).await?;
        let attempt = snapshot
            .attempts
            .get(&attempt_id)
            .ok_or_else(|| Error::not_found("attempt not found"))?;
        if let Some(previous) = replay(
            &snapshot,
            &operation,
            &format!("workspace.cleanup/{attempt_id}"),
            &Empty {},
        )? {
            return Ok(previous);
        }
        if operation.is_empty() {
            return Err(Error::invalid("operation ID required"));
        }
        if attempt.phase == "cleaned" {
            return Err(Error::invalid(
                "workspace is already cleaned; retry its original operation ID for its receipt",
            ));
        }
        if !cleanup_eligible(&snapshot, attempt) {
            return Err(Error::invalid(
                "task must be terminal without unsettled dispatch before cleanup",
            ));
        }
        let spec = spec(&snapshot, attempt)?;
        let observation = read(context, self.inner.runtime.inspect(context, &spec)).await?;
        if !control_quiescent(&observation) {
            return Err(Error::invalid(
                "runtime quiescence and ownership must be proven before cleanup",
            ));
        }
        if !self.inner.runtime.capabilities().quiescence {
            return Err(Error::invalid(
                "runtime cannot verify background-resource quiescence; cleanup is unavailable",
            ));
        }
        read(context, self.inner.runtime.check_quiescence(context, &spec)).await?;
        self.mutate(
            context,
            operation,
            format!("workspace.cleanup/{attempt_id}"),
            Empty {},
            move |state| {
                let attempt = state
                    .attempts
                    .get(&attempt_id)
                    .ok_or_else(missing_attempt)?;
                if !cleanup_eligible(state, attempt) {
                    return Err(Error::invalid(
                        "task must be terminal without unsettled dispatch before cleanup",
                    ));
                }
                let command = tandem_core::Command {
                    id: format!("cleanup_{attempt_id}"),
                    task_id: attempt.task_id.clone(),
                    attempt_id: attempt_id.clone(),
                    kind: "cleanup".into(),
                    status: "pending".into(),
                    generation: attempt.generation,
                    sequence: sequence(state.revision)?,
                    ..Default::default()
                };
                state.commands.insert(command.id.clone(), command);
                let attempt = state
                    .attempts
                    .get_mut(&attempt_id)
                    .ok_or_else(missing_attempt)?;
                attempt.phase = "cleanup_pending".into();
                Ok(attempt.workspace.clone())
            },
        )
        .await
    }
}

pub(crate) fn sequence(revision: i64) -> Result<i64> {
    revision
        .checked_add(1)
        .ok_or_else(|| Error::new("corrupt_state", "state revision is exhausted"))
}
pub(crate) fn task(state: &State, id: &str) -> Result<Task> {
    state
        .tasks
        .get(id)
        .cloned()
        .ok_or_else(|| Error::not_found("task not found"))
}
pub(crate) fn missing_attempt() -> Error {
    Error::new("corrupt_state", "task attempt is absent")
}
pub(crate) fn missing_project() -> Error {
    Error::new("corrupt_state", "task project is absent")
}

pub(crate) fn ancestors(state: &State, parent: &str) -> Result<Vec<String>> {
    let mut current = parent;
    let mut parents = Vec::new();
    let mut seen = BTreeSet::new();
    while !current.is_empty() {
        if !seen.insert(current.to_owned()) {
            return Err(Error::new(
                "corrupt_state",
                "stored task hierarchy is cyclic",
            ));
        }
        parents.push(current.to_owned());
        current = &state
            .tasks
            .get(current)
            .ok_or_else(|| Error::new("corrupt_state", "stored task ancestor is absent"))?
            .parent_id;
    }
    Ok(parents)
}
