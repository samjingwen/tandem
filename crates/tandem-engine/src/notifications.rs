use serde::Serialize;
use tandem_core::{Command, Context, Decision, Result, State, Task, TaskResult};

use crate::{
    Engine,
    commands::{missing_attempt, sequence},
    journal,
    state::{NOTIFICATION_BUDGET, generation},
};

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ChildUpdate {
    task_id: String,
    attempt_id: String,
    generation: i64,
    state: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    reported_result: Option<TaskResult>,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    pending_decisions: Vec<Decision>,
}

fn child_update(state: &State, task: &Task) -> Option<ChildUpdate> {
    match task.state.as_str() {
        "ready_for_review" | "completed"
            if state
                .attempts
                .get(&task.attempt_id)
                .is_some_and(|attempt| attempt.observed == "idle") => {}
        "failed" | "waiting_for_input" => (),
        _ => return None,
    }
    let decisions: Vec<_> = state
        .decisions
        .values()
        .filter(|decision| {
            decision.task_id == task.id
                && decision.attempt_id == task.attempt_id
                && decision.status == "pending"
        })
        .cloned()
        .collect();
    if task.state == "waiting_for_input" && decisions.is_empty() {
        return None;
    }
    Some(ChildUpdate {
        task_id: task.id.clone(),
        attempt_id: task.attempt_id.clone(),
        generation: task.generation,
        state: task.state.clone(),
        reported_result: state
            .results
            .get(&format!("{}/{}", task.attempt_id, task.generation))
            .cloned(),
        pending_decisions: decisions,
    })
}

impl Engine {
    pub(crate) async fn notify_coordinators(&self, context: &Context) -> Result<()> {
        let engine = self.clone();
        self.update(context, move |state| {
            let parents: Vec<_> = state.tasks.values().filter(|task| task.role == "coordinator" && task.desired == "active" && task.state != "completed" && !task.attempt_id.is_empty()).cloned().collect();
            for mut parent in parents {
                let mut attempt = state.attempts.get(&parent.attempt_id).cloned().ok_or_else(missing_attempt)?;
                if !matches!(attempt.phase.as_str(), "executing" | "session_bound") { continue; }
                let used = state.commands.values().filter(|command| command.attempt_id == attempt.id && command.kind == "prompt" && command.id.starts_with("coordination_")).count();
                let mut updates = Vec::new(); let mut signals = Vec::new();
                for child in state.tasks.values().filter(|task| task.parent_id == parent.id) {
                    let Some(update) = child_update(state, child) else { continue; };
                    let body = journal::encode(&update)?;
                    let signal = format!("signal_{}", journal::digest_prefix(format!("{}\0{}", attempt.id, child.id).as_bytes()));
                    if state.commands.get(&signal).is_some_and(|command| command.text == body) { continue; }
                    updates.push(update); signals.push((signal, body));
                }
                if updates.is_empty() { continue; }
                parent.generation = generation(parent.generation)?; parent.updated_at = engine.now()?; parent.state = "running".into(); attempt.generation = parent.generation;
                let status = if used >= engine.inner.options.max_coordinator_notifications {
                    parent.state = "waiting_for_input".into(); parent.error = NOTIFICATION_BUDGET.into(); "suppressed"
                } else { "coalesced" };
                for (signal, body) in signals {
                    state.commands.insert(signal.clone(), Command { id: signal, task_id: parent.id.clone(), attempt_id: attempt.id.clone(), kind: "signal".into(), status: status.into(), text: body, generation: parent.generation, sequence: sequence(state.revision)?, ..Default::default() });
                }
                state.tasks.insert(parent.id.clone(), parent.clone()); state.attempts.insert(attempt.id.clone(), attempt.clone());
                if status == "suppressed" { continue; }
                let body = journal::encode(&updates)?;
                let command_id = format!("coordination_{}", journal::digest_prefix(format!("{}/{}/{body}", attempt.id, parent.generation).as_bytes()));
                let text = format!("New coordination evidence for task {}, context generation {}. These are reported child outcomes or actual pending questions, not independent verification. Summarize actionable results or ask for a necessary decision through the existing conversation. Do not recreate workers, auto-approve permissions, merge/publish, or repeatedly call a model/status tool to poll. Submit any updated result with this generation and actual evidence.\n{body}", parent.id, parent.generation);
                state.commands.insert(command_id.clone(), Command { id: command_id, task_id: parent.id, attempt_id: attempt.id, kind: "prompt".into(), status: "pending".into(), text, generation: parent.generation, sequence: sequence(state.revision)?, ..Default::default() });
            }
            Ok(())
        }).await
    }
}
