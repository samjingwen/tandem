#[path = "support/bookkeeping.rs"]
mod bookkeeping;
mod support;

use std::{sync::atomic::Ordering, time::Duration};
use support::Fixture;
use tandem_core::*;
use tandem_engine::Options;

#[tokio::test]
async fn admission_replay_generation_acceptance_and_cleanup_are_durable() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.tick().await;
    let attempt = f.attempt(&task);
    assert_eq!(f.runtime.calls("ensure"), 1);
    assert_eq!(f.runtime.calls("prompt"), 1);
    assert_eq!(attempt.phase, "executing");
    assert_eq!(f.state().tasks[&task.id].state, "running");
    assert!(
        f.engine
            .accept(&f.context, "idle-not-evidence", &task.id)
            .await
            .is_err()
    );
    let follow = f
        .engine
        .followup(
            &f.context,
            "follow",
            &task.id,
            Followup {
                text: "Check Unicode <&> 世界".into(),
            },
        )
        .await
        .unwrap();
    assert_eq!(follow.generation, 2);
    f.tick().await;
    let mut stale = f.result(&task);
    stale.generation = 1;
    assert!(
        f.engine
            .submit_result(&f.context, "stale", &task.id, stale)
            .await
            .unwrap()
            .late
    );
    f.engine
        .submit_result(&f.context, "result", &task.id, f.result(&task))
        .await
        .unwrap();
    f.runtime.observe(&attempt.session_ref, "idle");
    f.tick().await;
    let accepted = f
        .engine
        .accept(&f.context, "accept", &task.id)
        .await
        .unwrap();
    assert_eq!(accepted.state, "completed");
    let receipt = f
        .engine
        .cleanup(&f.context, "cleanup", &attempt.id)
        .await
        .unwrap();
    assert!(receipt.allocated);
    f.tick().await;
    assert_eq!(f.attempt(&task).phase, "cleaned");
    let revision = f.state().revision;
    assert_eq!(
        f.engine
            .cleanup(&f.context, "cleanup", &attempt.id)
            .await
            .unwrap(),
        receipt
    );
    assert!(
        f.engine
            .cleanup(&f.context, "other-cleanup", &attempt.id)
            .await
            .is_err()
    );
    let replay = f
        .engine
        .followup(
            &f.context,
            "follow",
            &task.id,
            Followup {
                text: "Check Unicode <&> 世界".into(),
            },
        )
        .await
        .unwrap();
    assert_eq!(replay, follow);
    assert_eq!(f.state().revision, revision);
    assert_eq!(
        f.engine
            .followup(
                &f.context,
                "follow",
                &task.id,
                Followup {
                    text: "different".into()
                }
            )
            .await
            .unwrap_err()
            .code,
        "operation_conflict"
    );
    f.engine.shutdown().await;
}

#[tokio::test]
async fn ambiguous_prompt_holds_capacity_and_never_speculatively_resends() {
    let f = Fixture::new(Options {
        concurrency: 1,
        ..Default::default()
    })
    .await;
    f.runtime.state.lock().unwrap().prompt_error = Some(Error::uncertain());
    f.runtime.state.lock().unwrap().reconcile_error = Some(Error::uncertain());
    let first = f.create("first").await;
    let second = f.create("second").await;
    f.tick().await;
    let attempt = f.attempt(&first);
    f.runtime.observe(&attempt.session_ref, "idle");
    for _ in 0..10 {
        f.tick().await;
    }
    assert_eq!(f.runtime.calls("prompt"), 1);
    assert_eq!(f.runtime.calls("ensure"), 1);
    assert!(f.state().tasks[&second.id].attempt_id.is_empty());
    assert_eq!(f.state().tasks[&first.id].state, "recovering");
    assert_eq!(f.attempt(&first).failure_count, 8);
    f.control(&first, "cancel", false).await.unwrap();
    f.tick().await;
    assert_eq!(f.state().tasks[&first.id].state, "recovering");
    f.runtime.state.lock().unwrap().reconcile_error = None;
    f.runtime.state.lock().unwrap().prompt_error = None;
    f.tick().await;
    assert_eq!(f.state().tasks[&first.id].state, "cancelled");
    assert!(!f.state().tasks[&second.id].attempt_id.is_empty());
    assert_eq!(f.runtime.calls("prompt"), 2);
    f.engine.shutdown().await;
}

