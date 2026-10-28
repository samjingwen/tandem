#!/usr/bin/env python3
"""Own SQLx metadata through disposable canonical SQLite schemas only."""

import argparse
import os
import sqlite3
import subprocess
import tempfile
from contextlib import closing
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BOOTSTRAP = "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)"


def guard(root):
    # SQLx 0.9's macros read ancestor .env files even in offline mode.
    # Refuse them by metadata alone, before invoking any macro or CLI.
    starts = [root, root / "crates" / "tandem-store"]
    paths = {
        directory / ".env" for start in starts for directory in (start, *start.parents)
    }
    if any(os.path.lexists(path) for path in paths):
        raise RuntimeError(
            "SQLx dotenv discovery is unsafe: an ancestor .env exists; no contents were read"
        )


def schema(path):
    with closing(sqlite3.connect(path)) as connection, connection:
        connection.execute(BOOTSTRAP)
        connection.executescript(
            (ROOT / "internal/store/migrations/001_initial.sql").read_text()
        )
        connection.execute("INSERT INTO schema_migrations VALUES(1,'schema-fixture')")


def environment(offline):
    env = dict(os.environ)
    # Never consume inherited database configuration or an external query cache.
    env.pop("DATABASE_URL", None)
    env.pop("SQLX_OFFLINE_DIR", None)
    env["SQLX_OFFLINE"] = "true" if offline else "false"
    env["PATH"] = str(ROOT / ".tmp/sqlx-tools/bin") + os.pathsep + env.get("PATH", "")
    return env


def prepare(root, database, check=False):
    guard(root)
    env = environment(offline=False)
    env["DATABASE_URL"] = "sqlite://" + str(database.resolve())
    command = [
        "cargo",
        "sqlx",
        "--no-dotenv",
        "prepare",
        "--workspace",
        "--database-url",
        env["DATABASE_URL"],
    ]
    if check:
        command.append("--check")
    command += ["--", "--locked", "--all-targets"]
    subprocess.run(command, cwd=root, env=env, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=["guard", "prepare", "check"])
    args = parser.parse_args()
    guard(ROOT)
    if args.mode == "guard":
        return
    temporary = ROOT / ".tmp"
    temporary.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="sqlx-schema-", dir=temporary) as directory:
        database = Path(directory) / "schema.db"
        schema(database)
        prepare(ROOT, database, check=args.mode == "check")


if __name__ == "__main__":
    main()
