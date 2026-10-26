use std::fs;

use rusqlite::Connection;
use tandem_core::{Error, Operation, Project, RawJson, Store, Task};
use tandem_store::Sqlite;
use tempfile::TempDir;

fn open() -> (TempDir, Sqlite) {
    let root = TempDir::new().unwrap();
    let store = Sqlite::open(root.path().join("state")).unwrap();
    (root, store)
}

#[test]
fn commit_rollback_and_reopen_preserve_identity_and_receipt() {
    let (root, store) = open();
    let before = store.read_revision().unwrap();
    let outcome = r#"{"title":"old","large":9007199254740993,"answer":null}"#;
    store
        .update(&mut |state| {
            state.tasks.insert(
                "tsk_one".into(),
                Task {
                    id: "tsk_one".into(),
                    title: "old".into(),
                    ..Default::default()
                },
            );
            state.operations.insert(
                "create-1".into(),
                Operation {
                    id: "create-1".into(),
                    fingerprint: "original-fingerprint".into(),
                    outcome: RawJson::parse(outcome.into()).unwrap(),
                    ..Default::default()
                },
            );
            Ok(())
        })
        .unwrap();
    assert!(
        store
            .update(&mut |state| {
                state.tasks.get_mut("tsk_one").unwrap().title = "rolled back".into();
                Err(Error::invalid("injected rollback"))
            })
            .is_err()
    );
    drop(store);
    let store = Sqlite::open(root.path().join("state")).unwrap();
    let state = store.snapshot().unwrap();
    assert_eq!(state.instance_id, before.instance_id);
    assert_eq!(state.revision, before.revision + 1);
    assert_eq!(state.tasks["tsk_one"].title, "old");
    assert_eq!(state.operations["create-1"].outcome.as_str(), outcome);
}

#[test]
fn no_op_is_quiet_and_only_changed_rows_append_history() {
    let (_root, store) = open();
    store
        .update(&mut |state| {
            state.projects.insert(
                "prj_one".into(),
                Project {
                    id: "prj_one".into(),
                    path: "/fixture/one".into(),
                    ..Default::default()
                },
            );
            state.tasks.insert(
                "tsk_one".into(),
                Task {
                    id: "tsk_one".into(),
                    title: "before".into(),
                    ..Default::default()
                },
            );
            Ok(())
        })
        .unwrap();
    let before = store.read_revision().unwrap();
    store.update(&mut |_| Ok(())).unwrap();
    assert_eq!(store.read_revision().unwrap(), before);
    store
        .update(&mut |state| {
            state.tasks.get_mut("tsk_one").unwrap().title = "after".into();
            Ok(())
        })
        .unwrap();
    let connection = Connection::open(store.directory().join("tandem.db")).unwrap();
    let count: i64 = connection
        .query_row("SELECT count(*) FROM history", [], |row| row.get(0))
        .unwrap();
    assert_eq!(count, 3);
}

#[test]
fn all_failed_mutations_leave_durable_state_unchanged() {
    let (_root, store) = open();
    store
        .update(&mut |state| {
            state.tasks.insert("tsk_one".into(), Task::default());
            state
                .operations
                .insert("op_one".into(), Operation::default());
            Ok(())
        })
        .unwrap();
    let before = store.snapshot().unwrap();
    for mutation in ["identity", "revision", "delete", "receipt", "constraint"] {
        let result = store.update(&mut |state| {
            match mutation {
                "identity" => state.instance_id = "different".into(),
                "revision" => state.revision += 1,
                "delete" => {
                    state.tasks.clear();
                }
                "receipt" => {
                    state.operations.get_mut("op_one").unwrap().fingerprint = "changed".into()
                }
                "constraint" => {
                    for key in ["a", "b"] {
                        state.projects.insert(
                            key.into(),
                            Project {
                                id: key.into(),
                                path: "/same".into(),
                                ..Default::default()
                            },
                        );
                    }
                }
                _ => unreachable!(),
            }
            Ok(())
        });
        assert!(result.is_err(), "{mutation} was accepted");
        assert_eq!(
            store.snapshot().unwrap(),
            before,
            "{mutation} partially committed"
        );
    }
}