#[tokio::test]
async fn proven_non_delivery_has_a_finite_budget_and_explicit_resume() {
    let f = Fixture::new(Options::default()).await;
    f.runtime.state.lock().unwrap().prompt_error =
        Some(Error::not_delivered(Error::new("unavailable", "not sent")));
    let task = f.create("task").await;
    for _ in 0..12 {
        f.tick().await;
    }
    assert_eq!(f.runtime.calls("prompt"), 8);
    assert!(
        f.state().tasks[&task.id]
            .error
            .contains("retry budget exhausted")
    );
    f.control(&task, "resume", false).await.unwrap();
    f.runtime.state.lock().unwrap().prompt_error = None;
    f.tick().await;
    assert_eq!(f.attempt(&task).failure_count, 0);
    assert_eq!(f.attempt(&task).phase, "executing");
    f.engine.shutdown().await;
}

#[tokio::test]
async fn decisions_require_explicit_valid_answers_and_uncertainty_blocks_cleanup() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.tick().await;
    let attempt = f.attempt(&task);
    f.runtime.decision(&attempt.session_ref, "permission");
    f.tick().await;
    let decision = f.state().decisions.values().next().unwrap().clone();
    assert_eq!(f.runtime.calls("reply"), 0);
    for (op, answer) in [
        ("missing", None),
        ("null", Some("null")),
        ("unknown", Some(r#"{"decision":"once","unexpected":true}"#)),
    ] {
        let input = ReplyDecision {
            answer: answer.map(|text| RawJson::parse(text.into()).unwrap()),
        };
        assert!(
            f.engine
                .reply(&f.context, op, &decision.id, input)
                .await
                .is_err()
        );
    }
    let input = ReplyDecision {
        answer: Some(RawJson::parse(r#"{"decision":"once"}"#.into()).unwrap()),
    };
    f.engine
        .reply(&f.context, "reply", &decision.id, input)
        .await
        .unwrap();
    f.runtime.state.lock().unwrap().reply_error = Some(Error::uncertain());
    f.tick().await;
    assert_eq!(f.state().decisions[&decision.id].status, "uncertain");
    for _ in 0..3 {
        f.tick().await;
    }
    assert_eq!(f.runtime.calls("reply"), 1);
    assert!(f.control(&task, "retry", false).await.is_err());
    assert!(
        f.engine
            .cleanup(&f.context, "cleanup", &attempt.id)
            .await
            .is_err()
    );
    f.engine.shutdown().await;
}

#[tokio::test]
async fn failed_attempt_retry_and_cleanup_require_background_quiescence() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.tick().await;
    let attempt = f.attempt(&task);
    f.runtime.observe(&attempt.session_ref, "failed");
    f.tick().await;
    assert_eq!(f.state().tasks[&task.id].state, "failed");
    f.runtime.state.lock().unwrap().background_busy = true;
    assert!(f.control(&task, "retry", false).await.is_err());
    assert!(
        f.engine
            .cleanup(&f.context, "cleanup", &attempt.id)
            .await
            .is_err()
    );
    f.runtime.state.lock().unwrap().background_busy = false;
    f.control(&task, "retry", false).await.unwrap();
    f.tick().await;
    assert_ne!(f.attempt(&task).id, attempt.id);
    assert_eq!(f.state().attempts.len(), 2);
    f.engine.shutdown().await;
}

#[tokio::test]
async fn dropping_the_client_wait_does_not_drop_owned_effect_or_control_barrier() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.runtime.block_prompt.store(true, Ordering::Release);
    let engine = f.engine.clone();
    let context = f.context.clone();
    let waiter = tokio::spawn(async move { engine.tick(&context).await });
    tokio::time::timeout(Duration::from_secs(2), f.runtime.entered.acquire())
        .await
        .unwrap()
        .unwrap()
        .forget();
    waiter.abort();
    let _ = waiter.await;
    let engine = f.engine.clone();
    let context = f.context.clone();
    let task_id = task.id.clone();
    let control = tokio::spawn(async move {
        engine
            .control(
                &context,
                "pause",
                &task_id,
                Control {
                    action: "pause".into(),
                    cascade: false,
                },
            )
            .await
    });
    assert!(
        tokio::time::timeout(Duration::from_millis(30), async {
            while f.state().tasks[&task.id].desired == "active" {
                tokio::task::yield_now().await;
            }
        })
        .await
        .is_err()
    );
    f.runtime.release.add_permits(1);
    control.await.unwrap().unwrap();
    assert_eq!(
        f.state()
            .commands
            .values()
            .find(|command| command.kind == "prompt")
            .unwrap()
            .status,
        "admitted"
    );
    assert_eq!(f.runtime.calls("prompt"), 1);
    f.tick().await;
    assert_eq!(f.state().tasks[&task.id].state, "paused");
    f.engine.shutdown().await;
}

#[tokio::test]
async fn cancellation_books_uncertainty_and_shutdown_waits_for_owned_operations() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.runtime.block_prompt.store(true, Ordering::Release);
    let context = Context::background();
    let tick_context = context.clone();
    let engine = f.engine.clone();
    let waiter = tokio::spawn(async move { engine.tick(&tick_context).await });
    tokio::time::timeout(Duration::from_secs(2), f.runtime.entered.acquire())
        .await
        .unwrap()
        .unwrap()
        .forget();
    context.cancel();
    assert!(waiter.await.unwrap().is_err());
    tokio::time::timeout(Duration::from_secs(2), f.engine.shutdown())
        .await
        .unwrap();
    assert_eq!(f.state().tasks[&task.id].state, "recovering");
    assert_eq!(
        f.state()
            .commands
            .values()
            .find(|command| command.kind == "prompt")
            .unwrap()
            .status,
        "sending"
    );
    assert_eq!(
        f.engine
            .create(&f.context, "closed", CreateTask::default())
            .await
            .unwrap_err()
            .code,
        "unavailable"
    );
}

