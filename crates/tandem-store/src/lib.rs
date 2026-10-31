//! SQLite implementation of the runtime-independent Store boundary.
//!
//! The original migration remains the single schema owner. No scheduler, native
//! runtime discovery, retained-state migration, or service startup occurs here.

use std::{
    collections::BTreeMap,
    fs::{self, File, OpenOptions, TryLockError},
    path::{Path, PathBuf},
    sync::Mutex,
};

use pollster::block_on;
use serde::{Serialize, de::DeserializeOwned};
use sqlx::SqliteConnection;
use tandem_core::{Error, Result, Revision, State, Store};
use time::{OffsetDateTime, format_description::well_known::Rfc3339};

const INITIAL: &str = include_str!("../../../internal/store/migrations/001_initial.sql");

mod connection;
mod entities;

use connection::Database;

pub struct Sqlite {
    // Declaration order closes SQLite before releasing OS ownership on Drop.
    connection: Mutex<Database>,
    _lock: File,
    directory: PathBuf,
}

fn storage_error() -> Error {
    Error::new("storage_unavailable", "SQLite state operation failed")
}
fn io_error() -> Error {
    Error::new("storage_unavailable", "state filesystem operation failed")
}
fn corrupt() -> Error {
    Error::new(
        "corrupt_state",
        "stored state is invalid; explicit recovery is required",
    )
}

fn timestamp() -> Result<String> {
    OffsetDateTime::now_utc()
        .format(&Rfc3339)
        .map_err(|_| storage_error())
}

fn private_directory(path: &Path) -> Result<PathBuf> {
    let mut builder = fs::DirBuilder::new();
    builder.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        builder.mode(0o700);
    }
    builder.create(path).map_err(|_| io_error())?;
    let directory = fs::canonicalize(path).map_err(|_| io_error())?;
    let metadata = fs::metadata(&directory).map_err(|_| io_error())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if metadata.permissions().mode() & 0o077 != 0 {
            return Err(Error::invalid(
                "state directory must be owner-only; refusing to change existing permissions automatically",
            ));
        }
    }
    if !metadata.is_dir() {
        return Err(Error::invalid("state directory must be a directory"));
    }
    Ok(directory)
}

fn regular_or_absent(path: &Path) -> Result<()> {
    match fs::symlink_metadata(path) {
        Ok(meta) if meta.file_type().is_file() => Ok(()),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Ok(_) => Err(Error::invalid("refusing nonregular state file")),
        Err(_) => Err(io_error()),
    }
}

impl Sqlite {
    pub fn open(path: impl AsRef<Path>) -> Result<Self> {
        let directory = private_directory(path.as_ref())?;
        // Never follow a lock/database symlink to another ownership namespace.
        for name in [
            "scheduler.lock",
            "tandem.db",
            "tandem.db-wal",
            "tandem.db-shm",
        ] {
            regular_or_absent(&directory.join(name))?;
        }
        let mut options = OpenOptions::new();
        options.create(true).read(true).write(true).truncate(false);
        #[cfg(unix)]
        {
            use std::os::unix::fs::OpenOptionsExt;
            options.mode(0o600);
        }
        let lock = options
            .open(directory.join("scheduler.lock"))
            .map_err(|_| io_error())?;
        match lock.try_lock() {
            Ok(()) => (),
            Err(TryLockError::WouldBlock) => {
                return Err(Error::new(
                    "store_owned",
                    "state store already has an active scheduler owner; connect as a client instead",
                ));
            }
            Err(TryLockError::Error(_)) => return Err(io_error()),
        }
        let mut connection = Database::open(&directory.join("tandem.db"))?;
        connection.transaction(initialize)?;
        Ok(Self {
            connection: Mutex::new(connection),
            _lock: lock,
            directory,
        })
    }

    pub fn directory(&self) -> &Path {
        &self.directory
    }
}

fn initialize(connection: &mut SqliteConnection) -> Result<()> {
    block_on(sqlx::query!("CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)").execute(&mut *connection)).map_err(|_| storage_error())?;
    let version = block_on(
        sqlx::query_scalar!(
            "SELECT coalesce(max(version),0) AS \"version!\" FROM schema_migrations"
        )
        .fetch_one(&mut *connection),
    )
    .map_err(|_| storage_error())?;
    if version > 1 {
        return Err(Error::new(
            "newer_schema",
            "database schema is newer than this executable",
        ));
    }
    if version < 0 {
        return Err(corrupt());
    }
    if version == 0 {
        // The canonical multi-statement migration is the sole raw-SQL boundary.
        // It is executed on disposable schemas during metadata generation and
        // compatibility tests, not copied into a competing SQLx migration line.
        block_on(sqlx::raw_sql(INITIAL).execute(&mut *connection)).map_err(|_| storage_error())?;
        let mut random = [0_u8; 16];
        getrandom::fill(&mut random)
            .map_err(|_| Error::new("entropy_unavailable", "cannot allocate instance identity"))?;
        let instance: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
        block_on(
            sqlx::query!(
                "INSERT INTO metadata(key,value) VALUES('instance_id',?)",
                instance
            )
            .execute(&mut *connection),
        )
        .map_err(|_| storage_error())?;
        let at = timestamp()?;
        block_on(
            sqlx::query!("INSERT INTO schema_migrations VALUES(1,?)", at).execute(&mut *connection),
        )
        .map_err(|_| storage_error())?;
    }
    Ok(())
}