#[test]
fn same_process_and_canonical_alias_cannot_bypass_ownership() {
    let (root, store) = open();
    assert_eq!(
        Sqlite::open(store.directory()).err().unwrap().code,
        "store_owned"
    );
    #[cfg(unix)]
    {
        let alias = root.path().join("alias");
        std::os::unix::fs::symlink(store.directory(), &alias).unwrap();
        assert_eq!(Sqlite::open(alias).err().unwrap().code, "store_owned");
    }
    let directory = store.directory().to_owned();
    drop(store);
    assert!(Sqlite::open(directory).is_ok());
}

#[test]
fn permissive_or_nonregular_state_is_refused_without_repair() {
    let root = TempDir::new().unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(root.path(), fs::Permissions::from_mode(0o755)).unwrap();
        assert!(Sqlite::open(root.path()).is_err());
        assert_eq!(
            fs::metadata(root.path()).unwrap().permissions().mode() & 0o777,
            0o755
        );
        assert!(!root.path().join("tandem.db").exists());
        fs::set_permissions(root.path(), fs::Permissions::from_mode(0o700)).unwrap();
        for name in [
            "scheduler.lock",
            "tandem.db",
            "tandem.db-wal",
            "tandem.db-shm",
        ] {
            let path = root.path().join(name);
            std::os::unix::fs::symlink(root.path().join("outside"), &path).unwrap();
            assert!(Sqlite::open(root.path()).is_err(), "followed {name}");
            fs::remove_file(path).unwrap();
        }
    }
}

#[test]
fn newer_schema_and_corrupt_records_fail_closed() {
    let (_root, store) = open();
    let directory = store.directory().to_owned();
    let connection = Connection::open(directory.join("tandem.db")).unwrap();
    connection
        .execute("INSERT INTO tasks VALUES('broken','[]')", [])
        .unwrap();
    assert_eq!(store.snapshot().unwrap_err().code, "corrupt_state");
    connection.execute("DELETE FROM tasks", []).unwrap();
    connection
        .execute("INSERT INTO schema_migrations VALUES(2,'future')", [])
        .unwrap();
    drop(connection);
    drop(store);
    assert_eq!(Sqlite::open(directory).err().unwrap().code, "newer_schema");
}

#[test]
fn concurrent_changes_are_serialized_and_large_content_is_complete() {
    let (_root, store) = open();
    let payload = "世界🙂<&>\u{2028}".repeat(100_000);
    store
        .update(&mut |state| {
            state.tasks.insert(
                "tsk_big".into(),
                Task {
                    instructions: payload.clone(),
                    ..Default::default()
                },
            );
            Ok(())
        })
        .unwrap();
    std::thread::scope(|scope| {
        for _ in 0..8 {
            scope.spawn(|| {
                store
                    .update(&mut |state| {
                        state.tasks.get_mut("tsk_big").unwrap().generation += 1;
                        Ok(())
                    })
                    .unwrap();
            });
        }
    });
    let state = store.snapshot().unwrap();
    assert_eq!(state.revision, 9);
    assert_eq!(state.tasks["tsk_big"].generation, 8);
    assert_eq!(
        state.tasks["tsk_big"].instructions.as_bytes(),
        payload.as_bytes()
    );
}

#[test]
fn invalid_record_encoding_cannot_be_committed() {
    let (_root, store) = open();
    let before = store.snapshot().unwrap();
    let outcome = store.update(&mut |state| {
        let mut task = Task {
            title: "original".into(),
            ..Default::default()
        };
        task.extra
            .insert("title".into(), serde_json::json!("shadow"));
        state.tasks.insert("tsk_collision".into(), task);
        Ok(())
    });
    assert!(outcome.is_err(), "serialized field collision was committed");
    assert_eq!(store.snapshot().unwrap(), before);
}

#[test]
fn unrelated_updates_retain_unknown_fields() {
    let (_root, store) = open();
    let extension = serde_json::json!({"nested":[true,null,"完整🙂"]});
    store
        .update(&mut |state| {
            let mut task = Task {
                title: "before".into(),
                ..Default::default()
            };
            task.extra.insert("extension".into(), extension.clone());
            state.tasks.insert("tsk_extension".into(), task);
            Ok(())
        })
        .unwrap();
    store
        .update(&mut |state| {
            state.tasks.get_mut("tsk_extension").unwrap().title = "after".into();
            Ok(())
        })
        .unwrap();
    assert_eq!(
        store.snapshot().unwrap().tasks["tsk_extension"].extra["extension"],
        extension
    );
}
