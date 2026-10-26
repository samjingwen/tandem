// Test-only simulated execution. No discovery, real runtime, filesystem checkout
// allocation, credential access, or process/service lifecycle operations.
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};

use async_trait::async_trait;
use tandem_core::*;
use tandem_engine::{Engine, Options};
use tandem_store::Sqlite;
use tempfile::TempDir;
use tokio::sync::Semaphore;

#[derive(Default)]
pub struct Native {
    pub sessions: BTreeMap<String, Observation>,
    pub calls: Vec<(String, String)>,
    pub prompt_error: Option<Error>,
    pub ensure_error: Option<Error>,
    pub reply_error: Option<Error>,
    pub reconcile_error: Option<Error>,
    pub background_busy: bool,
    pub incompatible: bool,
    pub lose_ensure_response: bool,
}

pub struct FakeRuntime {
    pub state: Mutex<Native>,
    pub store: Arc<Sqlite>,
    pub entered: Semaphore,
    pub release: Semaphore,
    pub block_prompt: std::sync::atomic::AtomicBool,
    pub block_info: std::sync::atomic::AtomicBool,
    pub info_entered: Semaphore,
    pub info_release: Semaphore,
}

impl FakeRuntime {
    pub fn calls(&self, name: &str) -> usize {
        self.state
            .lock()
            .unwrap()
            .calls
            .iter()
            .filter(|(call, _)| call == name)
            .count()
    }
    pub fn observe(&self, reference: &str, state: &str) {
        self.state
            .lock()
            .unwrap()
            .sessions
            .get_mut(reference)
            .unwrap()
            .state = state.into();
    }
    pub fn decision(&self, reference: &str, kind: &str) {
        self.state
            .lock()
            .unwrap()
            .sessions
            .get_mut(reference)
            .unwrap()
            .decisions = vec![Decision {
            runtime_ref: "form-1".into(),
            kind: kind.into(),
            prompt: "Actual question".into(),
            ..Default::default()
        }];
    }
    fn check_owner(&self, spec: &SessionSpec) {
        let state = self.store.snapshot().unwrap();
        let attempt = &state.attempts[&spec.attempt_id];
        assert_eq!(attempt.session_ref, spec.reference);
        assert_eq!(state.tasks[&spec.task_id].attempt_id, attempt.id);
    }
}

#[async_trait]
impl Runtime for FakeRuntime {
    fn reference(&self, attempt: &str) -> String {
        format!("session/{attempt}")
    }
    fn capabilities(&self) -> RuntimeCapabilities {
        RuntimeCapabilities {
            reconcile_prompt: true,
            cancel_pending: true,
            quiescence: true,
        }
    }
    async fn info(&self, context: &Context) -> Result<RuntimeInfo> {
        if self.block_info.load(std::sync::atomic::Ordering::Acquire) {
            self.info_entered.add_permits(1);
            self.info_release.acquire().await.unwrap().forget();
        }
        context.check()?;
        Ok(RuntimeInfo {
            id: "fake".into(),
            version: "fixture".into(),
            compatible: !self.state.lock().unwrap().incompatible,
            ..Default::default()
        })
    }
    async fn ensure(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        self.check_owner(spec);
        let mut state = self.state.lock().unwrap();
        state.calls.push(("ensure".into(), spec.reference.clone()));
        if let Some(error) = &state.ensure_error {
            return Err(error.clone());
        }
        state
            .sessions
            .entry(spec.reference.clone())
            .or_insert(Observation {
                exists: true,
                owned: true,
                state: "idle".into(),
                ..Default::default()
            });
        if state.lose_ensure_response {
            state.lose_ensure_response = false;
            return Err(Error::uncertain());
        }
        Ok(())
    }
    async fn inspect(&self, context: &Context, spec: &SessionSpec) -> Result<Observation> {
        context.check()?;
        Ok(self
            .state
            .lock()
            .unwrap()
            .sessions
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
        self.check_owner(spec);
        assert_eq!(
            self.store.snapshot()?.commands[command].status,
            "sending",
            "admission must be durable before mutation"
        );
        {
            let mut state = self.state.lock().unwrap();
            state.calls.push(("prompt".into(), command.into()));
        }
        if self.block_prompt.load(std::sync::atomic::Ordering::Acquire) {
            self.entered.add_permits(1);
            self.release.acquire().await.unwrap().forget();
        }
        context.check()?;
        let mut state = self.state.lock().unwrap();
        if let Some(error) = &state.prompt_error {
            return Err(error.clone());
        }
        state.sessions.get_mut(&spec.reference).unwrap().state = "running".into();
        Ok(())
    }
    async fn reconcile_prompt(
        &self,
        context: &Context,
        _spec: &SessionSpec,
        command: &str,
        _text: &str,
    ) -> Result<()> {
        context.check()?;
        let mut state = self.state.lock().unwrap();
        state.calls.push(("reconcile".into(), command.into()));
        if let Some(error) = &state.reconcile_error {
            return Err(error.clone());
        }
        Ok(())
    }
    async fn interrupt(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        context.check()?;
        let mut state = self.state.lock().unwrap();
        state
            .calls
            .push(("interrupt".into(), spec.reference.clone()));
        state.sessions.get_mut(&spec.reference).unwrap().state = "idle".into();
        Ok(())
    }
    async fn cancel_pending(&self, context: &Context, spec: &SessionSpec) -> Result<()> {
        self.interrupt(context, spec).await?;
        self.state
            .lock()
            .unwrap()
            .calls
            .push(("cancel".into(), spec.reference.clone()));
        Ok(())
    }
    async fn check_quiescence(&self, context: &Context, _spec: &SessionSpec) -> Result<()> {
        context.check()?;
        if self.state.lock().unwrap().background_busy {
            return Err(Error::invalid("owned background work is active"));
        }
        Ok(())
    }
    async fn reply(
        &self,
        context: &Context,
        spec: &SessionSpec,
        decision: &Decision,
        _answer: &RawJson,
    ) -> Result<()> {
        context.check()?;
        assert_eq!(
            self.store.snapshot()?.decisions[&decision.id].status,
            "uncertain"
        );
        let mut state = self.state.lock().unwrap();
        state.calls.push(("reply".into(), decision.id.clone()));
        if let Some(error) = &state.reply_error {
            return Err(error.clone());
        }
        let observation = state.sessions.get_mut(&spec.reference).unwrap();
        observation.decisions.clear();
        observation.state = "running".into();
        Ok(())
    }
}

pub struct FakeWorkspaces {
    pub store: Arc<Sqlite>,
    pub calls: Mutex<Vec<String>>,
    pub error: Mutex<Option<Error>>,
}

#[async_trait]
impl Workspaces for FakeWorkspaces {
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
            instructions: input.instructions.clone(),
            instruction_files: input.instruction_files.clone(),
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
        assert_eq!(
            self.store.snapshot()?.attempts[&workspace.owner].workspace,
            *workspace
        );
        self.calls.lock().unwrap().push("ensure".into());
        if let Some(error) = self.error.lock().unwrap().clone() {
            return Err(error);
        }
        Ok(Workspace {
            allocated: true,
            base_sha: "fixture-sha".into(),
            ..workspace.clone()
        })
    }
    async fn inspect(
        &self,
        context: &Context,
        _project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        context.check()?;
        assert!(workspace.allocated);
        Ok(())
    }
    async fn remove(
        &self,
        context: &Context,
        _project: &Project,
        workspace: &Workspace,
    ) -> Result<()> {
        context.check()?;
        assert_eq!(
            self.store.snapshot()?.attempts[&workspace.owner].phase,
            "cleanup_pending"
        );
        self.calls.lock().unwrap().push("remove".into());
        Ok(())
    }
}

