//! SQLite implementation of the runtime-independent Store boundary.
//!
//! The original migration remains the single schema owner. No scheduler, native
//! runtime discovery, retained-state migration, or service startup occurs here.

use std::{
    collections::BTreeMap,
    fs::{self, File, OpenOptions, TryLockError},
    path::{Path, PathBuf},
    sync::Mutex,
    time::Duration,
};

use rusqlite::{Connection, Transaction, params};
use serde::{Serialize, de::DeserializeOwned};
use tandem_core::{Error, Result, Revision, State, Store};
use time::{OffsetDateTime, format_description::well_known::Rfc3339};

const INITIAL: &str = include_str!("../../../internal/store/migrations/001_initial.sql");

pub struct Sqlite {
    // Declaration order closes SQLite before releasing OS ownership on Drop.
    connection: Mutex<Connection>,
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
        let mut connection =
            Connection::open(directory.join("tandem.db")).map_err(|_| storage_error())?;
        connection
            .busy_timeout(Duration::from_secs(5))
            .map_err(|_| storage_error())?;
        connection
            .execute_batch(
                "PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;",
            )
            .map_err(|_| storage_error())?;
        initialize(&mut connection)?;
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

fn initialize(connection: &mut Connection) -> Result<()> {
    let transaction = connection.transaction().map_err(|_| storage_error())?;
    transaction.execute_batch("CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)").map_err(|_| storage_error())?;
    let version: i64 = transaction
        .query_row(
            "SELECT coalesce(max(version),0) FROM schema_migrations",
            [],
            |row| row.get(0),
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
        transaction
            .execute_batch(INITIAL)
            .map_err(|_| storage_error())?;
        let mut random = [0_u8; 16];
        getrandom::fill(&mut random)
            .map_err(|_| Error::new("entropy_unavailable", "cannot allocate instance identity"))?;
        let instance: String = random.iter().map(|byte| format!("{byte:02x}")).collect();
        transaction
            .execute(
                "INSERT INTO metadata(key,value) VALUES('instance_id',?)",
                [instance],
            )
            .map_err(|_| storage_error())?;
        transaction
            .execute("INSERT INTO schema_migrations VALUES(1,?)", [timestamp()?])
            .map_err(|_| storage_error())?;
    }
    transaction.commit().map_err(|_| storage_error())
}

fn load_map<T: DeserializeOwned>(
    connection: &Connection,
    table: &str,
) -> Result<BTreeMap<String, T>> {
    // table is a fixed implementation-owned literal, never caller input.
    let mut statement = connection
        .prepare(&format!("SELECT id,body FROM {table}"))
        .map_err(|_| storage_error())?;
    let rows = statement
        .query_map([], |row| {
            Ok((row.get::<_, String>(0)?, row.get::<_, String>(1)?))
        })
        .map_err(|_| storage_error())?;
    let mut values = BTreeMap::new();
    for row in rows {
        let (id, body) = row.map_err(|_| storage_error())?;
        if !body.trim_start().starts_with('{') {
            return Err(corrupt());
        }
        values.insert(id, serde_json::from_str(&body).map_err(|_| corrupt())?);
    }
    Ok(values)
}

fn revision(connection: &Connection) -> Result<Revision> {
    let (instance_id, revision): (String, String) = connection.query_row(
        "SELECT (SELECT value FROM metadata WHERE key='instance_id'), (SELECT value FROM metadata WHERE key='revision')", [],
        |row| Ok((row.get(0)?, row.get(1)?)),
    ).map_err(|_| corrupt())?;
    let revision: i64 = revision.parse().map_err(|_| corrupt())?;
    if instance_id.is_empty() || revision < 0 {
        return Err(corrupt());
    }
    Ok(Revision {
        instance_id,
        revision,
    })
}

fn snapshot(connection: &Connection) -> Result<State> {
    let revision = revision(connection)?;
    Ok(State {
        instance_id: revision.instance_id,
        revision: revision.revision,
        projects: load_map(connection, "projects")?,
        tasks: load_map(connection, "tasks")?,
        attempts: load_map(connection, "attempts")?,
        commands: load_map(connection, "commands")?,
        decisions: load_map(connection, "decisions")?,
        results: load_map(connection, "results")?,
        operations: load_map(connection, "operations")?,
    })
}

fn write_map<T: Serialize + DeserializeOwned + PartialEq>(
    transaction: &Transaction<'_>,
    table: &str,
    before: &BTreeMap<String, T>,
    after: &BTreeMap<String, T>,
    revision: i64,
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
        transaction.execute(&format!("INSERT INTO {table}(id,body) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body"), params![id, body]).map_err(|_| storage_error())?;
        transaction
            .execute(
                "INSERT INTO history(revision,entity,id,at,body) VALUES(?,?,?,?,?)",
                params![revision, table, id, timestamp()?, body],
            )
            .map_err(|_| storage_error())?;
    }
    Ok(())
}

impl Store for Sqlite {
    fn snapshot(&self) -> Result<State> {
        let mut connection = self.connection.lock().map_err(|_| storage_error())?;
        let transaction = connection.transaction().map_err(|_| storage_error())?;
        let state = snapshot(&transaction)?;
        transaction.commit().map_err(|_| storage_error())?;
        Ok(state)
    }

    fn read_revision(&self) -> Result<Revision> {
        let connection = self.connection.lock().map_err(|_| storage_error())?;
        revision(&connection)
    }

    fn update(&self, change: &mut dyn FnMut(&mut State) -> Result<()>) -> Result<()> {
        let mut connection = self.connection.lock().map_err(|_| storage_error())?;
        let transaction = connection.transaction().map_err(|_| storage_error())?;
        let before = snapshot(&transaction)?;
        let mut after = before.clone();
        change(&mut after)?;
        if before == after {
            return transaction.commit().map_err(|_| storage_error());
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
        write_map(
            &transaction,
            "projects",
            &before.projects,
            &after.projects,
            revision,
        )?;
        write_map(&transaction, "tasks", &before.tasks, &after.tasks, revision)?;
        write_map(
            &transaction,
            "attempts",
            &before.attempts,
            &after.attempts,
            revision,
        )?;
        write_map(
            &transaction,
            "commands",
            &before.commands,
            &after.commands,
            revision,
        )?;
        write_map(
            &transaction,
            "decisions",
            &before.decisions,
            &after.decisions,
            revision,
        )?;
        write_map(
            &transaction,
            "results",
            &before.results,
            &after.results,
            revision,
        )?;
        write_map(
            &transaction,
            "operations",
            &before.operations,
            &after.operations,
            revision,
        )?;
        transaction
            .execute(
                "UPDATE metadata SET value=? WHERE key='revision'",
                [revision.to_string()],
            )
            .map_err(|_| storage_error())?;
        transaction.commit().map_err(|_| storage_error())
    }
}
