use std::{
    collections::BTreeSet,
    sync::{Arc, atomic::Ordering},
    time::Duration,
};

use tandem_core::{
    Attempt, Command, Context, Decision, Error, Observation, Project, Result, SessionSpec, State,
    Task,
};
use tokio::sync::Notify;

use crate::{
    Engine,
    commands::{ancestors, missing_attempt, missing_project, sequence, task},
    id, journal, read,
    state::*,
    workspace_read, workspace_write, write,
};

impl Engine {
    /// Events are wake hints. The independent cadence reconciles durable state;
    /// neither polling nor shutdown invokes a model or stops runtime workers.
    pub async fn run(&self, context: &Context, interval: Duration) -> Result<()> {
        self.owned(context, move |engine, context| async move {
            engine.run_loop(context, interval).await
        })
        .await
    }

    async fn run_loop(&self, context: Context, interval: Duration) -> Result<()> {
        let interval = if interval.is_zero() {
            Duration::from_secs(1)
        } else {
            interval
        };
        let wake = Arc::new(Notify::new());
        let events_context = context.clone();
        let events_stop = events_context.clone();
        let events_engine = self.clone();
        let events_wake = wake.clone();
        let events = self.inner.tasks.spawn(async move {
            let mut backoff = Duration::from_secs(1);
            let notify: Arc<dyn Fn() + Send + Sync> = Arc::new(move || events_wake.notify_one());
            while events_context.check().is_ok() {
                let _ = read(
                    &events_context,
                    events_engine
                        .inner
                        .runtime
                        .events(&events_context, notify.clone()),
                )
                .await;
                notify();
                if read(&events_context, async {
                    tokio::time::sleep(backoff).await;
                    Ok(())
                })
                .await
                .is_err()
                {
                    break;
                }
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        });
        let result = async {
            loop {
                if context.check().is_err() || self.inner.stop.check().is_err() { return Ok(()); }
                let result = self.tick_inner(&context).await;
                if context.check().is_err() { return result; }
                tokio::select! {
                    () = context.cancellation_requested() => return Ok(()),
                    () = self.inner.stop.cancellation_requested() => return Ok(()),
                    () = crate::deadline(&context) => return Err(Error::new("timeout", "operation deadline exceeded")),
                    () = tokio::time::sleep(interval) => (),
                    () = wake.notified() => (),
                }
            }
        }.await;
        events_stop.cancel();
        events
            .await
            .map_err(|_| Error::new("internal", "runtime event task failed"))?;
        if self.inner.stop.check().is_err() {
            Ok(())
        } else {
            result
        }
    }

    pub async fn tick(&self, context: &Context) -> Result<()> {
        self.owned(context, |engine, context| async move {
            engine.tick_inner(&context).await
        })
        .await
    }

    async fn tick_inner(&self, context: &Context) -> Result<()> {
        let _tick = read(context, async { Ok(self.inner.tick.lock().await) }).await?;
        if !read(context, self.inner.runtime.info(context))
            .await
            .is_ok_and(|info| info.compatible)
        {
            self.inner.reconciled.store(false, Ordering::Release);
            if let Ok(snapshot) = self.snapshot(context).await {
                for attempt in snapshot.attempts.values() {
                    let task = task(&snapshot, &attempt.task_id)?;
                    if task.attempt_id == attempt.id
                        && attempt.phase != "cleaned"
                        && !matches!(task.state.as_str(), "completed" | "cancelled")
                    {
                        self.mark(
                            &attempt.id,
                            "disconnected",
                            Some(Error::new(
                                "runtime_unavailable",
                                "runtime unavailable or incompatible",
                            )),
                        )
                        .await?;
                    }
                }
            }
            return Err(Error::new(
                "runtime_unavailable",
                "runtime compatibility unavailable; dispatch disabled",
            ));
        }
        let snapshot = self.snapshot(context).await?;
        for attempt in snapshot.attempts.values() {
            let task = task(&snapshot, &attempt.task_id)?;
            let terminal = matches!(task.state.as_str(), "completed" | "failed");
            if attempt.phase == "cleanup_pending"
                || (task.attempt_id == attempt.id
                    && attempt.phase != "cleaned"
                    && !(terminal
                        && matches!(attempt.observed.as_str(), "idle" | "failed")
                        && !pending_prompt(&snapshot, &attempt.id)))
            {
                let result = self.reconcile(context, &attempt.id).await;
                if context.check().is_err() {
                    return result;
                }
            }
        }
        loop {
            let engine = self.clone();
            let allocated = self
                .update(context, move |state| engine.allocate(state))
                .await?;
            let Some(attempt) = allocated else {
                break;
            };
            let result = self.reconcile(context, &attempt.id).await;
            if context.check().is_err() {
                return result;
            }
        }
        self.notify_coordinators(context).await?;
        self.inner.reconciled.store(true, Ordering::Release);
        Ok(())
    }

    fn allocate(&self, state: &mut State) -> Result<Option<Attempt>> {
        let mut workers = 0;
        let mut coordinators = 0;
        for attempt in state
            .attempts
            .values()
            .filter(|attempt| consumes_slot(state, attempt))
        {
            if task(state, &attempt.task_id)?.role == "coordinator" {
                coordinators += 1;
            } else {
                workers += 1;
            }
        }
        let mut tasks = state
            .tasks
            .values()
            .filter(|task| {
                task.desired == "active" && task.state == "queued" && task.attempt_id.is_empty()
            })
            .map(|task| Ok((task.created_at.instant()?, task.id.clone())))
            .collect::<Result<Vec<_>>>()?;
        tasks.sort();
        for (_, task_id) in tasks {
            let mut task = task(state, &task_id)?;
            if (task.role == "worker" && workers >= self.inner.options.concurrency)
                || (task.role == "coordinator" && coordinators >= 1)
            {
                continue;
            }
            if task.dependencies.iter().flatten().any(|id| {
                state
                    .tasks
                    .get(id)
                    .is_none_or(|task| task.state != "completed")
            }) {
                continue;
            }
            if !ancestors_active(state, &task)? {
                continue;
            }
            let mut attempt = Attempt {
                id: id("run_")?,
                task_id: task.id.clone(),
                runtime_id: self.inner.options.runtime_id.clone(),
                phase: "workspace_pending".into(),
                observed: "unknown".into(),
                generation: task.generation,
                created_at: self.now()?,
                updated_at: self.now()?,
                ..Default::default()
            };
            // These two ports only PLAN identifiers. Durable ownership precedes
            // either workspace allocation or a native runtime write.
            attempt.session_ref = self.inner.runtime.reference(&attempt.id);
            attempt.workspace = self.inner.workspaces.plan(
                state
                    .projects
                    .get(&task.project_id)
                    .ok_or_else(missing_project)?,
                &attempt.id,
            );
            state.attempts.insert(attempt.id.clone(), attempt.clone());
            task.attempt_id = attempt.id.clone();
            task.state = "dispatching".into();
            state.tasks.insert(task.id.clone(), task);
            return Ok(Some(attempt));
        }
        Ok(None)
    }

    async fn mark(&self, attempt_id: &str, observed: &str, error: Option<Error>) -> Result<()> {
        let attempt_id = attempt_id.to_owned();
        let observed = observed.to_owned();
        let engine = self.clone();
        self.bookkeeping(move |state| {
            let mut attempt = state
                .attempts
                .get(&attempt_id)
                .cloned()
                .ok_or_else(missing_attempt)?;
            let before = attempt.clone();
            attempt.observed = observed.clone();
            if let Some(error) = &error {
                attempt.error = error.message.clone();
                if attempt.failure_count < 8 {
                    attempt.failure_count += 1;
                }
                if attempt.failure_count >= 8
                    && task(state, &attempt.task_id)?.desired == "cancelled"
                {
                    attempt
                        .error
                        .push_str("; cancellation retry budget exhausted; repeat cancel to retry");
                }
            }
            let budget = dispatch_budget_error(state, &attempt);
            if !budget.is_empty() {
                attempt.error = budget.into();
            }
            if attempt.observed != before.observed
                || attempt.error != before.error
                || attempt.failure_count != before.failure_count
            {
                attempt.updated_at = engine.now()?;
            }
            state.attempts.insert(attempt_id, attempt.clone());
            let mut task = task(state, &attempt.task_id)?;
            if (matches!(observed.as_str(), "unknown" | "disconnected")
                || error.is_some_and(|error| error.code == "uncertain"))
                && task.state != "completed"
                && !cleanup_barrier(Some(&attempt))
            {
                if task.state != "recovering" || task.error != attempt.error {
                    task.updated_at = engine.now()?;
                }
                task.state = "recovering".into();
                task.error = attempt.error;
                state.tasks.insert(task.id.clone(), task);
            }
            Ok(())
        })
        .await
    }

    async fn reconcile(&self, context: &Context, attempt_id: &str) -> Result<()> {
        let context = context.with_timeout(Duration::from_secs(15));
        let _effects = self.effects_lock(&context).await?;
        let snapshot = self.snapshot(&context).await?;
        let mut attempt = snapshot
            .attempts
            .get(attempt_id)
            .cloned()
            .ok_or_else(missing_attempt)?;
        let task = task(&snapshot, &attempt.task_id)?;
        let project = snapshot
            .projects
            .get(&task.project_id)
            .ok_or_else(missing_project)?;
        if attempt.phase == "cleanup_pending" {
            return self
                .reconcile_cleanup(&context, &snapshot, attempt, project)
                .await;
        }
        if matches!(
            attempt.phase.as_str(),
            "workspace_pending" | "session_pending"
        ) && (task.desired != "active" || !ancestors_active(&snapshot, &task)?)
        {
            return self
                .bookkeeping(move |state| {
                    let current = state
                        .tasks
                        .get_mut(&task.id)
                        .ok_or_else(|| Error::not_found("task not found"))?;
                    current.state = if current.desired != "active" {
                        current.desired.clone()
                    } else {
                        "queued".into()
                    };
                    state
                        .attempts
                        .get_mut(&attempt.id)
                        .ok_or_else(missing_attempt)?
                        .observed = "idle".into();
                    Ok(())
                })
                .await;
        }
        if attempt.runtime_id != self.inner.options.runtime_id {
            return self
                .mark(
                    attempt_id,
                    "unknown",
                    Some(Error::new(
                        "uncertain",
                        "runtime binding changed; explicit reconciliation required",
                    )),
                )
                .await;
        }
        if matches!(
            attempt.phase.as_str(),
            "workspace_pending" | "session_pending"
        ) && attempt.observed == "idle"
        {
            if !capacity_available(&snapshot, &attempt, self.inner.options.concurrency) {
                return Ok(());
            }
            attempt.observed = "unknown".into();
            let updated = attempt.clone();
            self.update(&context, move |state| {
                state.attempts.insert(updated.id.clone(), updated);
                Ok(())
            })
            .await?;
        }
        if attempt.phase == "workspace_pending" {
            if attempt.failure_count >= 8 {
                return Ok(());
            }
            let result = workspace_write(
                &context,
                self.inner
                    .workspaces
                    .ensure(&context, project, &attempt.workspace),
            )
            .await;
            match result {
                Ok(workspace) => attempt.workspace = workspace,
                Err(error) => return self.mark(attempt_id, "unknown", Some(error)).await,
            }
            attempt.phase = "session_pending".into();
            let updated = attempt.clone();
            self.bookkeeping(move |state| {
                state.attempts.insert(updated.id.clone(), updated);
                Ok(())
            })
            .await?;
        }
        if let Err(error) = workspace_read(
            &context,
            self.inner
                .workspaces
                .inspect(&context, project, &attempt.workspace),
        )
        .await
        {
            return self.mark(attempt_id, "unknown", Some(error)).await;
        }
        let spec = spec(&snapshot, &attempt)?;
        if attempt.phase == "session_pending" {
            if attempt.failure_count >= 8 {
                if !read(&context, self.inner.runtime.inspect(&context, &spec))
                    .await
                    .is_ok_and(|observation| observation.exists && observation.owned)
                {
                    return Ok(());
                }
            } else if let Err(error) =
                write(&context, || self.inner.runtime.ensure(&context, &spec)).await
            {
                return self.mark(attempt_id, "unknown", Some(error)).await;
            }
            attempt.phase = "session_bound".into();
            attempt.failure_count = 0;
            let updated = attempt.clone();
            let owner_task = task.clone();
            self.bookkeeping(move |state| {
                let command = Command {
                    id: format!("initial_{}", updated.id),
                    task_id: owner_task.id.clone(),
                    attempt_id: updated.id.clone(),
                    kind: "prompt".into(),
                    text: worker_prompt(state, &owner_task, &updated)?,
                    status: "pending".into(),
                    generation: owner_task.generation,
                    sequence: sequence(state.revision)?,
                    ..Default::default()
                };
                state.attempts.insert(updated.id.clone(), updated);
                state.commands.insert(command.id.clone(), command);
                Ok(())
            })
            .await?;
        }
        let observation = match read(&context, self.inner.runtime.inspect(&context, &spec)).await {
            Ok(observation) => observation,
            Err(error) => return self.mark(attempt_id, "disconnected", Some(error)).await,
        };
        if !observation.exists || !observation.owned {
            return self
                .mark(
                    attempt_id,
                    "unknown",
                    Some(Error::new(
                        "uncertain",
                        "session missing or ownership mismatch; no replacement dispatched",
                    )),
                )
                .await;
        }
        if matches!(observation.state.as_str(), "unknown" | "disconnected") {
            return self
                .mark(attempt_id, &observation.state, Some(Error::uncertain()))
                .await;
        }
        self.record_observation(attempt_id, &observation).await?;
        if task.desired != "active" {
            return self
                .settle_control(&context, &snapshot, &spec, attempt, task, observation)
                .await;
        }
        self.deliver(&context, &spec, &task, &mut attempt, &observation)
            .await
    }

    async fn reconcile_cleanup(
        &self,
        context: &Context,
        state: &State,
        attempt: Attempt,
        project: &Project,
    ) -> Result<()> {
        if !cleanup_eligible(state, &attempt) {
            return Err(Error::invalid(
                "cleanup requires a terminal task without unsettled dispatch",
            ));
        }
        let spec = spec(state, &attempt)?;
        let result = async {
            let observation = read(context, self.inner.runtime.inspect(context, &spec)).await?;
            if !control_quiescent(&observation) {
                return Err(Error::invalid(
                    "cleanup waits for confirmed runtime ownership and quiescence",
                ));
            }
            if !self.inner.runtime.capabilities().quiescence {
                return Err(Error::invalid(
                    "runtime cannot verify background-resource quiescence",
                ));
            }
            read(context, self.inner.runtime.check_quiescence(context, &spec)).await?;
            workspace_write(
                context,
                self.inner
                    .workspaces
                    .remove(context, project, &attempt.workspace),
            )
            .await
        }
        .await;
        self.bookkeeping(move |state| {
            let current = state
                .attempts
                .get_mut(&attempt.id)
                .ok_or_else(missing_attempt)?;
            if let Err(error) = result
                && error.code != "workspace_missing"
            {
                current.error = error.message;
                return Ok(());
            }
            current.workspace.allocated = false;
            current.phase = "cleaned".into();
            current.error.clear();
            state
                .commands
                .get_mut(&format!("cleanup_{}", attempt.id))
                .ok_or_else(|| Error::new("corrupt_state", "cleanup command is absent"))?
                .status = "completed".into();
            Ok(())
        })
        .await
    }

    async fn record_observation(&self, attempt_id: &str, observation: &Observation) -> Result<()> {
        let attempt_id = attempt_id.to_owned();
        let observation = observation.clone();
        let engine = self.clone();
        self.bookkeeping(move |state| {
            let mut attempt = state
                .attempts
                .get(&attempt_id)
                .cloned()
                .ok_or_else(missing_attempt)?;
            let clear = !uncertain_dispatch(state, &attempt_id)
                && dispatch_budget_error(state, &attempt).is_empty()
                && !(task(state, &attempt.task_id)?.desired == "cancelled"
                    && attempt.failure_count >= 8);
            if attempt.observed != observation.state || (!attempt.error.is_empty() && clear) {
                attempt.updated_at = engine.now()?;
            }
            attempt.observed = observation.state.clone();
            if clear {
                attempt.error.clear();
            }
            state.attempts.insert(attempt_id.clone(), attempt.clone());
            let mut seen = BTreeSet::new();
            for mut decision in observation.decisions {
                decision.id = format!(
                    "dec_{}",
                    journal::digest_prefix(
                        format!("{attempt_id}\0{}\0{}", decision.kind, decision.runtime_ref)
                            .as_bytes()
                    )
                );
                decision.task_id = attempt.task_id.clone();
                decision.attempt_id = attempt_id.clone();
                seen.insert(decision.id.clone());
                if let Some(old) = state.decisions.get(&decision.id) {
                    decision.status = old.status.clone();
                    decision.reply = old.reply.clone();
                    decision.error = old.error.clone();
                } else {
                    decision.status = "pending".into();
                }
                state.decisions.insert(decision.id.clone(), decision);
            }
            for decision in state.decisions.values_mut().filter(|decision| {
                decision.attempt_id == attempt_id
                    && !seen.contains(&decision.id)
                    && matches!(decision.status.as_str(), "pending" | "reply_pending")
            }) {
                decision.status = "expired".into();
            }
            refresh(state, &attempt.task_id, &observation.state, &engine.now()?)
        })
        .await
    }

    async fn settle_control(
        &self,
        context: &Context,
        state: &State,
        spec: &SessionSpec,
        mut attempt: Attempt,
        task: Task,
        mut observation: Observation,
    ) -> Result<()> {
        let mut uncertain = false;
        for command in state.commands.values().filter(|command| {
            command.attempt_id == attempt.id
                && command.kind == "prompt"
                && command.status == "sending"
        }) {
            if !self.inner.runtime.capabilities().reconcile_prompt
                || read(
                    context,
                    self.inner
                        .runtime
                        .reconcile_prompt(context, spec, &command.id, &command.text),
                )
                .await
                .is_err()
            {
                uncertain = true;
                continue;
            }
            self.admitted(&command.id, &attempt.id, false).await?;
            attempt.failure_count = 0;
        }
        let can_cancel = self.inner.runtime.capabilities().cancel_pending;
        if !control_quiescent(&observation)
            || (task.desired == "cancelled" && can_cancel && task.state != task.desired)
        {
            if attempt.failure_count >= 8 {
                return Ok(());
            }
            let result = if task.desired == "cancelled" && can_cancel && !uncertain {
                write(context, || self.inner.runtime.cancel_pending(context, spec)).await
            } else {
                write(context, || self.inner.runtime.interrupt(context, spec)).await
            };
            if let Err(error) = result {
                return self.mark(&attempt.id, "unknown", Some(error)).await;
            }
            observation = match read(context, self.inner.runtime.inspect(context, spec)).await {
                Ok(observation) => observation,
                Err(error) => return self.mark(&attempt.id, "disconnected", Some(error)).await,
            };
            if !control_quiescent(&observation) {
                return self
                    .mark(&attempt.id, "unknown", Some(Error::uncertain()))
                    .await;
            }
        }
        if uncertain {
            return self
                .mark(&attempt.id, &observation.state, Some(Error::uncertain()))
                .await;
        }
        let engine = self.clone();
        self.bookkeeping(move |state| {
            let current = state
                .tasks
                .get_mut(&task.id)
                .ok_or_else(|| Error::not_found("task not found"))?;
            if current.state != current.desired {
                current.state = current.desired.clone();
                current.updated_at = engine.now()?;
            }
            let cancelled = current.desired == "cancelled";
            state
                .attempts
                .get_mut(&attempt.id)
                .ok_or_else(missing_attempt)?
                .observed = observation.state;
            for command in state.commands.values_mut().filter(|command| {
                command.attempt_id == attempt.id
                    && matches!(command.status.as_str(), "pending" | "parked")
            }) {
                command.status = if cancelled { "cancelled" } else { "parked" }.into();
            }
            Ok(())
        })
        .await
    }

    async fn deliver(
        &self,
        context: &Context,
        spec: &SessionSpec,
        task: &Task,
        attempt: &mut Attempt,
        observation: &Observation,
    ) -> Result<()> {
        let mut snapshot = self.snapshot(context).await?;
        for decision in snapshot
            .decisions
            .values()
            .filter(|decision| decision.attempt_id == attempt.id)
        {
            if decision.status == "uncertain" {
                return Ok(());
            }
            if decision.status != "reply_pending" {
                continue;
            }
            if attempt.failure_count >= 8
                || (matches!(observation.state.as_str(), "idle" | "failed")
                    && !capacity_available(&snapshot, attempt, self.inner.options.concurrency))
            {
                return Ok(());
            }
            self.decision_status(context, &decision.id, "uncertain")
                .await?;
            let answer = decision
                .reply
                .as_ref()
                .ok_or_else(|| Error::new("corrupt_state", "pending decision reply is absent"))?;
            if let Err(error) = write(context, || {
                self.inner.runtime.reply(context, spec, decision, answer)
            })
            .await
            {
                if error.code == "invalid_request" && error.is_not_delivered() {
                    return self.reject_reply(decision, error).await;
                }
                if error.is_not_delivered() {
                    self.decision_status(
                        &Context::detached(Duration::from_secs(5)),
                        &decision.id,
                        "reply_pending",
                    )
                    .await?;
                }
                return self.mark(&attempt.id, "unknown", Some(error)).await;
            }
            let decision_id = decision.id.clone();
            let attempt_id = attempt.id.clone();
            let engine = self.clone();
            self.bookkeeping(move |state| {
                state
                    .decisions
                    .get_mut(&decision_id)
                    .ok_or_else(|| Error::not_found("decision not found"))?
                    .status = "answered".into();
                let current = state
                    .attempts
                    .get_mut(&attempt_id)
                    .ok_or_else(missing_attempt)?;
                current.phase = "executing".into();
                current.observed = "running".into();
                current.failure_count = 0;
                current.error.clear();
                let task_id = current.task_id.clone();
                refresh(state, &task_id, "running", &engine.now()?)
            })
            .await?;
            attempt.phase = "executing".into();
        }
        snapshot = self.snapshot(context).await?;
        let mut commands: Vec<_> = snapshot
            .commands
            .values()
            .filter(|command| {
                command.attempt_id == attempt.id && unsettled(command) && command.kind == "prompt"
            })
            .cloned()
            .collect();
        commands.sort_by(|a, b| (&a.sequence, &a.id).cmp(&(&b.sequence, &b.id)));
        for command in commands {
            let result = if command.status == "sending" {
                if self.inner.runtime.capabilities().reconcile_prompt {
                    read(
                        context,
                        self.inner.runtime.reconcile_prompt(
                            context,
                            spec,
                            &command.id,
                            &command.text,
                        ),
                    )
                    .await
                } else {
                    Err(Error::uncertain())
                }
            } else {
                if attempt.phase == "session_bound" && !ancestors_active(&snapshot, task)? {
                    return Ok(());
                }
                if matches!(observation.state.as_str(), "idle" | "failed")
                    && !capacity_available(&snapshot, attempt, self.inner.options.concurrency)
                {
                    return Ok(());
                }
                if snapshot
                    .attempts
                    .get(&attempt.id)
                    .ok_or_else(missing_attempt)?
                    .failure_count
                    >= 8
                {
                    return Ok(());
                }
                self.command_status(context, &command.id, "sending").await?;
                write(context, || {
                    self.inner
                        .runtime
                        .prompt(context, spec, &command.id, &command.text)
                })
                .await
            };
            if let Err(error) = result {
                if error.is_not_delivered() {
                    self.command_status(
                        &Context::detached(Duration::from_secs(5)),
                        &command.id,
                        "pending",
                    )
                    .await?;
                }
                let observed = if command.status == "sending" && error.code == "uncertain" {
                    observation.state.as_str()
                } else {
                    "unknown"
                };
                return self.mark(&attempt.id, observed, Some(error)).await;
            }
            self.admitted(&command.id, &attempt.id, true).await?;
            attempt.phase = "executing".into();
        }
        Ok(())
    }

    async fn admitted(&self, command_id: &str, attempt_id: &str, running: bool) -> Result<()> {
        let command_id = command_id.to_owned();
        let attempt_id = attempt_id.to_owned();
        let engine = self.clone();
        self.bookkeeping(move |state| {
            state
                .commands
                .get_mut(&command_id)
                .ok_or_else(|| Error::new("corrupt_state", "dispatch command is absent"))?
                .status = "admitted".into();
            let attempt = state
                .attempts
                .get_mut(&attempt_id)
                .ok_or_else(missing_attempt)?;
            attempt.phase = "executing".into();
            attempt.failure_count = 0;
            if running {
                attempt.observed = "running".into();
                attempt.error.clear();
                let task_id = attempt.task_id.clone();
                refresh(state, &task_id, "running", &engine.now()?)?;
            }
            Ok(())
        })
        .await
    }

    async fn command_status(
        &self,
        context: &Context,
        command_id: &str,
        status: &str,
    ) -> Result<()> {
        let command_id = command_id.to_owned();
        let status = status.to_owned();
        self.update(context, move |state| {
            state
                .commands
                .get_mut(&command_id)
                .ok_or_else(|| Error::new("corrupt_state", "dispatch command is absent"))?
                .status = status;
            Ok(())
        })
        .await
    }

    async fn decision_status(
        &self,
        context: &Context,
        decision_id: &str,
        status: &str,
    ) -> Result<()> {
        let decision_id = decision_id.to_owned();
        let status = status.to_owned();
        self.update(context, move |state| {
            state
                .decisions
                .get_mut(&decision_id)
                .ok_or_else(|| Error::not_found("decision not found"))?
                .status = status;
            Ok(())
        })
        .await
    }

    async fn reject_reply(&self, decision: &Decision, error: Error) -> Result<()> {
        let decision_id = decision.id.clone();
        self.bookkeeping(move |state| {
            let decision = state
                .decisions
                .get_mut(&decision_id)
                .ok_or_else(|| Error::not_found("decision not found"))?;
            decision.status = "pending".into();
            decision.reply = None;
            decision.error = error.message;
            let task = state
                .tasks
                .get_mut(&decision.task_id)
                .ok_or_else(|| Error::not_found("task not found"))?;
            task.state = "waiting_for_input".into();
            task.error = "decision reply was rejected; submit a corrected answer".into();
            Ok(())
        })
        .await
    }
}

fn ancestors_active(state: &State, task: &Task) -> Result<bool> {
    Ok(ancestors(state, &task.parent_id)?.iter().all(|parent| {
        state
            .tasks
            .get(parent)
            .is_some_and(|task| task.desired == "active")
    }))
}
