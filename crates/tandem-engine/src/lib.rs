//! Coordination, not an agent harness. Concrete native/SQLite/HTTP adapters do
//! not enter this crate. Tokio owns asynchronous effects and their lifecycle;
//! core contracts and cancellation state remain executor-independent.

mod commands;
pub mod journal;
mod notifications;
mod reconcile;
mod state;

use std::{
    future::Future,
    sync::{Arc, atomic::AtomicBool},
    time::Duration,
};

use serde::{Serialize, de::DeserializeOwned};
use tandem_core::{
    Context, Error, Operation, RawJson, Result, Runtime, State, Store, Timestamp, Workspaces,
};
use tokio::sync::Mutex;
use tokio_util::task::TaskTracker;

#[derive(Clone)]
pub struct Options {
    pub runtime_id: String,
    pub concurrency: usize,
    pub max_tasks: usize,
    pub max_depth: usize,
    pub max_coordinator_notifications: usize,
}

impl Default for Options {
    fn default() -> Self {
        Self {
            runtime_id: String::new(),
            concurrency: 2,
            max_tasks: 128,
            max_depth: 3,
            max_coordinator_notifications: 32,
        }
    }
}

struct Inner {
    store: Arc<dyn Store>,
    runtime: Arc<dyn Runtime>,
    workspaces: Arc<dyn Workspaces>,
    options: Options,
    tick: Mutex<()>,
    effects: Mutex<()>,
    reconciled: AtomicBool,
    // Serialize admission with close+wait, so shutdown cannot return between a
    // successful admission check and registration of its owned task.
    closing: std::sync::Mutex<bool>,
    stop: Context,
    tasks: TaskTracker,
}

#[derive(Clone)]
pub struct Engine {
    inner: Arc<Inner>,
}

impl Engine {
    pub fn new(
        store: Arc<dyn Store>,
        runtime: Arc<dyn Runtime>,
        workspaces: Arc<dyn Workspaces>,
        mut options: Options,
    ) -> Self {
        if options.concurrency == 0 {
            options.concurrency = 2;
        }
        if options.max_tasks == 0 {
            options.max_tasks = 128;
        }
        if options.max_depth == 0 {
            options.max_depth = 3;
        }
        if options.max_coordinator_notifications == 0 {
            options.max_coordinator_notifications = 32;
        }
        Self {
            inner: Arc::new(Inner {
                store,
                runtime,
                workspaces,
                options,
                tick: Mutex::new(()),
                effects: Mutex::new(()),
                reconciled: AtomicBool::new(false),
                closing: std::sync::Mutex::new(false),
                stop: Context::background(),
                tasks: TaskTracker::new(),
            }),
        }
    }

    pub async fn shutdown(&self) {
        // Reject new admission, stop the scheduler/event listener, and retain
        // store/effect ownership until every already-owned task has settled.
        {
            let mut closing = self
                .inner
                .closing
                .lock()
                .unwrap_or_else(|poison| poison.into_inner());
            *closing = true;
            self.inner.stop.cancel();
            self.inner.tasks.close();
        }
        self.inner.tasks.wait().await;
    }

    async fn owned<T, F, Fut>(&self, context: &Context, action: F) -> Result<T>
    where
        T: Send + 'static,
        F: FnOnce(Engine, Context) -> Fut + Send + 'static,
        Fut: Future<Output = Result<T>> + Send + 'static,
    {
        context.check()?;
        let engine = self.clone();
        let context = context.clone();
        let caller_context = context.clone();
        let task_context = match context.deadline() {
            Some(deadline) => Context::background()
                .with_timeout(deadline.saturating_duration_since(std::time::Instant::now())),
            None => Context::background(),
        };
        let stop = self.inner.stop.clone();
        let job = {
            let closing = self
                .inner
                .closing
                .lock()
                .map_err(|_| Error::new("internal", "coordination admission lock failed"))?;
            if *closing {
                return Err(Error::new(
                    "unavailable",
                    "coordination owner is shutting down",
                ));
            }
            self.inner.tasks.spawn(async move {
                let operation = action(engine, task_context.clone());
                tokio::pin!(operation);
                tokio::select! {
                    biased;
                    result = &mut operation => return result,
                    () = caller_context.cancellation_requested() => (),
                    () = deadline(&caller_context) => (),
                    () = stop.cancellation_requested() => (),
                }
                // Signal cooperative cancellation but do not drop the
                // action: it must retain barriers through bookkeeping.
                task_context.cancel();
                operation.await
            })
        };
        // Dropping this wait detaches only the caller. The tracker and task's
        // Engine clone retain effect/store ownership through outcome bookkeeping.
        read(&context, async move {
            job.await
                .map_err(|_| Error::new("internal", "owned coordination operation failed"))?
        })
        .await
    }

    fn now(&self) -> Result<Timestamp> {
        time::OffsetDateTime::now_utc()
            .format(&time::format_description::well_known::Rfc3339)
            .map(Timestamp)
            .map_err(|_| Error::new("internal", "clock is outside the supported timestamp range"))
    }

