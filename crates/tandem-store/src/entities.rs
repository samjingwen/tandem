use std::collections::BTreeMap;

use futures_util::TryStreamExt;
use pollster::block_on;
use serde::de::DeserializeOwned;
use sqlx::SqliteConnection;
use tandem_core::{Attempt, Command, Decision, Operation, Project, Result, Task, TaskResult};

use crate::{corrupt, storage_error, write_map};

struct StoredRow {
    // SQLite's historical TEXT PRIMARY KEY permits NULL. Refuse it as the
    // original reader did, without changing the retained schema or overriding
    // SQLx's correct nullable-column inference.
    id: Option<String>,
    body: String,
}

fn decode<T: DeserializeOwned>(row: StoredRow) -> Result<(String, T)> {
    let id = row.id.ok_or_else(storage_error)?;
    if !row.body.trim_start().starts_with('{') {
        return Err(corrupt());
    }
    Ok((id, serde_json::from_str(&row.body).map_err(|_| corrupt())?))
}

// Every invocation supplies SQL literals. SQLx expands and checks each table's
// real SELECT and UPSERT independently, rather than validating a sample query
// while executing unchecked formatted identifiers at runtime.
macro_rules! entity {
    ($load:ident, $write:ident, $name:literal, $ty:ty, $select:literal, $upsert:literal) => {
        pub(crate) fn $load(connection: &mut SqliteConnection) -> Result<BTreeMap<String, $ty>> {
            let mut rows = sqlx::query_as!(StoredRow, $select).fetch(connection);
            block_on(async {
                let mut values = BTreeMap::new();
                while let Some(row) = rows.try_next().await.map_err(|_| storage_error())? {
                    let (id, value) = decode(row)?;
                    values.insert(id, value);
                }
                Ok(values)
            })
        }

        pub(crate) fn $write(
            connection: &mut SqliteConnection,
            before: &BTreeMap<String, $ty>,
            after: &BTreeMap<String, $ty>,
            revision: i64,
        ) -> Result<()> {
            write_map(
                connection,
                before,
                after,
                revision,
                $name,
                |connection, id, body| {
                    block_on(sqlx::query!($upsert, id, body).execute(connection))
                        .map(|_| ())
                        .map_err(|_| storage_error())
                },
            )
        }
    };
}

entity!(
    projects,
    write_projects,
    "projects",
    Project,
    "SELECT id,body FROM projects",
    "INSERT INTO projects(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
entity!(
    tasks,
    write_tasks,
    "tasks",
    Task,
    "SELECT id,body FROM tasks",
    "INSERT INTO tasks(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
entity!(
    attempts,
    write_attempts,
    "attempts",
    Attempt,
    "SELECT id,body FROM attempts",
    "INSERT INTO attempts(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
entity!(
    commands,
    write_commands,
    "commands",
    Command,
    "SELECT id,body FROM commands",
    "INSERT INTO commands(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
entity!(
    decisions,
    write_decisions,
    "decisions",
    Decision,
    "SELECT id,body FROM decisions",
    "INSERT INTO decisions(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
entity!(
    results,
    write_results,
    "results",
    TaskResult,
    "SELECT id,body FROM results",
    "INSERT INTO results(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
entity!(
    operations,
    write_operations,
    "operations",
    Operation,
    "SELECT id,body FROM operations",
    "INSERT INTO operations(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"
);
