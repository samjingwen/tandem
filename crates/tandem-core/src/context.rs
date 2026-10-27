use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use event_listener::Event;

use crate::{Error, Result};

struct Cancellation {
    requested: AtomicBool,
    changed: Event,
}

/// Cooperative cancellation/deadline state, independent of an executor or HTTP
/// request lifetime. Dropping a client future does not erase admitted effects.
#[derive(Clone)]
pub struct Context {
    cancellation: Arc<Cancellation>,
    deadline: Option<Instant>,
}

impl Default for Context {
    fn default() -> Self {
        Self::background()
    }
}

impl Context {
    pub fn background() -> Self {
        Self {
            cancellation: Arc::new(Cancellation {
                requested: AtomicBool::new(false),
                changed: Event::new(),
            }),
            deadline: None,
        }
    }

    /// Derives a smaller deadline without changing the parent's deadline or
    /// adding cancel-on-drop behavior. Explicit cancellation remains shared.
    pub fn with_timeout(&self, timeout: Duration) -> Self {
        let requested = Instant::now().checked_add(timeout);
        Self {
            cancellation: self.cancellation.clone(),
            deadline: match (self.deadline, requested) {
                (Some(parent), Some(child)) => Some(parent.min(child)),
                (parent, child) => parent.or(child),
            },
        }
    }

    pub fn detached(timeout: Duration) -> Self {
        Self::background().with_timeout(timeout)
    }

    pub fn deadline(&self) -> Option<Instant> {
        self.deadline
    }

    pub fn cancel(&self) {
        self.cancellation.requested.store(true, Ordering::Release);
        self.cancellation.changed.notify(usize::MAX);
    }

    pub fn check(&self) -> Result<()> {
        if self.cancellation.requested.load(Ordering::Acquire) {
            return Err(Error::new("canceled", "operation canceled"));
        }
        if self
            .deadline
            .is_some_and(|deadline| Instant::now() >= deadline)
        {
            return Err(Error::new("timeout", "operation deadline exceeded"));
        }
        Ok(())
    }

    /// Wakes on explicit cancellation; an executor adapter is responsible for
    /// selecting its own timer against deadline(). Listening before checking the
    /// flag prevents a lost cancellation notification.
    pub async fn cancellation_requested(&self) {
        loop {
            let listener = self.cancellation.changed.listen();
            if self.cancellation.requested.load(Ordering::Acquire) {
                return;
            }
            listener.await;
        }
    }
}
