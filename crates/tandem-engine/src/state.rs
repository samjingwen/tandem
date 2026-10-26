use std::collections::BTreeSet;

use tandem_core::{
    Attempt, Command, Error, Observation, Result, SessionSpec, State, Task, Timestamp,
};

pub(crate) const NOTIFICATION_BUDGET: &str = "automatic coordination notification budget exhausted; inspect pending decisions or send an explicit follow-up";

pub(crate) fn spec(state: &State, attempt: &Attempt) -> Result<SessionSpec> {
    let task = state
        .tasks
        .get(&attempt.task_id)
        .ok_or_else(|| Error::new("corrupt_state", "attempt task is absent"))?;
    Ok(SessionSpec {
        reference: attempt.session_ref.clone(),
        task_id: task.id.clone(),
        attempt_id: attempt.id.clone(),
        instance_id: state.instance_id.clone(),
        directory: attempt.workspace.path.clone(),
        role: task.role.clone(),
        title: task.title.clone(),
    })
}

pub(crate) fn control_quiescent(observation: &Observation) -> bool {
    observation.exists
        && observation.owned
        && matches!(observation.state.as_str(), "idle" | "failed")
}

pub(crate) fn cleanup_barrier(attempt: Option<&Attempt>) -> bool {
    attempt.is_some_and(|attempt| matches!(attempt.phase.as_str(), "cleanup_pending" | "cleaned"))
}

pub(crate) fn unsettled(command: &Command) -> bool {
    matches!(command.status.as_str(), "pending" | "sending" | "parked")
}

pub(crate) fn pending_prompt(state: &State, attempt: &str) -> bool {
    state.commands.values().any(|command| {
        command.attempt_id == attempt && command.kind == "prompt" && unsettled(command)
    })
}

pub(crate) fn uncertain_dispatch(state: &State, attempt: &str) -> bool {
    state.commands.values().any(|command| {
        command.attempt_id == attempt && command.kind == "prompt" && command.status == "sending"
    }) || state
        .decisions
        .values()
        .any(|decision| decision.attempt_id == attempt && decision.status == "uncertain")
}

pub(crate) fn dispatch_budget_error(state: &State, attempt: &Attempt) -> &'static str {
    if attempt.failure_count < 8
        || state
            .tasks
            .get(&attempt.task_id)
            .is_some_and(|task| task.desired == "cancelled")
    {
        return "";
    }
    if state.commands.values().any(|command| {
        command.attempt_id == attempt.id
            && command.kind == "prompt"
            && matches!(command.status.as_str(), "pending" | "parked")
    }) || state
        .decisions
        .values()
        .any(|decision| decision.attempt_id == attempt.id && decision.status == "reply_pending")
    {
        return "dispatch retry budget exhausted; resume the task to retry pending delivery";
    }
    ""
}

pub(crate) fn consumes_slot(state: &State, attempt: &Attempt) -> bool {
    state
        .tasks
        .get(&attempt.task_id)
        .is_some_and(|task| task.attempt_id == attempt.id)
        && attempt.phase != "cleaned"
        && (uncertain_dispatch(state, &attempt.id)
            || !matches!(attempt.observed.as_str(), "failed" | "idle"))
}

pub(crate) fn capacity_available(state: &State, attempt: &Attempt, concurrency: usize) -> bool {
    let role = state
        .tasks
        .get(&attempt.task_id)
        .map(|task| task.role.as_str())
        .unwrap_or("");
    let used = state
        .attempts
        .values()
        .filter(|other| {
            other.id != attempt.id
                && state
                    .tasks
                    .get(&other.task_id)
                    .is_some_and(|task| task.role == role)
                && consumes_slot(state, other)
        })
        .count();
    used < if role == "coordinator" {
        1
    } else {
        concurrency
    }
}

pub(crate) fn cleanup_eligible(state: &State, attempt: &Attempt) -> bool {
    state
        .tasks
        .get(&attempt.task_id)
        .is_some_and(|task| matches!(task.state.as_str(), "completed" | "cancelled" | "failed"))
        && !pending_prompt(state, &attempt.id)
        && !uncertain_dispatch(state, &attempt.id)
        && !state
            .decisions
            .values()
            .any(|decision| decision.attempt_id == attempt.id && decision.status == "reply_pending")
}

