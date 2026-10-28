use std::{
    path::PathBuf,
    process::Command,
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
        mpsc,
    },
    time::Duration,
};

use async_trait::async_trait;
use tandem_core::*;
use tandem_engine::{Engine, Options};
use tandem_store::Sqlite;
use tokio::sync::Notify;

use crate::Git;

struct Idle;

#[async_trait]
impl Runtime for Idle {
    fn reference(&self, attempt: &str) -> String {
        attempt.into()
    }
    fn capabilities(&self) -> RuntimeCapabilities {
        RuntimeCapabilities {
            quiescence: true,
            ..Default::default()
        }
    }
    async fn info(&self, _: &Context) -> Result<RuntimeInfo> {
        Ok(RuntimeInfo {
            compatible: true,
            ..Default::default()
        })
    }
    async fn ensure(&self, context: &Context, _: &SessionSpec) -> Result<()> {
        context.check()
    }
    async fn inspect(&self, _: &Context, _: &SessionSpec) -> Result<Observation> {
        Ok(Observation {
            exists: true,
            owned: true,
            state: "idle".into(),
            ..Default::default()
        })
    }
    async fn prompt(&self, context: &Context, _: &SessionSpec, _: &str, _: &str) -> Result<()> {
        context.check()
    }
    async fn interrupt(&self, context: &Context, _: &SessionSpec) -> Result<()> {
        context.check()
    }
    async fn reply(&self, _: &Context, _: &SessionSpec, _: &Decision, _: &RawJson) -> Result<()> {
        unreachable!()
    }
    async fn check_quiescence(&self, context: &Context, _: &SessionSpec) -> Result<()> {
        context.check()
    }
}

struct Settling {
    git: Git,
    root: PathBuf,
    stage: &'static str,
    command: bool,
    armed: AtomicBool,
    release: Arc<Notify>,
    receiver: Mutex<Option<mpsc::Receiver<()>>>,
}

impl Settling {
    async fn effect(&self, context: &Context, stage: &str) -> Result<()> {
        if stage != self.stage || !self.armed.swap(false, Ordering::SeqCst) {
            return Ok(());
        }
        let result = if self.command {
            let result = crate::process::run(
                context,
                &self.root,
                "sh",
                &[
                    "-c".as_ref(),
                    "echo $$ > child; touch started; while :; do sleep 1; done".as_ref(),
                ],
            )
            .await
            .map(|_| ());
            self.release.notified().await;
            result
        } else {
            let receiver = self.receiver.lock().unwrap().take().unwrap();
            let root = self.root.clone();
            crate::files::blocking(context, move |context| {
                std::fs::write(root.join("started"), b"started").unwrap();
                receiver.recv().unwrap();
                std::fs::write(root.join("mutation"), b"settled mutation").unwrap();
                context.check()
            })
            .await
        };
        std::fs::write(self.root.join("settled"), b"settled").unwrap();
        result
    }
}

