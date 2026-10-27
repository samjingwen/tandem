//! Actual Rust engine + SQLite + Git workspace composition. Only model/runtime
//! execution is simulated; no native OpenCode or existing repository is used.

#![cfg(any(target_os = "linux", target_os = "macos"))]

use std::{
    collections::BTreeSet,
    path::Path,
    process::Command,
    sync::{Arc, Mutex},
};

use async_trait::async_trait;
use tandem_core::*;
use tandem_engine::{Engine, Options};
use tandem_store::Sqlite;
use tandem_workspace::Git;

struct OwnedWorkspaces {
    git: Git,
    store: Arc<Sqlite>,
}

#[async_trait]
impl Workspaces for OwnedWorkspaces {
    fn plan(&self, project: &Project, owner: &str) -> Workspace {
        self.git.plan(project, owner)
    }
    async fn register(&self, context: &Context, input: &RegisterProject) -> Result<Project> {
        self.git.register(context, input).await
    }
    async fn ensure(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<Workspace> {
        let state = self.store.snapshot()?;
        let attempt = &state.attempts[&workspace.owner];
        assert_eq!(
            attempt.workspace, *workspace,
            "ownership must be durable before Git allocation"
        );
        assert_eq!(state.tasks[&attempt.task_id].attempt_id, attempt.id);
        self.git.ensure(context, project, workspace).await
    }
    async fn inspect(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        self.git.inspect(context, project, workspace).await
    }
    async fn remove(
        &self,
        context: &Context,
        project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        assert_eq!(
            self.store.snapshot()?.attempts[&workspace.owner].phase,
            "cleanup_pending"
        );
        self.git.remove(context, project, workspace).await
    }
}

struct Execution {
    store: Arc<Sqlite>,
    sessions: Mutex<BTreeSet<String>>,
}

#[async_trait]
impl Runtime for Execution {
    fn reference(&self, attempt: &str) -> String {
        format!("fixture/{attempt}")
    }
    fn capabilities(&self) -> RuntimeCapabilities {
        RuntimeCapabilities {
            quiescence: true,
            ..Default::default()
        }
    }
    async fn info(&self, context: &Context) -> Result<RuntimeInfo> {
        context.check()?;
        Ok(RuntimeInfo {
            id: "fixture".into(),
            compatible: true,
            ..Default::default()
        })
    }
    async fn ensure(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        let state = self.store.snapshot()?;
        let attempt = &state.attempts[&spec.attempt_id];
        assert!(attempt.workspace.allocated);
        assert_eq!(attempt.session_ref, spec.reference);
        assert_eq!(attempt.workspace.path, spec.directory);
        assert!(Path::new(&spec.directory).join(".git").is_file());
        self.sessions.lock().unwrap().insert(spec.reference.clone());
        Ok(())
    }
    async fn inspect(&self, context: &Context, spec: &SessionSpec) -> Result<Observation> {
        context.check()?;
        let exists = self.sessions.lock().unwrap().contains(&spec.reference);
        Ok(Observation {
            exists,
            owned: exists,
            state: "idle".into(),
            ..Default::default()
        })
    }
    async fn prompt(
        &self,
        context: &Context,
        _spec: &SessionSpec,
        command: &str,
        _text: &str,
    ) -> Result<()> {
        context.check()?;
        assert_eq!(self.store.snapshot()?.commands[command].status, "sending");
        Ok(())
    }
    async fn interrupt(&self, context: &Context, _spec: &SessionSpec) -> Result<()> {
        context.check()
    }
    async fn reply(
        &self,
        _context: &Context,
        _spec: &SessionSpec,
        _decision: &Decision,
        _answer: &RawJson,
    ) -> Result<()> {
        Err(Error::invalid("fixture has no decisions"))
    }
    async fn check_quiescence(&self, context: &Context, _spec: &SessionSpec) -> Result<()> {
        context.check()
    }
}

fn git(path: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .current_dir(path)
        .args(args)
        .env_clear()
        .env("PATH", std::env::var_os("PATH").unwrap_or_default())
        .env("HOME", path)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_COUNT", "2")
        .env("GIT_CONFIG_KEY_0", "core.hooksPath")
        .env("GIT_CONFIG_VALUE_0", "/dev/null")
        .env("GIT_CONFIG_KEY_1", "core.fsmonitor")
        .env("GIT_CONFIG_VALUE_1", "false")
        .output()
        .unwrap();
    assert!(output.status.success(), "fixture Git failed");
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}

#[tokio::test]
async fn durable_engine_ownership_precedes_real_allocation_and_cleanup() {
    let root = tempfile::tempdir().unwrap();
    let root = root.path().canonicalize().unwrap();
    let repo = root.join("repo");
    std::fs::create_dir(&repo).unwrap();
    git(&repo, &["init", "-b", "master"]);
    git(
        &repo,
        &[
            "-c",
            "user.name=Fixture",
            "-c",
            "user.email=fixture@example.invalid",
            "commit",
            "--allow-empty",
            "-m",
            "fixture",
        ],
    );
    let store = Arc::new(Sqlite::open(root.join("state")).unwrap());
    let execution = Arc::new(Execution {
        store: store.clone(),
        sessions: Mutex::new(BTreeSet::new()),
    });
    let workspaces = Arc::new(OwnedWorkspaces {
        git: Git::new(root.join("trees"), false).unwrap(),
        store: store.clone(),
    });
    let engine = Engine::new(
        store.clone(),
        execution,
        workspaces,
        Options {
            runtime_id: "fixture".into(),
            ..Default::default()
        },
    );
    let context = Context::background();
    let project = engine
        .register(
            &context,
            "register",
            RegisterProject {
                name: "Fixture".into(),
                path: repo.to_str().unwrap().into(),
                base_ref: "master".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    let task = engine
        .create(
            &context,
            "create",
            CreateTask {
                project_id: project.id,
                title: "Controlled workspace fixture".into(),
                instructions: "No native execution".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    engine.tick(&context).await.unwrap();
    let state = store.snapshot().unwrap();
    let attempt = state.attempts[&state.tasks[&task.id].attempt_id].clone();
    assert_eq!(attempt.phase, "executing");
    assert!(attempt.workspace.allocated);
    engine
        .control(
            &context,
            "cancel",
            &task.id,
            Control {
                action: "cancel".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    engine.tick(&context).await.unwrap();
    assert_eq!(store.snapshot().unwrap().tasks[&task.id].state, "cancelled");
    let receipt = engine
        .cleanup(&context, "cleanup", &attempt.id)
        .await
        .unwrap();
    engine.tick(&context).await.unwrap();
    assert_eq!(
        store.snapshot().unwrap().attempts[&attempt.id].phase,
        "cleaned"
    );
    assert!(!Path::new(&attempt.workspace.path).exists());
    assert_eq!(
        git(&repo, &["rev-parse", &attempt.workspace.branch]),
        attempt.workspace.base_sha
    );
    assert_eq!(
        engine
            .cleanup(&context, "cleanup", &attempt.id)
            .await
            .unwrap(),
        receipt,
        "receipt must not become the latest cleanup record"
    );
    engine.shutdown().await;
}
