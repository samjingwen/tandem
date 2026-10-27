//! Disposable JSON-lines library consumer, not a native adapter or daemon.
//! Only runtime/workspace execution is simulated; Service and SQLite are real.
use std::{
    collections::BTreeMap,
    io::{self, BufRead, Write},
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use async_trait::async_trait;
use serde_json::{Value, json};
use tandem_core::*;
use tandem_engine::{Engine, Options};
use tandem_store::Sqlite;
use tokio::{sync::Semaphore, task::JoinHandle};

struct Storage {
    sqlite: Arc<Sqlite>,
    delay: AtomicBool,
    blocked: AtomicBool,
    release: (Mutex<bool>, Condvar),
}

impl Storage {
    fn release(&self) {
        *self.release.0.lock().unwrap() = true;
        self.release.1.notify_all();
    }
}

impl Store for Storage {
    fn snapshot(&self) -> Result<State> {
        self.sqlite.snapshot()
    }
    fn read_revision(&self) -> Result<Revision> {
        self.sqlite.read_revision()
    }
    fn update(&self, change: &mut dyn FnMut(&mut State) -> Result<()>) -> Result<()> {
        if self.delay.swap(false, Ordering::AcqRel) {
            self.blocked.store(true, Ordering::Release);
            let mut released = self.release.0.lock().unwrap();
            while !*released {
                released = self.release.1.wait(released).unwrap();
            }
            self.blocked.store(false, Ordering::Release);
        }
        self.sqlite.update(change)
    }
}

#[derive(Default)]
struct Execution {
    observations: BTreeMap<String, Observation>,
    prompt_error: String,
    reconcile_uncertain: bool,
    background_busy: bool,
    delay_bookkeeping: bool,
    audit: Vec<Value>,
}

struct Boundary {
    storage: Arc<Storage>,
    execution: Mutex<Execution>,
    disconnect: Semaphore,
}

impl Boundary {
    fn record(&self, kind: &str, target: &str) -> Result<()> {
        // Capture actual durable records at the external execution boundary.
        let state = self.storage.snapshot()?;
        self.execution.lock().unwrap().audit.push(json!({
            "kind": kind, "target": target, "durable": state,
        }));
        Ok(())
    }
}

#[async_trait]
impl Runtime for Boundary {
    fn reference(&self, id: &str) -> String {
        format!("simulated/{id}")
    }
    fn capabilities(&self) -> RuntimeCapabilities {
        RuntimeCapabilities {
            reconcile_prompt: true,
            cancel_pending: true,
            quiescence: true,
        }
    }
    async fn info(&self, context: &Context) -> Result<RuntimeInfo> {
        context.check()?;
        Ok(RuntimeInfo {
            id: "simulated".into(),
            compatible: true,
            ..Default::default()
        })
    }
    async fn ensure(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        self.record("session.ensure", &spec.attempt_id)?;
        self.execution.lock().unwrap().observations.insert(
            spec.reference.clone(),
            Observation {
                exists: true,
                owned: true,
                state: "idle".into(),
                ..Default::default()
            },
        );
        Ok(())
    }
    async fn inspect(&self, context: &Context, spec: &SessionSpec) -> Result<Observation> {
        context.check()?;
        Ok(self
            .execution
            .lock()
            .unwrap()
            .observations
            .get(&spec.reference)
            .cloned()
            .unwrap_or_default())
    }
    async fn prompt(
        &self,
        context: &Context,
        spec: &SessionSpec,
        command: &str,
        _text: &str,
    ) -> Result<()> {
        context.check()?;
        self.record("prompt", command)?;
        let mut execution = self.execution.lock().unwrap();
        match execution.prompt_error.as_str() {
            "uncertain" => Err(Error::uncertain()),
            "not_delivered" => Err(Error::not_delivered(Error::new(
                "unavailable",
                "simulated non-delivery",
            ))),
            _ => {
                execution
                    .observations
                    .get_mut(&spec.reference)
                    .unwrap()
                    .state = "running".into();
                if execution.delay_bookkeeping {
                    self.storage.delay.store(true, Ordering::Release);
                }
                Ok(())
            }
        }
    }
    async fn reconcile_prompt(
        &self,
        context: &Context,
        _spec: &SessionSpec,
        command: &str,
        _text: &str,
    ) -> Result<()> {
        context.check()?;
        self.record("prompt.inspect", command)?;
        if self.execution.lock().unwrap().reconcile_uncertain {
            Err(Error::uncertain())
        } else {
            Ok(())
        }
    }
    async fn interrupt(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        self.record("interrupt", &spec.attempt_id)?;
        self.execution
            .lock()
            .unwrap()
            .observations
            .get_mut(&spec.reference)
            .unwrap()
            .state = "idle".into();
        Ok(())
    }
    async fn cancel_pending(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        self.record("cancel_pending", &spec.attempt_id)
    }
    async fn check_quiescence(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        self.record("quiescence", &spec.attempt_id)?;
        if self.execution.lock().unwrap().background_busy {
            Err(Error::invalid("simulated background work"))
        } else {
            Ok(())
        }
    }
    async fn reply(
        &self,
        _context: &Context,
        _spec: &SessionSpec,
        _decision: &Decision,
        _answer: &RawJson,
    ) -> Result<()> {
        Err(Error::not_delivered(Error::invalid(
            "reply not supported by safety consumer",
        )))
    }
    async fn events(&self, context: &Context, _notify: Arc<dyn Fn() + Send + Sync>) -> Result<()> {
        struct Subscription<'a>(&'a Boundary);
        impl Drop for Subscription<'_> {
            fn drop(&mut self) {
                self.0
                    .execution
                    .lock()
                    .unwrap()
                    .audit
                    .push(json!({"kind": "events.stopped"}));
            }
        }
        context.check()?;
        self.execution
            .lock()
            .unwrap()
            .audit
            .push(json!({"kind": "events.started"}));
        let _subscription = Subscription(self);
        self.disconnect.acquire().await.unwrap().forget();
        Ok(())
    }
}