#[async_trait]
impl Workspaces for Settling {
    fn plan(&self, project: &Project, owner: &str) -> Workspace {
        self.git.plan(project, owner)
    }
    async fn register(&self, context: &Context, input: &RegisterProject) -> Result<Project> {
        self.effect(context, "register").await?;
        self.git.register(context, input).await
    }
    async fn ensure(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<Workspace> {
        self.effect(context, "ensure").await?;
        self.git.ensure(context, project, workspace).await
    }
    async fn inspect(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        self.effect(context, "inspect").await?;
        self.git.inspect(context, project, workspace).await
    }
    async fn remove(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        self.effect(context, "remove").await?;
        self.git.remove(context, project, workspace).await
    }
}

async fn settlement(stage: &'static str, command: bool, trigger: &str) {
    eprintln!("settlement: {stage}, command={command}, trigger={trigger}");
    let root = tempfile::tempdir().unwrap();
    let root = root.path().canonicalize().unwrap();
    let repo = root.join("repo");
    std::fs::create_dir(&repo).unwrap();
    for args in [
        vec!["init", "-b", "master"],
        vec![
            "-c",
            "user.name=Fixture",
            "-c",
            "user.email=fixture@example.invalid",
            "commit",
            "--allow-empty",
            "-m",
            "fixture",
        ],
    ] {
        assert!(
            Command::new("git")
                .current_dir(&repo)
                .args(args)
                .env_clear()
                .env("PATH", std::env::var_os("PATH").unwrap_or_default())
                .env("HOME", &root)
                .env("GIT_CONFIG_NOSYSTEM", "1")
                .env("GIT_CONFIG_COUNT", "2")
                .env("GIT_CONFIG_KEY_0", "core.hooksPath")
                .env("GIT_CONFIG_VALUE_0", "/dev/null")
                .env("GIT_CONFIG_KEY_1", "core.fsmonitor")
                .env("GIT_CONFIG_VALUE_1", "false")
                .output()
                .unwrap()
                .status
                .success()
        );
    }
    let store = Arc::new(Sqlite::open(root.join("state")).unwrap());
    let (sender, receiver) = mpsc::channel();
    let release = Arc::new(Notify::new());
    let workspaces = Arc::new(Settling {
        git: Git::new(root.join("trees"), false).unwrap(),
        root: root.clone(),
        stage,
        command,
        armed: AtomicBool::new(false),
        release: release.clone(),
        receiver: Mutex::new(Some(receiver)),
    });
    let engine = Engine::new(
        store.clone(),
        Arc::new(Idle),
        workspaces.clone(),
        Options::default(),
    );
    let input = RegisterProject {
        name: "Fixture".into(),
        path: repo.to_str().unwrap().into(),
        base_ref: "master".into(),
        ..Default::default()
    };
    let background = Context::background();
    if stage != "register" {
        let project = engine
            .register(&background, "register", input.clone())
            .await
            .unwrap();
        let task = engine
            .create(
                &background,
                "create",
                CreateTask {
                    project_id: project.id,
                    title: "Fixture".into(),
                    instructions: "No model".into(),
                    ..Default::default()
                },
            )
            .await
            .unwrap();
        if stage == "remove" {
            engine.tick(&background).await.unwrap();
            engine
                .control(
                    &background,
                    "cancel",
                    &task.id,
                    Control {
                        action: "cancel".into(),
                        ..Default::default()
                    },
                )
                .await
                .unwrap();
            engine.tick(&background).await.unwrap();
            let attempt = store.snapshot().unwrap().tasks[&task.id].attempt_id.clone();
            engine
                .cleanup(&background, "cleanup", &attempt)
                .await
                .unwrap();
        }
    }
    workspaces.armed.store(true, Ordering::SeqCst);
    let context = if trigger == "deadline" {
        Context::background().with_timeout(Duration::from_secs(2))
    } else {
        Context::background()
    };
    let caller_engine = engine.clone();
    let caller_context = context.clone();
    let caller = tokio::spawn(async move {
        if stage == "register" {
            caller_engine
                .register(&caller_context, "register", input)
                .await
                .map(|_| ())
        } else {
            caller_engine.tick(&caller_context).await
        }
    });
    tokio::time::timeout(Duration::from_secs(5), async {
        while !root.join("started").exists() {
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    })
    .await
    .unwrap();
    match trigger {
        "cancel" => {
            context.cancel();
            assert_eq!(caller.await.unwrap().unwrap_err().code, "canceled");
        }
        "deadline" => assert_eq!(caller.await.unwrap().unwrap_err().code, "timeout"),
        "abandon" => {
            caller.abort();
            let _ = caller.await;
        }
        _ => unreachable!(),
    }
    let shutdown_engine = engine.clone();
    let mut shutdown = tokio::spawn(async move { shutdown_engine.shutdown().await });
    let early = tokio::time::timeout(Duration::from_millis(50), &mut shutdown)
        .await
        .is_ok();
    let before = store.snapshot().unwrap();
    release.notify_one();
    if !command {
        sender.send(()).unwrap();
    }
    if !early {
        tokio::time::timeout(Duration::from_secs(5), shutdown)
            .await
            .unwrap()
            .unwrap();
    }
    if !command {
        tokio::time::timeout(Duration::from_secs(5), async {
            while !root.join("mutation").exists() {
                tokio::time::sleep(Duration::from_millis(5)).await;
            }
        })
        .await
        .unwrap();
    }
    assert!(
        !early,
        "{stage}/{command}/{trigger}: shutdown released unsettled workspace ownership"
    );
    assert!(root.join("settled").is_file());
    if command {
        let pid = std::fs::read_to_string(root.join("child")).unwrap();
        assert!(
            !Command::new("ps")
                .args(["-p", pid.trim(), "-o", "pid="])
                .output()
                .unwrap()
                .status
                .success()
        );
    } else {
        assert_eq!(
            std::fs::read(root.join("mutation")).unwrap(),
            b"settled mutation"
        );
    }
    if stage != "register" {
        let attempt = before.attempts.values().next().unwrap();
        assert!(
            attempt.error.is_empty(),
            "bookkeeping ran before settlement"
        );
        if stage == "remove" {
            assert_eq!(attempt.phase, "cleanup_pending");
        }
    }
    let after = store.snapshot().unwrap();
    tokio::time::sleep(Duration::from_millis(25)).await;
    assert_eq!(store.snapshot().unwrap(), after);
}

#[tokio::test]
async fn engine_retains_all_workspace_operations_until_effects_settle() {
    for stage in ["register", "ensure", "inspect", "remove"] {
        for command in [false, true] {
            for trigger in ["cancel", "deadline", "abandon"] {
                settlement(stage, command, trigger).await;
            }
        }
    }
}