#[tokio::test]
async fn coordinator_evidence_is_coalesced_bounded_and_unchanged_polls_are_quiet() {
    let f = Fixture::new(Options {
        max_coordinator_notifications: 1,
        ..Default::default()
    })
    .await;
    let parent = f.create_child("parent", "", "coordinator").await;
    f.tick().await;
    let child = f.create_child("child", &parent.id, "worker").await;
    f.tick().await;
    f.engine
        .submit_result(&f.context, "child-result", &child.id, f.result(&child))
        .await
        .unwrap();
    let attempt = f.attempt(&child);
    f.runtime.observe(&attempt.session_ref, "idle");
    f.tick().await;
    assert_eq!(
        f.state()
            .commands
            .values()
            .filter(|command| command.id.starts_with("coordination_"))
            .count(),
        1
    );
    f.tick().await;
    let before = f.state().revision;
    f.tick().await;
    assert_eq!(f.state().revision, before);
    f.engine
        .accept(&f.context, "accept-child", &child.id)
        .await
        .unwrap();
    f.tick().await;
    assert_eq!(
        f.state()
            .commands
            .values()
            .filter(|command| command.id.starts_with("coordination_"))
            .count(),
        1
    );
    assert_eq!(
        f.state()
            .commands
            .values()
            .filter(|command| command.kind == "signal" && command.status == "suppressed")
            .count(),
        1
    );
    assert!(
        f.state().tasks[&parent.id]
            .error
            .contains("notification budget exhausted")
    );
    f.engine.shutdown().await;
}

#[tokio::test]
async fn lifecycle_run_stops_without_interrupting_workers() {
    let f = Fixture::new(Options::default()).await;
    let engine = f.engine.clone();
    let context = Context::background();
    let runner = tokio::spawn(async move { engine.run(&context, Duration::from_millis(5)).await });
    // Allow admission before calling shutdown; the scheduler itself must leave
    // through the owner's stop signal, not require a caller-owned cancellation.
    tokio::task::yield_now().await;
    tokio::time::timeout(Duration::from_secs(2), f.engine.shutdown())
        .await
        .unwrap();
    let result = runner.await.unwrap();
    assert!(result.is_ok() || result.unwrap_err().code == "unavailable");
    assert_eq!(f.runtime.calls("interrupt"), 0);
}

#[tokio::test]
async fn shutdown_cancels_scheduler_reads_in_flight_without_a_client_cancel() {
    let f = Fixture::new(Options::default()).await;
    f.runtime.block_info.store(true, Ordering::Release);
    let engine = f.engine.clone();
    let runner = tokio::spawn(async move {
        engine
            .run(&Context::background(), Duration::from_secs(1))
            .await
    });
    tokio::time::timeout(Duration::from_secs(2), f.runtime.info_entered.acquire())
        .await
        .unwrap()
        .unwrap()
        .forget();
    let engine = f.engine.clone();
    let mut shutdown = tokio::spawn(async move { engine.shutdown().await });
    let stopped = tokio::time::timeout(Duration::from_millis(100), &mut shutdown)
        .await
        .is_ok();
    // Settle the disposable fixture even when reproducing the regression.
    f.runtime.info_release.add_permits(1);
    if !stopped {
        shutdown.await.unwrap();
    }
    let _ = runner.await.unwrap();
    assert!(
        stopped,
        "shutdown did not cancel a scheduler-owned read in flight"
    );
    assert_eq!(f.runtime.calls("interrupt"), 0);
}