    pub async fn snapshot(&self, context: &Context) -> Result<State> {
        let store = self.inner.store.clone();
        let context = context.clone();
        tokio::task::spawn_blocking(move || {
            context.check()?;
            store.snapshot()
        })
        .await
        .map_err(|_| Error::new("internal", "state read task failed"))?
    }

    pub async fn read_revision(&self, context: &Context) -> Result<tandem_core::Revision> {
        let store = self.inner.store.clone();
        let context = context.clone();
        tokio::task::spawn_blocking(move || {
            context.check()?;
            store.read_revision()
        })
        .await
        .map_err(|_| Error::new("internal", "revision read task failed"))?
    }

    async fn update<T, F>(&self, context: &Context, change: F) -> Result<T>
    where
        T: Send + 'static,
        F: FnOnce(&mut State) -> Result<T> + Send + 'static,
    {
        let store = self.inner.store.clone();
        let context = context.clone();
        tokio::task::spawn_blocking(move || {
            context.check()?;
            let mut change = Some(change);
            let mut output = None;
            store.update(&mut |state| {
                context.check()?;
                let apply = change.take().ok_or_else(|| {
                    Error::new("internal", "state callback was invoked more than once")
                })?;
                let result = apply(state)?;
                context.check()?;
                output = Some(result);
                Ok(())
            })?;
            output.ok_or_else(|| Error::new("internal", "state transaction produced no outcome"))
        })
        .await
        .map_err(|_| Error::new("internal", "state transaction task failed"))?
    }

    async fn bookkeeping<T, F>(&self, change: F) -> Result<T>
    where
        T: Send + 'static,
        F: FnOnce(&mut State) -> Result<T> + Send + 'static,
    {
        self.update(&Context::detached(Duration::from_secs(5)), change)
            .await
    }

    async fn mutate<T, I, F>(
        &self,
        context: &Context,
        operation: String,
        kind: String,
        input: I,
        change: F,
    ) -> Result<T>
    where
        T: Serialize + DeserializeOwned + Send + 'static,
        I: Serialize,
        F: FnOnce(&mut State) -> Result<T> + Send + 'static,
    {
        if operation.trim().is_empty() || operation.len() > 160 {
            return Err(Error::invalid(
                "a stable operation ID of 1-160 characters is required",
            ));
        }
        let fingerprint = journal::fingerprint(&kind, &input)?;
        let engine = self.clone();
        self.update(context, move |state| {
            if let Some(old) = state.operations.get(&operation) {
                if old.fingerprint != fingerprint {
                    return Err(operation_conflict());
                }
                return serde_json::from_str(old.outcome.as_str()).map_err(|_| {
                    Error::new("corrupt_state", "stored operation outcome is invalid")
                });
            }
            let output = change(state)?;
            let outcome = RawJson::parse(journal::encode(&output)?)
                .map_err(|_| Error::new("internal", "operation outcome could not be encoded"))?;
            state.operations.insert(
                operation.clone(),
                Operation {
                    id: operation,
                    fingerprint,
                    outcome,
                    created_at: engine.now()?,
                    ..Default::default()
                },
            );
            Ok(output)
        })
        .await
    }

    async fn effects_lock(&self, context: &Context) -> Result<tokio::sync::MutexGuard<'_, ()>> {
        read(context, async { Ok(self.inner.effects.lock().await) }).await
    }
}

pub(crate) fn operation_conflict() -> Error {
    Error::new(
        "operation_conflict",
        "operation ID was already used for a different request",
    )
}

pub(crate) fn id(prefix: &str) -> Result<String> {
    let mut random = [0_u8; 16];
    getrandom::fill(&mut random).map_err(|_| {
        Error::new(
            "entropy_unavailable",
            "cannot allocate coordination identity",
        )
    })?;
    Ok(format!("{prefix}{}", journal::hex(&random)))
}

async fn deadline(context: &Context) {
    if let Some(instant) = context.deadline() {
        tokio::time::sleep_until(instant.into()).await;
    } else {
        std::future::pending::<()>().await;
    }
}

pub(crate) async fn read<T, F: Future<Output = Result<T>>>(
    context: &Context,
    future: F,
) -> Result<T> {
    context.check()?;
    tokio::select! {
        biased;
        result = future => result,
        () = context.cancellation_requested() => Err(context.check().err().unwrap_or_else(|| Error::new("canceled", "operation canceled"))),
        () = deadline(context) => Err(Error::new("timeout", "operation deadline exceeded")),
    }
}

pub(crate) async fn write<T, F, Fut>(context: &Context, action: F) -> Result<T>
where
    F: FnOnce() -> Fut,
    Fut: Future<Output = Result<T>>,
{
    context.check().map_err(Error::not_delivered)?;
    tokio::select! {
        biased;
        result = action() => result,
        () = context.cancellation_requested() => Err(Error::uncertain()),
        () = deadline(context) => Err(Error::uncertain()),
    }
}
