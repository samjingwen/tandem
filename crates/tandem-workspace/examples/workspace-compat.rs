//! Test-only consumer. Callers supply disposable directories explicitly; there
//! is no service/runtime discovery or retained application state access.

use std::{
    io::{self, Read},
    time::Duration,
};

use serde::Deserialize;
use serde_json::{Value, json};
use tandem_core::{Context, Error, Project, RegisterProject, Workspace, Workspaces};
use tandem_workspace::Git;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
struct Request {
    root: String,
    #[serde(default)]
    use_worktrunk: bool,
    method: String,
    #[serde(default)]
    project: Project,
    #[serde(default)]
    workspace: Workspace,
    #[serde(default)]
    input: RegisterProject,
    #[serde(default)]
    owner: String,
    timeout_ms: Option<u64>,
    cancel_after_ms: Option<u64>,
    #[serde(default)]
    canceled: bool,
}

async fn run(request: Request) -> tandem_core::Result<Value> {
    let workspaces = Git::new(&request.root, request.use_worktrunk)?;
    let mut context = Context::background();
    if let Some(timeout) = request.timeout_ms {
        context = context.with_timeout(Duration::from_millis(timeout));
    }
    if request.canceled {
        context.cancel();
    }
    let cancel = request.cancel_after_ms.map(|delay| {
        let context = context.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(delay)).await;
            context.cancel();
        })
    });
    let result = match request.method.as_str() {
        "plan" => Ok(json!(workspaces.plan(&request.project, &request.owner))),
        "register" => workspaces
            .register(&context, &request.input)
            .await
            .map(|value| json!(value)),
        "ensure" => workspaces
            .ensure(&context, &request.project, &request.workspace)
            .await
            .map(|value| json!(value)),
        "inspect" => workspaces
            .inspect(&context, &request.project, &request.workspace)
            .await
            .map(|()| Value::Null),
        "remove" => workspaces
            .remove(&context, &request.project, &request.workspace)
            .await
            .map(|()| Value::Null),
        _ => Err(Error::invalid("unknown fixture method")),
    };
    if let Some(cancel) = cancel {
        cancel.abort();
        let _ = cancel.await;
    }
    result
}

#[tokio::main]
async fn main() {
    let mut input = String::new();
    let result = match io::stdin().take((1 << 20) + 1).read_to_string(&mut input) {
        Ok(_) if input.len() <= 1 << 20 => match serde_json::from_str::<Request>(&input) {
            Ok(request) => run(request).await,
            Err(_) => Err(Error::invalid("invalid fixture input")),
        },
        _ => Err(Error::invalid(
            "fixture input exceeds limit or is unreadable",
        )),
    };
    let output = match result {
        Ok(value) => json!({"value":value}),
        Err(error) => json!({"error":error}),
    };
    println!("{output}");
}