#[tokio::test]
async fn registration_and_input_limits_are_transactional_and_graphs_stay_acyclic() {
    let f = Fixture::new(Options {
        max_depth: 1,
        ..Default::default()
    })
    .await;
    let parent = f.create("parent").await;
    let child = f.create_child("child", &parent.id, "worker").await;
    let request = CreateTask {
        project_id: f.project.id.clone(),
        parent_id: child.id.clone(),
        title: "too deep".into(),
        instructions: "instructions".into(),
        ..Default::default()
    };
    assert!(f.engine.create(&f.context, "depth", request).await.is_err());
    let request = CreateTask {
        project_id: f.project.id.clone(),
        parent_id: parent.id.clone(),
        dependencies: vec![parent.id.clone()],
        title: "cyclic".into(),
        instructions: "instructions".into(),
        ..Default::default()
    };
    assert!(f.engine.create(&f.context, "cycle", request).await.is_err());
    assert_eq!(f.state().tasks.len(), 2);
    assert!(!f.state().operations.contains_key("depth"));
    f.engine.shutdown().await;
}

#[tokio::test]
async fn lost_session_creation_response_recovers_same_reference_after_restart() {
    let f = Fixture::new(Options::default()).await;
    f.runtime.state.lock().unwrap().lose_ensure_response = true;
    let task = f.create("task").await;
    f.tick().await;
    let before = f.attempt(&task);
    assert_eq!(before.phase, "session_pending");
    assert_eq!(f.runtime.state.lock().unwrap().sessions.len(), 1);
    f.engine.shutdown().await;
    let engine = tandem_engine::Engine::new(
        f.store.clone(),
        f.runtime.clone(),
        f.workspaces.clone(),
        Options {
            runtime_id: "fake".into(),
            ..Default::default()
        },
    );
    engine.tick(&f.context).await.unwrap();
    let after = f.attempt(&task);
    assert_eq!(before.id, after.id);
    assert_eq!(before.session_ref, after.session_ref);
    assert_eq!(after.phase, "executing");
    assert_eq!(f.runtime.state.lock().unwrap().sessions.len(), 1);
    assert_eq!(f.runtime.calls("prompt"), 1);
    engine.shutdown().await;
}

#[tokio::test]
async fn independent_worker_and_coordinator_limits_and_dependency_release() {
    let f = Fixture::new(Options::default()).await;
    let first = f.create("first").await;
    let _second = f.create("second").await;
    let third = f.create("third").await;
    let _coordinator = f.create_child("coordinator", "", "coordinator").await;
    let other_coordinator = f.create_child("other-coordinator", "", "coordinator").await;
    let dependent = f
        .engine
        .create(
            &f.context,
            "dependent",
            CreateTask {
                project_id: f.project.id.clone(),
                dependencies: vec![first.id.clone()],
                title: "dependent".into(),
                instructions: "follow dependency".into(),
                ..Default::default()
            },
        )
        .await
        .unwrap();
    f.tick().await;
    assert_eq!(f.runtime.calls("prompt"), 3);
    assert!(f.state().tasks[&third.id].attempt_id.is_empty());
    assert!(f.state().tasks[&other_coordinator.id].attempt_id.is_empty());
    assert!(f.state().tasks[&dependent.id].attempt_id.is_empty());
    let attempt = f.attempt(&first);
    f.runtime.observe(&attempt.session_ref, "idle");
    f.engine
        .submit_result(&f.context, "result", &first.id, f.result(&first))
        .await
        .unwrap();
    f.tick().await;
    assert!(!f.state().tasks[&third.id].attempt_id.is_empty());
    assert!(
        f.state().tasks[&dependent.id].attempt_id.is_empty(),
        "reported result is not accepted dependency completion"
    );
    f.engine
        .accept(&f.context, "accept", &first.id)
        .await
        .unwrap();
    let third_attempt = f.attempt(&third);
    f.runtime.observe(&third_attempt.session_ref, "idle");
    f.tick().await;
    assert!(!f.state().tasks[&dependent.id].attempt_id.is_empty());
    f.engine.shutdown().await;
}

