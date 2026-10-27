"""Drive the real Rust Service/SQLite consumer with simulated execution only.

Oracles are the ownership, uncertainty, cancellation and limit contracts in
docs/RUST_MIGRATION.md, not fixture-reported pass flags or copied engine logic.
Requests, responses, execution-boundary records and independently read SQLite
rows are retained when --evidence is supplied. No native acceptance is claimed.
"""

import argparse
import hashlib
import json
import select
import sqlite3
import subprocess
import tempfile
import time
from pathlib import Path


class Consumer:
    def __init__(self, binary, root, records, name):
        self.directory = root / name
        self.directory.mkdir(mode=0o700)
        self.records = records
        self.name = name
        self.process = subprocess.Popen(
            [str(binary), str(self.directory)],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        try:
            self.project = self.call(
                "register",
                operation="register",
                input={
                    "name": "Disposable",
                    "path": "/synthetic/project",
                    "baseRef": "master",
                },
            )["id"]
        except BaseException:
            self.close()
            raise

    def call(self, kind, error=False, **fields):
        request = {"kind": kind, **fields}
        self.process.stdin.write(json.dumps(request) + "\n")
        self.process.stdin.flush()
        assert select.select([self.process.stdout], [], [], 10)[0], request
        line = self.process.stdout.readline()
        assert line, f"consumer exited during {request}"
        response = json.loads(line)
        self.records.append(
            {"scenario": self.name, "request": request, "response": response}
        )
        if error:
            assert "error" in response, response
            return response["error"]
        assert "ok" in response, response
        return response["ok"]

    def create(self, operation, **fields):
        return self.call(
            "create",
            operation=operation,
            input={
                "projectId": self.project,
                "title": operation,
                "instructions": "Exercise library ownership",
                **fields,
            },
        )

    def state(self):
        state = self.call("snapshot")
        # A second consumer reads real committed SQLite rows, not engine expectations.
        with sqlite3.connect(
            f"file:{self.directory / 'tandem.db'}?mode=ro", uri=True
        ) as db:
            for table in ("tasks", "attempts", "commands", "results"):
                rows = {
                    key: json.loads(body)
                    for key, body in db.execute(f"SELECT id, body FROM {table}")
                }
                assert rows == state[table], (table, rows, state[table])
            operations = {
                key: json.loads(body)
                for key, body in db.execute("SELECT id, body FROM operations")
            }
        self.records.append({"scenario": self.name, "sqliteOperations": operations})
        return state, operations

    def audit(self):
        return self.call("audit")

    def count(self, kind):
        return sum(event["kind"] == kind for event in self.audit()["events"])

    def wait(self, predicate, seconds=2):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            if predicate():
                return
            time.sleep(0.01)
        raise AssertionError(f"{self.name}: observation deadline exceeded")

    def control(self, task, action, error=False):
        return self.call(
            "control",
            target=task["id"],
            operation=action,
            error=error,
            input={"action": action, "cascade": False},
        )

    def result(self, task, operation, generation):
        state, _ = self.state()
        return self.call(
            "result",
            target=task["id"],
            operation=operation,
            input={
                "attemptId": state["tasks"][task["id"]]["attemptId"],
                "generation": generation,
                "summary": "Independently checked",
                "evidence": [
                    {
                        "name": "assertion",
                        "reference": "fixture://driver",
                        "passed": True,
                    }
                ],
            },
        )

    def ownership(self):
        for event in self.audit()["events"]:
            kind = event["kind"]
            if kind not in (
                "workspace.ensure",
                "session.ensure",
                "prompt",
                "workspace.remove",
            ):
                continue
            state = event["durable"]
            if kind == "prompt":
                command = state["commands"][event["target"]]
                assert command["status"] == "sending"
                attempt = state["attempts"][command["attemptId"]]
            else:
                attempt = state["attempts"][event["target"]]
            assert state["tasks"][attempt["taskId"]]["attemptId"] == attempt["id"]
            assert attempt["workspace"]["owner"] == attempt["id"]
            assert attempt["sessionRef"]
            if kind == "workspace.remove":
                assert attempt["phase"] == "cleanup_pending"

    def close(self):
        if self.process.poll() is None:
            try:
                self.call("release")
            finally:
                self.process.stdin.close()
            try:
                status = self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
                raise AssertionError(f"{self.name}: consumer failed to settle")
            assert status == 0, self.process.stderr.read()
        else:
            assert self.process.returncode == 0, self.process.stderr.read()
        self.process.stdout.close()
        self.process.stderr.close()


def uncertainty(c):
    c.call("configure", prompt_error="uncertain", reconcile_uncertain=True)
    first, second = c.create("first"), c.create("second")
    c.call("tick")
    state, _ = c.state()
    attempt_id = state["tasks"][first["id"]]["attemptId"]
    c.call("observe", target=first["id"], state="idle")
    for _ in range(10):
        c.call("tick")
    state, _ = c.state()
    assert state["attempts"][attempt_id]["failureCount"] == 8
    assert state["tasks"][first["id"]]["state"] == "recovering"
    assert not state["tasks"][second["id"]].get("attemptId")
    assert c.count("prompt") == c.count("session.ensure") == 1
    assert {
        event["target"]
        for event in c.audit()["events"]
        if event["kind"] == "prompt.inspect"
    } == set(state["commands"])
    c.control(first, "retry", error=True)
    c.control(first, "cancel")
    c.call("tick")
    c.call("cleanup", target=attempt_id, operation="premature-cleanup", error=True)
    assert c.count("workspace.remove") == 0
    state, operations = c.state()
    assert state["tasks"][first["id"]]["state"] == "recovering"
    assert "premature-cleanup" not in operations
    c.call("configure", prompt_error="", reconcile_uncertain=False)
    c.call("tick")
    state, _ = c.state()
    assert state["tasks"][first["id"]]["state"] == "cancelled"
    assert state["tasks"][second["id"]].get("attemptId")
    assert c.count("prompt") == 2
    c.ownership()


def generations(c):
    task = c.create("task")
    c.call("tick")
    state, _ = c.state()
    attempt = state["attempts"][state["tasks"][task["id"]]["attemptId"]]
    c.call("accept", target=task["id"], operation="idle-is-not-completion", error=True)
    receipt = c.call(
        "followup",
        target=task["id"],
        operation="follow",
        input={"text": "Second generation"},
    )
    assert receipt["generation"] == 2
    c.call("tick")
    assert c.result(task, "stale", 1)["late"]
    c.call("accept", target=task["id"], operation="stale-is-not-completion", error=True)
    assert not c.result(task, "current", 2)["late"]
    c.call("observe", target=task["id"], state="idle")
    c.call("tick")
    assert (
        c.call("accept", target=task["id"], operation="accept")["state"] == "completed"
    )
    c.call("configure", background_busy=True)
    c.call("cleanup", target=attempt["id"], operation="busy-cleanup", error=True)
    assert c.count("workspace.remove") == 0
    c.call("configure", background_busy=False)
    c.call("cleanup", target=attempt["id"], operation="cleanup")
    c.call("tick")
    state, operations = c.state()
    assert state["attempts"][attempt["id"]]["phase"] == "cleaned"
    assert c.count("workspace.remove") == 1
    assert "busy-cleanup" not in operations
    revision = state["revision"]
    assert (
        c.call(
            "followup",
            target=task["id"],
            operation="follow",
            input={"text": "Second generation"},
        )
        == receipt
    )
    assert c.state()[0]["revision"] == revision
    c.ownership()


def limits(c):
    first = c.create("first")
    child = c.create("child", parentId=first["id"])
    before, _ = c.state()
    for operation, fields in [
        ("depth", {"parentId": child["id"]}),
        ("cycle", {"parentId": first["id"], "dependencies": [first["id"]]}),
    ]:
        c.call(
            "create",
            operation=operation,
            error=True,
            input={
                "projectId": c.project,
                "title": operation,
                "instructions": "invalid graph",
                **fields,
            },
        )
    assert c.state()[0]["revision"] == before["revision"]
    dependent = c.create("dependent", dependencies=[first["id"]])
    c.create("coordinator", role="coordinator")
    c.call(
        "create",
        operation="overflow",
        error=True,
        input={
            "projectId": c.project,
            "title": "overflow",
            "instructions": "too many",
        },
    )
    c.call("tick")
    state, operations = c.state()
    assert len(state["tasks"]) == 4
    assert not {"depth", "cycle", "overflow"} & operations.keys()
    assert len(state["attempts"]) == 2  # One worker and the separate coordinator slot.
    assert not state["tasks"][child["id"]].get("attemptId")
    assert not state["tasks"][dependent["id"]].get("attemptId")
    assert c.count("prompt") == 2
    c.ownership()


def budget(c):
    c.call("configure", prompt_error="not_delivered")
    task = c.create("task")
    for _ in range(12):
        c.call("tick")
    state, _ = c.state()
    attempt_id = state["tasks"][task["id"]]["attemptId"]
    assert c.count("prompt") == 8
    assert "retry budget exhausted" in state["tasks"][task["id"]]["error"]
    c.control(task, "resume")
    c.call("configure", prompt_error="")
    c.call("tick")
    state, _ = c.state()
    assert state["tasks"][task["id"]]["attemptId"] == attempt_id
    assert state["attempts"][attempt_id]["failureCount"] == 0
    assert state["attempts"][attempt_id]["phase"] == "executing"
    # Explicit resume queues its own instruction as well as resetting delivery
    # of the proven-undelivered initial instruction. Both must be delivered.
    prompts = [
        event["target"] for event in c.audit()["events"] if event["kind"] == "prompt"
    ]
    initial = f"initial_{attempt_id}"
    assert prompts.count(initial) == 9
    assert len(prompts) == 10
    assert len(set(prompts)) == 2
    c.ownership()


def role_limits(c):
    first = c.create("worker-1")
    second = c.create("worker-2")
    coordinator = c.create("coordinator-1", role="coordinator")
    waiting = c.create("coordinator-2", role="coordinator")
    c.call("tick")
    state, _ = c.state()
    assert len(state["attempts"]) == 2
    assert state["tasks"][first["id"]].get("attemptId")
    assert state["tasks"][coordinator["id"]].get("attemptId")
    assert not state["tasks"][second["id"]].get("attemptId")
    assert not state["tasks"][waiting["id"]].get("attemptId")
    c.ownership()


def notifications(c):
    parent = c.create("parent", role="coordinator")
    c.call("tick")
    child = c.create("child", parentId=parent["id"])
    c.call("tick")
    c.result(child, "child-result", 1)
    c.call("observe", target=child["id"], state="idle")
    c.call("tick")
    state, _ = c.state()
    notifications = [
        command
        for command in state["commands"].values()
        if command["id"].startswith("coordination_")
    ]
    assert len(notifications) == 1
    assert child["id"] in notifications[0]["text"]
    c.call("tick")
    before, _ = c.state()
    c.call("tick")
    assert c.state()[0]["revision"] == before["revision"]
    c.call("accept", target=child["id"], operation="accept-child")
    c.call("tick")
    state, _ = c.state()
    assert (
        sum(
            command["id"].startswith("coordination_")
            for command in state["commands"].values()
        )
        == 1
    )
    assert (
        sum(
            command["kind"] == "signal" and command["status"] == "suppressed"
            for command in state["commands"].values()
        )
        == 1
    )
    assert "notification budget exhausted" in state["tasks"][parent["id"]]["error"]
    c.ownership()


def lifecycle(c, mode, reconnecting):
    c.call("configure", delay_bookkeeping=True)
    task = c.create("task")
    start = time.monotonic()
    c.call(
        "start",
        name="run",
        action="run",
        **({"deadline_ms": 2000} if mode == "deadline" else {}),
    )
    c.wait(lambda: c.audit()["blocked"])
    c.wait(lambda: c.count("events.started") == 1)
    if reconnecting:
        if mode == "deadline":
            time.sleep(max(0, start + 1.5 - time.monotonic()))
        c.call("disconnect")
        c.wait(lambda: c.count("events.stopped") == 1)
    if mode == "shutdown":
        c.call("start", name="shutdown", action="shutdown")
    elif mode == "cancel":
        c.call("cancel", target="run")
    c.wait(lambda: c.count("events.stopped") == 1, seconds=3)
    if mode == "deadline":
        time.sleep(max(0, start + 2 - time.monotonic()))
    time.sleep(1.1)  # Exceeds the original one-second reconnect backoff.
    assert c.count("events.started") == 1
    assert c.audit()["blocked"]
    if mode != "shutdown":
        c.call("start", name="shutdown", action="shutdown")
    time.sleep(0.05)
    assert not c.call("job", target="shutdown")["finished"]
    state, _ = c.state()
    assert state["tasks"][task["id"]]["desired"] == "active"
    assert any(command["status"] == "sending" for command in state["commands"].values())
    assert c.count("prompt") == 1
    assert c.count("interrupt") == 0
    c.call(
        "create",
        operation="after-shutdown",
        input={
            "projectId": c.project,
            "title": "rejected",
            "instructions": "no admission",
        },
        error=True,
    )
    c.call("release")
    c.wait(lambda: c.call("job", target="shutdown")["finished"])
    c.call("join", target="shutdown")
    # Cancellation/deadline may detach the API wait; settlement is proven by shutdown.
    c.wait(lambda: c.call("job", target="run")["finished"])
    c.call("join", target="run", error=mode in ("cancel", "deadline"))
    c.ownership()


def expired_bookkeeping(c):
    c.call("configure", delay_bookkeeping=True)
    task = c.create("task")
    c.call("start", name="tick", action="tick")
    c.wait(lambda: c.audit()["blocked"])
    c.call("drop_wait", target="tick")
    c.call(
        "start",
        name="control",
        action="control",
        target=task["id"],
        operation="pause",
        input={"action": "pause", "cascade": False},
    )
    time.sleep(5.1)  # The unchanged five-second bookkeeping contract must expire.
    assert not c.call("job", target="control")["finished"]
    state, operations = c.state()
    assert state["tasks"][task["id"]]["desired"] == "active"
    assert "pause" not in operations
    c.call("release")
    c.wait(lambda: c.call("job", target="control")["finished"])
    c.call("join", target="control")
    state, operations = c.state()
    assert "pause" in operations
    assert any(command["status"] == "sending" for command in state["commands"].values())
    assert c.count("prompt") == 1
    c.ownership()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("--evidence", type=Path)
    parser.add_argument("--scenario", help="Run one named consumer regression")
    args = parser.parse_args()
    binary = args.binary.absolute()
    records = []
    report = {
        "scope": "Real Rust Service/scheduler/SQLite; simulated execution only; no native acceptance",
        "head": subprocess.check_output(
            ["git", "rev-parse", "HEAD"], text=True
        ).strip(),
        "binarySha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
        "sourceSha256": {},
        "passed": [],
        "records": records,
    }
    for path in [
        *Path("crates").rglob("*.rs"),
        *Path("crates").rglob("Cargo.toml"),
        Path("Cargo.toml"),
        Path("Cargo.lock"),
        Path("Makefile"),
        Path("tools/test-engine-safety.py"),
    ]:
        report["sourceSha256"][str(path)] = hashlib.sha256(
            path.read_bytes()
        ).hexdigest()
    scenarios = [
        ("uncertainty", uncertainty),
        ("generations-cleanup", generations),
        ("limits", limits),
        ("delivery-budget", budget),
        ("role-limits", role_limits),
        ("notification-budget", notifications),
        ("expired-bookkeeping-dropped-wait", expired_bookkeeping),
    ]
    for mode in ("shutdown", "cancel", "deadline"):
        for reconnecting in (False, True):
            scenarios.append(
                (
                    f"{mode}-reconnecting-{reconnecting}",
                    lambda c, m=mode, r=reconnecting: lifecycle(c, m, r),
                )
            )
    if args.scenario:
        scenarios = [
            (name, scenario) for name, scenario in scenarios if name == args.scenario
        ]
        assert scenarios, f"Unknown scenario: {args.scenario}"
    try:
        Path(".tmp").mkdir(exist_ok=True)
        with tempfile.TemporaryDirectory(
            prefix="engine-safety-", dir=".tmp"
        ) as directory:
            for name, scenario in scenarios:
                consumer = Consumer(binary, Path(directory).absolute(), records, name)
                try:
                    scenario(consumer)
                finally:
                    consumer.close()
                report["passed"].append(name)
                print(f"PASS {name}", flush=True)
    except BaseException as error:
        report["failure"] = repr(error)
        raise
    finally:
        if args.evidence:
            args.evidence.write_text(
                json.dumps(report, ensure_ascii=False, indent=2) + "\n"
            )


if __name__ == "__main__":
    main()