pub struct Fixture {
    pub _root: TempDir,
    pub store: Arc<Sqlite>,
    pub runtime: Arc<FakeRuntime>,
    pub workspaces: Arc<FakeWorkspaces>,
    pub engine: Engine,
    pub context: Context,
    pub project: Project,
}

impl Fixture {
    pub async fn new(options: Options) -> Self {
        let root = TempDir::new().unwrap();
        let store = Arc::new(Sqlite::open(root.path().join("state")).unwrap());
        let runtime = Arc::new(FakeRuntime {
            state: Mutex::new(Native::default()),
            store: store.clone(),
            entered: Semaphore::new(0),
            release: Semaphore::new(0),
            block_prompt: std::sync::atomic::AtomicBool::new(false),
            block_info: std::sync::atomic::AtomicBool::new(false),
            info_entered: Semaphore::new(0),
            info_release: Semaphore::new(0),
        });
        let workspaces = Arc::new(FakeWorkspaces {
            store: store.clone(),
            calls: Mutex::new(Vec::new()),
            error: Mutex::new(None),
        });
        let engine = Engine::new(
            store.clone(),
            runtime.clone(),
            workspaces.clone(),
            Options {
                runtime_id: "fake".into(),
                ..options
            },
        );
        let context = Context::background();
        let project = engine
            .register(
                &context,
                "register",
                RegisterProject {
                    name: "Fixture".into(),
                    path: "/synthetic/project".into(),
                    base_ref: "master".into(),
                    ..Default::default()
                },
            )
            .await
            .unwrap();
        Self {
            _root: root,
            store,
            runtime,
            workspaces,
            engine,
            context,
            project,
        }
    }
    pub async fn create(&self, operation: &str) -> Task {
        self.create_child(operation, "", "worker").await
    }
    pub async fn create_child(&self, operation: &str, parent: &str, role: &str) -> Task {
        self.engine
            .create(
                &self.context,
                operation,
                CreateTask {
                    project_id: self.project.id.clone(),
                    parent_id: parent.into(),
                    title: operation.into(),
                    instructions: "Implement a controlled fixture".into(),
                    role: role.into(),
                    ..Default::default()
                },
            )
            .await
            .unwrap()
    }
    pub fn state(&self) -> State {
        self.store.snapshot().unwrap()
    }
    pub async fn tick(&self) {
        self.engine.tick(&self.context).await.unwrap();
    }
    pub fn attempt(&self, task: &Task) -> Attempt {
        let state = self.state();
        state.attempts[&state.tasks[&task.id].attempt_id].clone()
    }
    pub fn result(&self, task: &Task) -> SubmitResult {
        let task = self.state().tasks[&task.id].clone();
        SubmitResult {
            attempt_id: task.attempt_id,
            generation: task.generation,
            summary: "Verified fixture".into(),
            evidence: vec![EvidenceInput {
                name: "test".into(),
                reference: "fixture://assertion".into(),
                passed: true,
            }],
        }
    }
    pub async fn control(&self, task: &Task, action: &str, cascade: bool) -> Result<Task> {
        self.engine
            .control(
                &self.context,
                &format!("{action}-{}", task.id),
                &task.id,
                Control {
                    action: action.into(),
                    cascade,
                },
            )
            .await
    }
}
