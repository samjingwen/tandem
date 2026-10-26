use super::support;

use std::sync::{
    Arc, Condvar, Mutex,
    atomic::{AtomicBool, Ordering},
};
use std::time::Duration;

use tandem_core::*;
use tandem_engine::{Engine, Options};
use tandem_store::Sqlite;
use tokio::sync::Semaphore;

// Delay only the first post-dispatch transaction before invoking its callback.
// This models waiting behind the storage owner, not a native fixture or retry.
struct DelayedStore {
    store: Arc<Sqlite>,
    once: AtomicBool,
    entered: Semaphore,
    release: (Mutex<bool>, Condvar),
}

impl Store for DelayedStore {
    fn snapshot(&self) -> Result<State> {
        self.store.snapshot()
    }
    fn read_revision(&self) -> Result<Revision> {
        self.store.read_revision()
    }
    fn update(&self, change: &mut dyn FnMut(&mut State) -> Result<()>) -> Result<()> {
        if self
            .store
            .snapshot()?
            .commands
            .values()
            .any(|command| command.status == "sending")
            && self.once.swap(false, Ordering::AcqRel)
        {
            self.entered.add_permits(1);
            let (mutex, condition) = &self.release;
            let mut released = mutex.lock().unwrap();
            while !*released {
                released = condition.wait(released).unwrap();
            }
        }
        self.store.update(change)
    }
}

#[tokio::test]
async fn bookkeeping_deadline_expires_without_releasing_effect_ownership_early() {
    let f = support::Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.engine.shutdown().await;
    let store = Arc::new(DelayedStore {
        store: f.store.clone(),
        once: AtomicBool::new(true),
        entered: Semaphore::new(0),
        release: (Mutex::new(false), Condvar::new()),
    });
    let engine = Engine::new(
        store.clone(),
        f.runtime.clone(),
        f.workspaces.clone(),
        Options {
            runtime_id: "fake".into(),
            ..Default::default()
        },
    );
    let owner = engine.clone();
    let waiter = tokio::spawn(async move { owner.tick(&Context::background()).await });
    tokio::time::timeout(Duration::from_secs(2), store.entered.acquire())
        .await
        .unwrap()
        .unwrap()
        .forget();
    waiter.abort();
    let _ = waiter.await;
    let owner = engine.clone();
    let task_id = task.id.clone();
    let control = tokio::spawn(async move {
        owner
            .control(
                &Context::background(),
                "pause",
                &task_id,
                Control {
                    action: "pause".into(),
                    cascade: false,
                },
            )
            .await
    });
    tokio::time::sleep(Duration::from_millis(5100)).await;
    let held = f.state().tasks[&task.id].desired == "active";
    *store.release.0.lock().unwrap() = true;
    store.release.1.notify_all();
    control.await.unwrap().unwrap();
    engine.shutdown().await;
    assert!(held, "control crossed in-flight bookkeeping");
    let state = f.state();
    assert!(
        state
            .commands
            .values()
            .any(|command| command.status == "sending"),
        "expired bookkeeping fabricated an admission receipt"
    );
    assert_eq!(f.runtime.calls("prompt"), 1);
}
