#!/usr/bin/env python3
"""Behavioral checks for safe disposable SQLx preparation."""

import json
import os
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from contextlib import closing
from pathlib import Path
from unittest.mock import patch

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
        with patch.dict(
            os.environ, {"DATABASE_URL": "synthetic", "SQLX_OFFLINE_DIR": "synthetic"}
        ):
            env = sqlx.environment(offline=True)
        self.assertNotIn("DATABASE_URL", env)
        self.assertNotIn("SQLX_OFFLINE_DIR", env)
        self.assertEqual(env["SQLX_OFFLINE"], "true")

    def test_local_cli_selection_is_closed_to_global_discovery(self):
        with tempfile.TemporaryDirectory(dir=sqlx.ROOT / ".tmp") as directory:
            root = Path(directory)
            database = root / "schema.db"
            sqlx.schema(database)
            local = root / ".tmp/sqlx-tools/bin/cargo-sqlx"
            local.parent.mkdir(parents=True)
            global_bin = root / "global/bin"
            global_bin.mkdir(parents=True)
            marker = root / "global-invoked"
            for name in ("cargo", "cargo-sqlx"):
                executable = global_bin / name
                executable.write_text(
                    f"#!{sys.executable}\nfrom pathlib import Path\n"
                    f"Path({str(marker)!r}).touch()\nraise SystemExit(99)\n"
                )
                executable.chmod(0o700)
            receipt = root / "local-receipt.json"
            script = (
                f"#!{sys.executable}\nimport json, os, sys\n"
                "from pathlib import Path\n"
                "if sys.argv[1:] == ['sqlx', '--no-dotenv', '--version']:\n"
                "    print('sqlx-cli VERSION')\n"
                "else:\n"
                "    if not os.environ.get('CARGO'):\n"
                "        raise SystemExit('CARGO was not supplied')\n"
                f"    Path({str(receipt)!r}).write_text(json.dumps({{\n"
                "        'args': sys.argv[1:],\n"
                "        'database': os.environ.get('DATABASE_URL'),\n"
                "        'cache': os.environ.get('SQLX_OFFLINE_DIR'),\n"
                "        'offline': os.environ.get('SQLX_OFFLINE'),\n"
                "        'cargo': os.environ['CARGO'],\n"
                "    }))\n"
            )
            with (
                patch.object(sqlx, "ROOT", root),
                patch.dict(
                    os.environ,
                    {
                        "PATH": str(global_bin),
                        "CARGO_HOME": str(global_bin.parent),
                        "DATABASE_URL": "synthetic-retained",
                        "SQLX_OFFLINE_DIR": "synthetic-cache",
                    },
                ),
            ):
                os.environ.pop("CARGO", None)
                for check in (False, True):
                    with self.subTest(check=check, cli="missing"):
                        with self.assertRaises(FileNotFoundError):
                            sqlx.prepare(root, database, check=check)
                self.assertFalse(marker.exists())
                for version in ("0.8.6", "0.9.0"):
                    local.write_text(script.replace("VERSION", version))
                    local.chmod(0o700)
                    for check in (False, True):
                        with self.subTest(check=check, version=version):
                            if version != "0.9.0":
                                with self.assertRaisesRegex(RuntimeError, "0.9.0"):
                                    sqlx.prepare(root, database, check=check)
                                self.assertFalse(receipt.exists())
                                continue
                            sqlx.prepare(root, database, check=check)
                            observed = json.loads(receipt.read_text())
                            url = "sqlite://" + str(database.resolve())
                            self.assertEqual(
                                observed,
                                {
                                    "args": [
                                        "sqlx",
                                        "--no-dotenv",
                                        "prepare",
                                        "--workspace",
                                        "--database-url",
                                        url,
                                    ]
                                    + (["--check"] if check else [])
                                    + ["--", "--locked", "--all-targets"],
                                    "database": url,
                                    "cache": None,
                                    "offline": "false",
                                    "cargo": str(global_bin / "cargo"),
                                },
                            )
                subprocess.run(
                    sqlx.prepare_command(database) + ["--", "--locked", "--offline"],
                    cwd=root,
                    env=sqlx.environment(offline=False),
                    check=True,
                )
                self.assertEqual(
                    json.loads(receipt.read_text())["args"][-3:],
                    ["--", "--locked", "--offline"],
                )
                self.assertFalse(marker.exists())

    def test_explicit_cargo_override_is_used_and_missing_cargo_is_refused(self):
        with tempfile.TemporaryDirectory(dir=sqlx.ROOT / ".tmp") as directory:
            root = Path(directory)
            cargo = root / "selected-cargo"
            cargo.write_text(f"#!{sys.executable}\nraise SystemExit(0)\n")
            cargo.chmod(0o700)
            with patch.dict(os.environ, {"CARGO": str(cargo), "PATH": str(root)}):
                self.assertEqual(sqlx.environment(offline=True)["CARGO"], str(cargo))
                cargo.unlink()
                with self.assertRaisesRegex(RuntimeError, "Cargo executable"):
                    sqlx.environment(offline=False)


if __name__ == "__main__":
    unittest.main()
