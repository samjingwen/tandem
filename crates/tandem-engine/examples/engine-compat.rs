//! Explicit-directory, disposable compatibility consumer. All concrete native
//! effects are disabled. This is not a daemon or production client command.
use std::{
    collections::BTreeMap,
    io::{self, Read},
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use tandem_core::*;
use tandem_engine::{Engine, Options, journal};
use tandem_store::Sqlite;

#[derive(Deserialize)]
struct Request {
    kind: String,
    #[serde(default)]
    operation: String,
    #[serde(default)]
    target: String,
    input: RawJson,
}

#[derive(Serialize)]
struct Journal {
    body: String,
    fingerprint: String,
}

fn encode<T: Serialize>(kind: &str, input: T) -> Result<String> {
    let result = Journal {
        body: journal::encode(&input)?,
        fingerprint: journal::fingerprint(kind, &input)?,
    };
    journal::encode(&result)
}

fn fingerprint(request: &Request) -> Result<String> {
    let input = request.input.as_str();
    match request.kind.split('/').next().unwrap_or("") {
        "project.register" => encode(&request.kind, parse::<RegisterProject>(input)?),
        "task.create" => encode(&request.kind, parse::<CreateTask>(input)?),
        "task.followup" => encode(&request.kind, parse::<Followup>(input)?),
        "task.control" => encode(&request.kind, parse::<Control>(input)?),
        "task.result" => encode(&request.kind, parse::<SubmitResult>(input)?),
        "decision.reply" => encode(&request.kind, parse::<ReplyDecision>(input)?),
        "task.accept" | "workspace.cleanup" => encode(&request.kind, serde_json::json!({})),
        _ => Err(Error::invalid("unknown fixture operation")),
    }
}

fn parse<T: serde::de::DeserializeOwned>(input: &str) -> Result<T> {
    serde_json::from_str(input).map_err(|_| Error::invalid("invalid fixture input"))
}

#[derive(Default)]
struct Disabled {
    observations: Option<BTreeMap<String, Observation>>,
    effect_attempted: AtomicBool,
}
impl Disabled {
    fn forbid_effect(&self) -> Error {
        self.effect_attempted.store(true, Ordering::Release);
        disabled()
    }
}
fn disabled() -> Error {
    Error::new(
        "fixture_effect_forbidden",
        "compatibility consumer forbids native effects",
    )
}

#[async_trait]
impl Runtime for Disabled {
    fn reference(&self, _id: &str) -> String {
        "forbidden".into()
    }
    async fn info(&self, _context: &Context) -> Result<RuntimeInfo> {
        if self.observations.is_some() {
            Ok(RuntimeInfo {
                compatible: true,
                ..Default::default()
            })
        } else {
            Err(disabled())
        }
    }
    async fn ensure(&self, _context: &Context, _spec: &SessionSpec) -> Result<()> {
        Err(self.forbid_effect())
    }
    async fn inspect(&self, _context: &Context, spec: &SessionSpec) -> Result<Observation> {
        self.observations
            .as_ref()
            .and_then(|observations| observations.get(&spec.reference))
            .cloned()
            .ok_or_else(disabled)
    }
    async fn prompt(
        &self,
        _context: &Context,
        _spec: &SessionSpec,
        _id: &str,
        _text: &str,
    ) -> Result<()> {
        Err(self.forbid_effect())
    }
    async fn interrupt(&self, _context: &Context, _spec: &SessionSpec) -> Result<()> {
        Err(self.forbid_effect())
    }
    async fn cancel_pending(&self, _context: &Context, _spec: &SessionSpec) -> Result<()> {
        Err(self.forbid_effect())
    }
    async fn reply(
        &self,
        _context: &Context,
        _spec: &SessionSpec,
        _decision: &Decision,
        _answer: &RawJson,
    ) -> Result<()> {
        Err(self.forbid_effect())
    }
}

#[async_trait]
impl Workspaces for Disabled {
    fn plan(&self, _project: &Project, _attempt: &str) -> Workspace {
        Workspace::default()
    }
    async fn register(&self, _context: &Context, _input: &RegisterProject) -> Result<Project> {
        Err(disabled())
    }
    async fn ensure(
        &self,
        _context: &Context,
        _project: &Project,
        _workspace: &Workspace,
    ) -> Result<Workspace> {
        Err(self.forbid_effect())
    }
    async fn inspect(
        &self,
        _context: &Context,
        _project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        if self.observations.is_some() && workspace.allocated {
            Ok(())
        } else {
            Err(disabled())
        }
    }
    async fn remove(
        &self,
        _context: &Context,
        _project: &Project,
        _workspace: &Workspace,
    ) -> Result<()> {
        Err(self.forbid_effect())
    }
}

async fn call(engine: &Engine, request: Request) -> Result<String> {
    let context = Context::background();
    let input = request.input.as_str();
    match request.kind.split('/').next().unwrap_or("") {
        "project.register" => journal::encode(
            &engine
                .register(&context, &request.operation, parse(input)?)
                .await?,
        ),
        "task.create" => journal::encode(
            &engine
                .create(&context, &request.operation, parse(input)?)
                .await?,
        ),
        "task.followup" => journal::encode(
            &engine
                .followup(&context, &request.operation, &request.target, parse(input)?)
                .await?,
        ),
        "task.control" => journal::encode(
            &engine
                .control(&context, &request.operation, &request.target, parse(input)?)
                .await?,
        ),
        "task.result" => journal::encode(
            &engine
                .submit_result(&context, &request.operation, &request.target, parse(input)?)
                .await?,
        ),
        "decision.reply" => journal::encode(
            &engine
                .reply(&context, &request.operation, &request.target, parse(input)?)
                .await?,
        ),
        "task.accept" => journal::encode(
            &engine
                .accept(&context, &request.operation, &request.target)
                .await?,
        ),
        "workspace.cleanup" => journal::encode(
            &engine
                .cleanup(&context, &request.operation, &request.target)
                .await?,
        ),
        _ => Err(Error::invalid("unknown fixture operation")),
    }
}

async fn run() -> Result<String> {
    let args: Vec<_> = std::env::args_os().skip(1).collect();
    let mut source = String::new();
    io::stdin()
        .read_to_string(&mut source)
        .map_err(|_| Error::invalid("fixture input failed"))?;
    let request: Request = parse(&source)?;
    match args.first().and_then(|mode| mode.to_str()) {
        Some("journal") if args.len() == 1 => fingerprint(&request),
        Some(mode @ ("call" | "observe")) if args.len() == 2 => {
            let store = Arc::new(Sqlite::open(&args[1])?);
            let fixture = Arc::new(Disabled {
                observations: if mode == "observe" {
                    Some(parse(request.input.as_str())?)
                } else {
                    None
                },
                ..Default::default()
            });
            let engine = Engine::new(
                store,
                fixture.clone(),
                fixture.clone(),
                Options {
                    runtime_id: "fake".into(),
                    ..Default::default()
                },
            );
            let result = if mode == "observe" {
                match engine.tick(&Context::background()).await {
                    Ok(()) => journal::encode(&engine.snapshot(&Context::background()).await?),
                    Err(error) => Err(error),
                }
            } else {
                call(&engine, request).await
            };
            engine.shutdown().await;
            if fixture.effect_attempted.load(Ordering::Acquire) {
                Err(disabled())
            } else {
                result
            }
        }
        _ => Err(Error::invalid(
            "expected fixture mode and optional owned directory",
        )),
    }
}

#[tokio::main]
async fn main() {
    match run().await {
        Ok(output) => println!("{output}"),
        Err(error) => {
            eprintln!(
                "{}",
                serde_json::to_string(&error).expect("serializable fixture error")
            );
            std::process::exit(1);
        }
    }
}