pub(crate) fn descendants(state: &State, id: &str, cascade: bool) -> Result<Vec<String>> {
    let mut ids = vec![id.to_owned()];
    if cascade {
        let mut seen = BTreeSet::from([id.to_owned()]);
        let mut index = 0;
        while index < ids.len() {
            let parent = ids[index].clone();
            for task in state.tasks.values().filter(|task| task.parent_id == parent) {
                if !seen.insert(task.id.clone()) {
                    return Err(Error::new(
                        "corrupt_state",
                        "stored task hierarchy is cyclic",
                    ));
                }
                ids.push(task.id.clone());
            }
            index += 1;
        }
    }
    Ok(ids)
}

pub(crate) fn generation(value: i64) -> Result<i64> {
    value
        .checked_add(1)
        .filter(|value| *value > 0)
        .ok_or_else(|| Error::invalid("instruction generation is exhausted"))
}

pub(crate) fn refresh(
    state: &mut State,
    task_id: &str,
    observed: &str,
    now: &Timestamp,
) -> Result<()> {
    let mut task = state
        .tasks
        .get(task_id)
        .cloned()
        .ok_or_else(|| Error::not_found("task not found"))?;
    if task.desired != "active" || task.state == "completed" {
        return Ok(());
    }
    let mut attempt = state
        .attempts
        .get(&task.attempt_id)
        .cloned()
        .ok_or_else(|| Error::new("corrupt_state", "task attempt is absent"))?;
    let mut status = "running";
    let mut message = String::new();
    let pending = state.decisions.values().any(|decision| {
        decision.attempt_id == attempt.id
            && matches!(decision.status.as_str(), "pending" | "reply_pending")
    });
    if uncertain_dispatch(state, &attempt.id) {
        status = "recovering";
        message = attempt.error.clone();
    } else if !dispatch_budget_error(state, &attempt).is_empty() {
        status = "recovering";
        message = dispatch_budget_error(state, &attempt).into();
        attempt.error = message.clone();
        state.attempts.insert(attempt.id.clone(), attempt.clone());
    } else if observed == "failed" && !pending_prompt(state, &attempt.id) {
        status = if pending {
            "waiting_for_input"
        } else {
            "failed"
        };
        if !pending {
            message = "runtime execution failed".into();
            attempt.phase = "failed".into();
            state.attempts.insert(attempt.id.clone(), attempt.clone());
        }
    } else {
        for decision in state.decisions.values().filter(|decision| {
            decision.attempt_id == attempt.id
                && matches!(decision.status.as_str(), "pending" | "reply_pending")
        }) {
            status = "waiting_for_input";
            if !decision.error.is_empty() {
                message = "decision reply was rejected; submit a corrected answer".into();
            }
        }
        if status == "running"
            && state
                .results
                .get(&format!("{}/{}", attempt.id, task.generation))
                .is_some_and(|result| !result.late)
        {
            status = "ready_for_review";
        }
        if status == "running"
            && state.commands.values().any(|command| {
                command.attempt_id == attempt.id
                    && command.kind == "signal"
                    && command.status == "suppressed"
                    && command.generation == task.generation
            })
        {
            if observed == "idle" {
                status = "waiting_for_input";
            }
            message = NOTIFICATION_BUDGET.into();
        }
    }
    if task.state != status || task.error != message {
        task.state = status.into();
        task.error = message;
        task.updated_at = now.clone();
        state.tasks.insert(task.id.clone(), task);
    }
    Ok(())
}

pub(crate) fn worker_prompt(state: &State, task: &Task, attempt: &Attempt) -> Result<String> {
    let project = state
        .projects
        .get(&task.project_id)
        .ok_or_else(|| Error::new("corrupt_state", "task project is absent"))?;
    let role = if task.role == "coordinator" {
        "You are a Tandem coordinator. Do not edit project source. Use Tandem tools to inspect project tasks, route followups, and create bounded subtasks with explicit parent and dependencies. Never start workers outside Tandem. Ask when project/task routing is ambiguous. Wait for evidence and report outcomes; idleness does not mean completion."
    } else {
        "You are a Tandem worker. Work only in the assigned checkout. Do not spawn unmanaged subagents, merge, publish, delete worktrees, or answer permission requests automatically. Worktrees are not security sandboxes."
    };
    Ok(format!(
        "{role}\nInstance: {}\nTask: {}\nAttempt: {}\nInstruction generation: {}\nProject: {}\nAssigned directory: {}\nBase: {} ({})\n\nProject instructions:\n{}\n\nTask:\n{}\n\nReport explicit results and verification references via Tandem's task result operation, including this instruction generation. Keep secrets out of results and logs.\n",
        state.instance_id,
        task.id,
        attempt.id,
        task.generation,
        project.id,
        attempt.workspace.path,
        attempt.workspace.base_ref,
        attempt.workspace.base_sha,
        project.instructions,
        task.instructions
    ))
}