#[async_trait]
impl Workspaces for Boundary {
    fn plan(&self, project: &Project, attempt: &str) -> Workspace {
        Workspace {
            owner: attempt.into(),
            path: format!("/synthetic/{attempt}"),
            branch: attempt.into(),
            base_ref: project.base_ref.clone(),
            ..Default::default()
        }
    }
    async fn register(&self, context: &Context, input: &RegisterProject) -> Result<Project> {
        context.check()?;
        Ok(Project {
            name: input.name.clone(),
            path: input.path.clone(),
            base_ref: input.base_ref.clone(),
            ..Default::default()
        })
    }
    async fn ensure(
        &self,
        context: &Context,
        _project: &Project,
        workspace: &Workspace,
    ) -> Result<Workspace> {
        context.check()?;
        self.record("workspace.ensure", &workspace.owner)?;
        Ok(Workspace {
            allocated: true,
            ..workspace.clone()
        })
    }
    async fn inspect(
        &self,
        context: &Context,
        _project: &Project,
        _workspace: &Workspace,
    ) -> Result<()> {
        context.check()
    }
    async fn remove(
        &self,
        context: &Context,
        _project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        context.check()?;
        self.record("workspace.remove", &workspace.owner)
    }
}

struct Consumer {
    engine: Engine,
    boundary: Arc<Boundary>,
    jobs: BTreeMap<String, JoinHandle<Result<Value>>>,
    contexts: BTreeMap<String, Context>,
}

fn text<'a>(v: &'a Value, key: &str) -> &'a str {
    v[key].as_str().unwrap_or("")
}
fn encode<T: serde::Serialize>(result: Result<T>) -> Result<Value> {
    serde_json::to_value(result?).map_err(|_| Error::invalid("cannot encode consumer response"))
}
fn input<T: serde::de::DeserializeOwned>(v: &Value) -> Result<T> {
    serde_json::from_value(v["input"].clone()).map_err(|e| Error::invalid(e.to_string()))
}

