use std::time::Duration;

use pollster::block_on;
use sqlx::{
    Connection, SqliteConnection, Transaction,
    sqlite::{SqliteConnectOptions, SqliteJournalMode, SqliteSynchronous},
};
use tandem_core::Result;

use crate::storage_error;

/// SQLx's single-connection SQLite driver is runtime-agnostic. The synchronous
/// Store port stays serialized, including when invoked from a Tokio caller.
pub(crate) struct Database {
    connection: Option<SqliteConnection>,
    healthy: bool,
}

impl Database {
    pub(crate) fn open(path: &std::path::Path) -> Result<Self> {
        let options = SqliteConnectOptions::new()
            .filename(path)
            .create_if_missing(true)
            .journal_mode(SqliteJournalMode::Wal)
            .synchronous(SqliteSynchronous::Full)
            .foreign_keys(true)
            .busy_timeout(Duration::from_secs(5));
        // The immutable driver patch awaits startup-error worker shutdown before
        // returning Err, while Sqlite::open still owns the scheduler lock.
        let connection =
            block_on(SqliteConnection::connect_with(&options)).map_err(|_| storage_error())?;
        Ok(Self {
            connection: Some(connection),
            healthy: true,
        })
    }

    pub(crate) fn transaction<T>(
        &mut self,
        work: impl FnOnce(&mut SqliteConnection) -> Result<T>,
    ) -> Result<T> {
        if !self.healthy {
            return Err(storage_error());
        }
        let connection = self.connection.as_mut().ok_or_else(storage_error)?;
        let transaction = block_on(connection.begin()).map_err(|_| storage_error())?;
        let mut guard = SettledTransaction(Some(transaction));
        self.healthy = false;
        let result = work(guard.0.as_mut().ok_or_else(storage_error)?);
        let transaction = guard.0.take().ok_or_else(storage_error)?;
        let settlement = if result.is_ok() {
            block_on(transaction.commit())
        } else {
            block_on(transaction.rollback())
        };
        drop(guard);
        // A failed commit/rollback may enqueue a rollback on Transaction::drop.
        // A FIFO worker acknowledgment settles that work before the mutex can
        // release. Fail closed after any uncertain settlement.
        let drained = block_on(connection.ping());
        settlement.map_err(|_| storage_error())?;
        drained.map_err(|_| storage_error())?;
        self.healthy = true;
        result
    }
}

impl Drop for Database {
    fn drop(&mut self) {
        if let Some(connection) = self.connection.take() {
            // close() joins SQLx's SQLite worker, unlike dropping the handle.
            // The containing Sqlite retains its lifetime OS lock until this
            // method returns, including initialization failure and poisoning.
            let _closed = block_on(connection.close());
        }
    }
}

struct SettledTransaction<'a>(Option<Transaction<'a, sqlx::Sqlite>>);

impl Drop for SettledTransaction<'_> {
    fn drop(&mut self) {
        if let Some(transaction) = self.0.take() {
            // Also settle callback unwinding before the Mutex becomes poisoned.
            let _rolled_back = block_on(transaction.rollback());
        }
    }
}
