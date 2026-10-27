//! Test-only executable consumer of the actual adapter. Its caller supplies a
//! disposable HTTP fixture endpoint explicitly; no native process is launched.

use std::{
    io::{self, Read},
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use serde::Deserialize;
use serde_json::{Value, json};
use tandem_core::{Context, Decision, Error, RawJson, Runtime, SessionSpec};
use tandem_opencode::{Model, OpenCode, Options};

#[derive(Default, Deserialize)]
#[serde(default)]
struct Spec {
    #[serde(rename = "Reference")]
    reference: String,
    #[serde(rename = "TaskID")]
    task_id: String,
    #[serde(rename = "AttemptID")]
    attempt_id: String,
    #[serde(rename = "InstanceID")]
    instance_id: String,
    #[serde(rename = "Directory")]
    directory: String,
    #[serde(rename = "Role")]
    role: String,
    #[serde(rename = "Title")]
    title: String,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Request {
    url: String,
    password: String,
    model: Option<Model>,
    method: String,
    #[serde(default)]
    spec: Spec,
    #[serde(default)]
    command: String,
    #[serde(default)]
    text: String,
    #[serde(default)]
    decision: Decision,
    #[serde(default)]
    answer: RawJson,
    timeout_ms: Option<u64>,
    cancel_after_ms: Option<u64>,
    #[serde(default)]
    canceled: bool,
    #[serde(default)]
    cancel_on_event: bool,
}

async fn run(request: Request, wakes: Arc<AtomicUsize>) -> tandem_core::Result<Value> {
    let runtime = OpenCode::new(Options {
        url: request.url,
        password: request.password,
        model: request.model,
    })?;
    let spec = SessionSpec {
        reference: request.spec.reference,
        task_id: request.spec.task_id,
        attempt_id: request.spec.attempt_id,
        instance_id: request.spec.instance_id,
        directory: request.spec.directory,
        role: request.spec.role,
        title: request.spec.title,
    };
    let mut context = Context::background();
    if let Some(timeout) = request.timeout_ms {
        context = context.with_timeout(Duration::from_millis(timeout));
    }
    if request.canceled {
        context.cancel();
    }
    let cancellation = request.cancel_after_ms.map(|delay| {
        let context = context.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(delay)).await;
            context.cancel();
        })
    });
    let result = match request.method.as_str() {
        "reference" => Ok(json!(runtime.reference(&request.command))),
        "info" => runtime.info(&context).await.and_then(|v| {
            serde_json::to_value(v).map_err(|_| Error::invalid("invalid fixture output"))
        }),
        "inspect" => runtime.inspect(&context, &spec).await.and_then(|v| {
            serde_json::to_value(v).map_err(|_| Error::invalid("invalid fixture output"))
        }),
        "ensure" => runtime.ensure(&context, &spec).await.map(|()| Value::Null),
        "prompt" => runtime
            .prompt(&context, &spec, &request.command, &request.text)
            .await
            .map(|()| Value::Null),
        "reconcile" => runtime
            .reconcile_prompt(&context, &spec, &request.command, &request.text)
            .await
            .map(|()| Value::Null),
        "interrupt" => runtime
            .interrupt(&context, &spec)
            .await
            .map(|()| Value::Null),
        "cancel" => runtime
            .cancel_pending(&context, &spec)
            .await
            .map(|()| Value::Null),
        "quiescence" => runtime
            .check_quiescence(&context, &spec)
            .await
            .map(|()| Value::Null),
        "reply" => runtime
            .reply(&context, &spec, &request.decision, &request.answer)
            .await
            .map(|()| Value::Null),
        "events" => {
            let event_context = context.clone();
            runtime
                .events(
                    &context,
                    Arc::new(move || {
                        wakes.fetch_add(1, Ordering::SeqCst);
                        if request.cancel_on_event {
                            event_context.cancel();
                        }
                    }),
                )
                .await
                .map(|()| Value::Null)
        }
        _ => Err(Error::invalid("unknown fixture method")),
    };
    if let Some(task) = cancellation {
        task.abort();
    }
    result
}

#[tokio::main]
async fn main() {
    let wakes = Arc::new(AtomicUsize::new(0));
    let mut input = String::new();
    let result = match io::stdin().take((16 << 20) + 1).read_to_string(&mut input) {
        Ok(_) if input.len() <= 16 << 20 => match serde_json::from_str::<Request>(&input) {
            Ok(request) => run(request, wakes.clone()).await,
            Err(_) => Err(Error::invalid("invalid fixture input")),
        },
        _ => Err(Error::invalid(
            "fixture input exceeds limit or is unreadable",
        )),
    };
    let output = match result {
        Ok(value) => json!({"value":value,"wakes":wakes.load(Ordering::SeqCst)}),
        Err(error) => {
            json!({"error":{"code":error.code,"message":error.message,"notDelivered":error.is_not_delivered()},"wakes":wakes.load(Ordering::SeqCst)})
        }
    };
    println!("{output}");
}
