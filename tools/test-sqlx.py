#!/usr/bin/env python3
"""Behavioral checks for safe disposable SQLx preparation."""

import os
import sqlite3
import tempfile
import unittest
from contextlib import closing
from pathlib import Path

import sqlx


class SqlxPreparation(unittest.TestCase):
    def test_canonical_schema_is_queryable_without_application_data(self):
        with tempfile.TemporaryDirectory(dir=sqlx.ROOT / ".tmp") as directory:
            database = Path(directory) / "schema.db"
            sqlx.schema(database)
            with closing(sqlite3.connect(database)) as connection:
                self.assertEqual(
                    connection.execute(
                        "SELECT max(version) FROM schema_migrations"
                    ).fetchone(),
                    (1,),
                )
                for table in (
                    "projects",
                    "tasks",
                    "attempts",
                    "commands",
                    "decisions",
                    "results",
                    "operations",
                ):
                    self.assertEqual(
                        connection.execute(f"SELECT id,body FROM {table}").fetchall(),
                        [],
                    )
                self.assertEqual(
                    connection.execute(
                        "SELECT value FROM metadata WHERE key='revision'"
                    ).fetchone(),
                    ("0",),
                )
                with self.assertRaises(sqlite3.IntegrityError):
                    connection.execute("INSERT INTO tasks VALUES('invalid','not-json')")

    @unittest.skipUnless(hasattr(os, "mkfifo"), "FIFO metadata guard is a Unix check")
    def test_guard_refuses_dotenv_without_opening_even_a_fifo(self):
        with tempfile.TemporaryDirectory(dir=sqlx.ROOT / ".tmp") as directory:
            root = Path(directory)
            os.mkfifo(root / ".env")
            with self.assertRaisesRegex(RuntimeError, "no contents were read"):
                sqlx.guard(root / "nested")

    def test_guard_refuses_a_broken_dotenv_symlink(self):
        with tempfile.TemporaryDirectory(dir=sqlx.ROOT / ".tmp") as directory:
            root = Path(directory)
            (root / ".env").symlink_to(root / "absent")
            with self.assertRaises(RuntimeError):
                sqlx.guard(root)

    def test_compiler_environment_drops_inherited_database_and_cache(self):
        from unittest.mock import patch

        with patch.dict(
            os.environ, {"DATABASE_URL": "synthetic", "SQLX_OFFLINE_DIR": "synthetic"}
        ):
            env = sqlx.environment(offline=True)
        self.assertNotIn("DATABASE_URL", env)
        self.assertNotIn("SQLX_OFFLINE_DIR", env)
        self.assertEqual(env["SQLX_OFFLINE"], "true")


if __name__ == "__main__":
    unittest.main()