#[tokio::test]
async fn paused_ancestor_blocks_new_children_but_does_not_pause_existing_child() {
    let f = Fixture::new(Options::default()).await;
    let parent = f.create_child("parent", "", "coordinator").await;
    let child = f.create_child("child", &parent.id, "worker").await;
    f.tick().await;
    f.control(&parent, "pause", false).await.unwrap();
    f.tick().await;
    assert_eq!(f.state().tasks[&child.id].desired, "active");
    f.engine
        .followup(
            &f.context,
            "follow-child",
            &child.id,
            Followup {
                text: "continue existing work".into(),
            },
        )
        .await
        .unwrap();
    f.tick().await;
    assert_eq!(f.runtime.calls("prompt"), 3);
    let input = CreateTask {
        project_id: f.project.id.clone(),
        parent_id: parent.id.clone(),
        title: "new child".into(),
        instructions: "new work".into(),
        ..Default::default()
    };
    assert!(
        f.engine
            .create(&f.context, "blocked-child", input)
            .await
            .is_err()
    );
    f.engine.shutdown().await;
}

#[tokio::test]
async fn rejected_reply_returns_to_pending_without_losing_the_old_receipt() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.tick().await;
    let attempt = f.attempt(&task);
    f.runtime.decision(&attempt.session_ref, "question");
    f.tick().await;
    let decision = f.state().decisions.values().next().unwrap().clone();
    let answer = ReplyDecision {
        answer: Some(RawJson::parse("null".into()).unwrap()),
    };
    let receipt = f
        .engine
        .reply(&f.context, "answer", &decision.id, answer.clone())
        .await
        .unwrap();
    f.runtime.state.lock().unwrap().reply_error = Some(Error::invalid("invalid choice"));
    f.tick().await;
    assert_eq!(f.state().decisions[&decision.id].status, "pending");
    assert!(f.state().decisions[&decision.id].reply.is_none());
    assert_eq!(
        f.engine
            .reply(&f.context, "answer", &decision.id, answer.clone())
            .await
            .unwrap(),
        receipt
    );
    f.runtime.state.lock().unwrap().reply_error = None;
    f.engine
        .reply(&f.context, "corrected-answer", &decision.id, answer)
        .await
        .unwrap();
    f.tick().await;
    assert_eq!(f.state().decisions[&decision.id].status, "answered");
    assert_eq!(f.runtime.calls("reply"), 2);
    f.engine.shutdown().await;
}

#[tokio::test]
async fn cleanup_barrier_cannot_be_crossed_by_controls_or_new_results() {
    let f = Fixture::new(Options::default()).await;
    let task = f.create("task").await;
    f.tick().await;
    let attempt = f.attempt(&task);
    f.runtime.observe(&attempt.session_ref, "failed");
    f.tick().await;
    f.engine
        .cleanup(&f.context, "cleanup", &attempt.id)
        .await
        .unwrap();
    assert!(f.control(&task, "resume", false).await.is_err());
    let result = f
        .engine
        .submit_result(&f.context, "late", &task.id, f.result(&task))
        .await
        .unwrap();
    assert!(result.late);
    f.runtime.state.lock().unwrap().background_busy = true;
    f.tick().await;
    assert_eq!(f.attempt(&task).phase, "cleanup_pending");
    assert!(
        !f.workspaces
            .calls
            .lock()
            .unwrap()
            .iter()
            .any(|call| call == "remove")
    );
    f.runtime.state.lock().unwrap().background_busy = false;
    f.tick().await;
    assert_eq!(f.attempt(&task).phase, "cleaned");
    f.engine.shutdown().await;
}

#[tokio::test]
async fn allocation_orders_actual_instants_not_timestamp_strings() {
    let f = Fixture::new(Options {
        concurrency: 1,
        ..Default::default()
    })
    .await;
    let earlier = f.create("earlier").await;
    let later = f.create("later").await;
    f.store
        .update(&mut |state| {
            state.tasks.get_mut(&earlier.id).unwrap().created_at =
                Timestamp("2026-10-26T08:00:00+08:00".into());
            state.tasks.get_mut(&later.id).unwrap().created_at =
                Timestamp("2026-10-26T01:00:00Z".into());
            Ok(())
        })
        .unwrap();
    f.tick().await;
    assert!(!f.state().tasks[&earlier.id].attempt_id.is_empty());
    assert!(f.state().tasks[&later.id].attempt_id.is_empty());
    f.engine.shutdown().await;
}