fn revision(connection: &mut SqliteConnection) -> Result<Revision> {
    let row = block_on(sqlx::query!(
        "SELECT (SELECT value FROM metadata WHERE key='instance_id') AS instance_id, (SELECT value FROM metadata WHERE key='revision') AS revision"
    ).fetch_one(connection)).map_err(|_| corrupt())?;
    let instance_id = row.instance_id.ok_or_else(corrupt)?;
    let revision = row.revision.ok_or_else(corrupt)?;
    let revision: i64 = revision.parse().map_err(|_| corrupt())?;
    if instance_id.is_empty() || revision < 0 {
        return Err(corrupt());
    }
    Ok(Revision {
        instance_id,
        revision,
    })
}

fn snapshot(connection: &mut SqliteConnection) -> Result<State> {
    let revision = revision(connection)?;
    Ok(State {
        instance_id: revision.instance_id,
        revision: revision.revision,
        projects: entities::projects(connection)?,
        tasks: entities::tasks(connection)?,
        attempts: entities::attempts(connection)?,
        commands: entities::commands(connection)?,
        decisions: entities::decisions(connection)?,
        results: entities::results(connection)?,
        operations: entities::operations(connection)?,
    })
}

fn write_map<T: Serialize + DeserializeOwned + PartialEq>(
    connection: &mut SqliteConnection,
    before: &BTreeMap<String, T>,
    after: &BTreeMap<String, T>,
    revision: i64,
    table: &str,
    mut write: impl FnMut(&mut SqliteConnection, &str, &str) -> Result<()>,
) -> Result<()> {
    if before.keys().any(|key| !after.contains_key(key)) {
        return Err(Error::invalid(
            "retained records cannot be deleted through a state transaction",
        ));
    }
    for (id, value) in after {
        if before.get(id) == Some(value) {
            continue;
        }
        let body = serde_json::to_string(value).map_err(|_| corrupt())?;
        // A caller can construct an extension field that shadows a typed field.
        // Refuse any representation that cannot round-trip before committing a
        // record the next owner would be unable to load without losing data.
        let decoded: T = serde_json::from_str(&body)
            .map_err(|_| Error::invalid("record has conflicting or invalid encoded fields"))?;
        if &decoded != value {
            return Err(Error::invalid(
                "record encoding would discard retained data",
            ));
        }
        write(connection, id, &body)?;
        let at = timestamp()?;
        block_on(
            sqlx::query!(
                "INSERT INTO history(revision,entity,id,at,body) VALUES(?,?,?,?,?)",
                revision,
                table,
                id,
                at,
                body
            )
            .execute(&mut *connection),
        )
        .map_err(|_| storage_error())?;
    }
    Ok(())
}

impl Store for Sqlite {
    fn snapshot(&self) -> Result<State> {
        let mut connection = self.connection.lock().map_err(|_| storage_error())?;
        connection.transaction(snapshot)
    }

    fn read_revision(&self) -> Result<Revision> {
        let mut connection = self.connection.lock().map_err(|_| storage_error())?;
        connection.transaction(revision)
    }

    fn update(&self, change: &mut dyn FnMut(&mut State) -> Result<()>) -> Result<()> {
        let mut connection = self.connection.lock().map_err(|_| storage_error())?;
        connection.transaction(|connection| update(connection, change))
    }
}

fn update(
    connection: &mut SqliteConnection,
    change: &mut dyn FnMut(&mut State) -> Result<()>,
) -> Result<()> {
    let before = snapshot(connection)?;
    let mut after = before.clone();
    change(&mut after)?;
    if before == after {
        return Ok(());
    }
    if before.instance_id != after.instance_id || before.revision != after.revision {
        return Err(Error::invalid(
            "instance identity and revision are store-owned",
        ));
    }
    // Exact accepted operation receipts are immutable, including their raw
    // outcome spelling. No rewrite/replay can silently create a new receipt.
    if before
        .operations
        .iter()
        .any(|(key, value)| after.operations.get(key) != Some(value))
    {
        return Err(Error::invalid("accepted operation receipts are immutable"));
    }
    let revision = before.revision.checked_add(1).ok_or_else(corrupt)?;
    entities::write_projects(connection, &before.projects, &after.projects, revision)?;
    entities::write_tasks(connection, &before.tasks, &after.tasks, revision)?;
    entities::write_attempts(connection, &before.attempts, &after.attempts, revision)?;
    entities::write_commands(connection, &before.commands, &after.commands, revision)?;
    entities::write_decisions(connection, &before.decisions, &after.decisions, revision)?;
    entities::write_results(connection, &before.results, &after.results, revision)?;
    entities::write_operations(connection, &before.operations, &after.operations, revision)?;
    let value = revision.to_string();
    block_on(
        sqlx::query!("UPDATE metadata SET value=? WHERE key='revision'", value).execute(connection),
    )
    .map_err(|_| storage_error())?;
    Ok(())
}
