#!/usr/bin/env python3
"""Exercise SQLx's actual compiler and schema-metadata checks in isolation."""

import hashlib
import json
import os
import sqlite3
import subprocess
import tempfile
import tomllib
from contextlib import closing
from pathlib import Path

import sqlx


def metadata_hashes(root):
    return {
        path.name: hashlib.sha256(path.read_bytes()).hexdigest()
        for path in (root / ".sqlx").glob("query-*.json")
    }


def main():
    sqlx.guard(sqlx.ROOT)
    # Exercise the actual installed CLI from an ordinary shell, where Cargo has
    # not injected CARGO. Every shared environment must supply it independently.
    os.environ.pop("CARGO", None)
    cargo = sqlx.environment(offline=True)["CARGO"]
    fixture = Path(tempfile.mkdtemp(prefix="sqlx-proof-", dir=sqlx.ROOT / ".tmp"))
    (fixture / "src").mkdir()
    manifest = tomllib.loads((sqlx.ROOT / "Cargo.toml").read_text())
    dependency = manifest["workspace"]["dependencies"]["sqlx"]
    profile = manifest["profile"]["release"]
    # Use the same pinned dependency/features as the maintained executable.
    (fixture / "Cargo.toml").write_text(
        '[workspace]\n[package]\nname = "sqlx-offline-proof"\n'
        'version = "0.0.0"\nedition = "2024"\npublish = false\n'
        "[dependencies]\nsqlx = " + json.dumps(dependency).replace(": ", " = ") + "\n"
        "[profile.release]\n"
        + "".join(
            key + " = " + json.dumps(value).replace(": ", " = ") + "\n"
            for key, value in profile.items()
        )
    )
    original = (
        "struct Stored { id: Option<String>, body: String }\n"
        'fn main() { let _ = sqlx::query_as!(Stored, "SELECT id,body FROM tasks"); }\n'
    )
    source = fixture / "src/main.rs"
    source.write_text(original)
    database = fixture / "schema.db"
    sqlx.schema(database)

    def run(name, command, *, offline=True, passes=True, database_url=None):
        env = sqlx.environment(offline=offline)
        env["CARGO_TARGET_DIR"] = str(fixture / "target")
        # If a normal build attempts online introspection, this deliberately
        # unsupported backend makes it fail rather than contact any database.
        env["DATABASE_URL"] = database_url or "unsupported-sqlx-proof://no-database"
        sqlx.guard(fixture)
        with (fixture / f"{name}.log").open("w") as log:
            result = subprocess.run(
                command,
                cwd=fixture,
                env=env,
                stdout=log,
                stderr=subprocess.STDOUT,
                timeout=120,
            )
        if (result.returncode == 0) != passes:
            raise RuntimeError(
                f"Unexpected SQLx proof result: {name}; see {fixture.name}/{name}.log"
            )

    check = [cargo, "check", "--locked", "--offline"]
    prepare = sqlx.prepare_command(database)
    run("lock", [cargo, "generate-lockfile", "--offline"])
    run("prepare", prepare + ["--", "--locked", "--offline"], offline=False)
    before = metadata_hashes(fixture)
    assert before, "SQLx did not produce real query metadata"
    run("conforming-offline", check)
    run(
        "conforming-metadata",
        prepare + ["--check", "--", "--locked", "--offline"],
        offline=False,
    )

    source.write_text(original.replace("body: String", "body: i64"))
    run("rejected-row-type", check, passes=False)
    diagnostic = (fixture / "rejected-row-type.log").read_text()
    assert "E0277" in diagnostic or "E0308" in diagnostic
    source.write_text(original.replace("SELECT id,body", "SELECT body,id"))
    run("rejected-uncached-query", check, passes=False)
    run(
        "rejected-stale-query-metadata",
        prepare + ["--check", "--", "--locked", "--offline"],
        offline=False,
        passes=False,
    )
    assert metadata_hashes(fixture) == before, (
        "--check modified committed-cache equivalents"
    )

    source.write_text(original)
    cache = fixture / ".sqlx"
    cache.rename(fixture / "saved-cache")
    try:
        run("rejected-missing-cache", check, passes=False)
    finally:
        (fixture / "saved-cache").rename(cache)
    with closing(sqlite3.connect(database)) as connection, connection:
        connection.execute("ALTER TABLE tasks RENAME COLUMN body TO body_changed")
    run(
        "rejected-schema-drift",
        prepare + ["--check", "--", "--locked", "--offline"],
        offline=False,
        passes=False,
    )
    assert metadata_hashes(fixture) == before
    with closing(sqlite3.connect(database)) as connection, connection:
        connection.execute("ALTER TABLE tasks RENAME COLUMN body_changed TO body")
    run("restored-offline", check)
    run(
        "restored-metadata",
        prepare + ["--check", "--", "--locked", "--offline"],
        offline=False,
    )
    # Compile and load the real SQLx proc macro with the maintained release
    # profile too. Dev-only cargo check misses macOS dylib stripping failures.
    run(
        "release-macro-consumer",
        [cargo, "build", "--release", "--locked", "--offline"],
    )
    run("release-executable", [str(fixture / "target/release/sqlx-offline-proof")])
    print(
        "SQLx offline compilation, release macro loading and metadata checks passed conforming consumers and rejected row-type errors, missing/uncached queries, stale query metadata and schema drift."
    )
    print("Evidence: " + str(fixture.relative_to(sqlx.ROOT)))


if __name__ == "__main__":
    main()