impl Consumer {
    async fn call(&mut self, v: Value) -> Result<Value> {
        let service: &dyn Service = &self.engine;
        let context = Context::background();
        let op = text(&v, "operation");
        let target = text(&v, "target");
        match text(&v, "kind") {
            "register" => encode(service.register(&context, op, input(&v)?).await),
            "create" => encode(service.create(&context, op, input(&v)?).await),
            "followup" => encode(service.followup(&context, op, target, input(&v)?).await),
            "result" => encode(
                service
                    .submit_result(&context, op, target, input(&v)?)
                    .await,
            ),
            "accept" => encode(service.accept(&context, op, target).await),
            "control" => encode(service.control(&context, op, target, input(&v)?).await),
            "cleanup" => encode(service.cleanup(&context, op, target).await),
            "tick" => encode(self.engine.tick(&context).await),
            "snapshot" => encode(service.snapshot(&context).await),
            "configure" => {
                let mut e = self.boundary.execution.lock().unwrap();
                if let Some(mode) = v["prompt_error"].as_str() {
                    e.prompt_error = mode.into();
                }
                if let Some(b) = v["reconcile_uncertain"].as_bool() {
                    e.reconcile_uncertain = b;
                }
                if let Some(b) = v["background_busy"].as_bool() {
                    e.background_busy = b;
                }
                if let Some(b) = v["delay_bookkeeping"].as_bool() {
                    e.delay_bookkeeping = b;
                }
                Ok(Value::Null)
            }
            "observe" => {
                let state = self.boundary.storage.snapshot()?;
                let attempt = &state.attempts[&state.tasks[target].attempt_id];
                self.boundary
                    .execution
                    .lock()
                    .unwrap()
                    .observations
                    .get_mut(&attempt.session_ref)
                    .unwrap()
                    .state = text(&v, "state").into();
                Ok(Value::Null)
            }
            "audit" => Ok(json!({
                "events": self.boundary.execution.lock().unwrap().audit,
                "blocked": self.boundary.storage.blocked.load(Ordering::Acquire),
            })),
            "start" => {
                let name = text(&v, "name").to_owned();
                if self.jobs.contains_key(&name) {
                    return Err(Error::invalid("duplicate job"));
                }
                let context = match v["deadline_ms"].as_u64() {
                    Some(ms) => context.with_timeout(Duration::from_millis(ms)),
                    None => context,
                };
                self.contexts.insert(name.clone(), context.clone());
                let engine = self.engine.clone();
                let action = text(&v, "action").to_owned();
                let target = target.to_owned();
                let op = op.to_owned();
                let control = v["input"].clone();
                self.jobs.insert(
                    name,
                    tokio::spawn(async move {
                        match action.as_str() {
                            "run" => encode(engine.run(&context, Duration::from_secs(60)).await),
                            "tick" => encode(engine.tick(&context).await),
                            "control" => encode(
                                engine
                                    .control(
                                        &context,
                                        &op,
                                        &target,
                                        serde_json::from_value(control)
                                            .map_err(|_| Error::invalid("bad control"))?,
                                    )
                                    .await,
                            ),
                            "shutdown" => {
                                engine.shutdown().await;
                                Ok(Value::Null)
                            }
                            _ => Err(Error::invalid("unknown job action")),
                        }
                    }),
                );
                Ok(Value::Null)
            }
            "cancel" => {
                self.contexts[target].cancel();
                Ok(Value::Null)
            }
            "drop_wait" => {
                let job = self
                    .jobs
                    .remove(target)
                    .ok_or_else(|| Error::invalid("unknown job"))?;
                job.abort();
                let _ = job.await;
                Ok(Value::Null)
            }
            "job" => Ok(json!({"finished": self.jobs[target].is_finished()})),
            "join" => self
                .jobs
                .remove(target)
                .ok_or_else(|| Error::invalid("unknown job"))?
                .await
                .map_err(|_| Error::invalid("job failed"))?,
            "disconnect" => {
                self.boundary.disconnect.add_permits(1);
                Ok(Value::Null)
            }
            "release" => {
                self.boundary.storage.release();
                Ok(Value::Null)
            }
            _ => Err(Error::invalid("unknown safety consumer request")),
        }
    }
}

#[tokio::main]
async fn main() {
    let directory = std::env::args_os()
        .nth(1)
        .expect("explicit disposable directory required");
    let storage = Arc::new(Storage {
        sqlite: Arc::new(Sqlite::open(directory).unwrap()),
        delay: AtomicBool::new(false),
        blocked: AtomicBool::new(false),
        release: (Mutex::new(false), Condvar::new()),
    });
    let boundary = Arc::new(Boundary {
        storage,
        execution: Mutex::new(Execution::default()),
        disconnect: Semaphore::new(0),
    });
    let engine = Engine::new(
        boundary.storage.clone(),
        boundary.clone(),
        boundary.clone(),
        Options {
            runtime_id: "simulated".into(),
            concurrency: 1,
            max_tasks: 4,
            max_depth: 1,
            max_coordinator_notifications: 1,
        },
    );
    let mut consumer = Consumer {
        engine,
        boundary,
        jobs: BTreeMap::new(),
        contexts: BTreeMap::new(),
    };
    // Blocking stdin is isolated from Tokio so cancellation and event reads keep running.
    let (send, mut receive) = tokio::sync::mpsc::channel(1);
    std::thread::spawn(move || {
        for line in io::stdin().lock().lines() {
            if send.blocking_send(line.unwrap()).is_err() {
                break;
            }
        }
    });
    while let Some(line) = receive.recv().await {
        let result = match serde_json::from_str(&line) {
            Ok(request) => consumer.call(request).await,
            Err(e) => Err(Error::invalid(e.to_string())),
        };
        let response = match result {
            Ok(value) => json!({"ok": value}),
            Err(error) => json!({"error": {"code": error.code, "message": error.message}}),
        };
        println!("{response}");
        io::stdout().flush().unwrap();
    }
    consumer.boundary.storage.release();
    consumer.engine.shutdown().await;
    for (_, job) in consumer.jobs {
        let _ = job.await;
    }
}
