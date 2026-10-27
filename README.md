# Tandem

Persistent coordination for coding agents.

Tandem is an open-source coordination engine with a standalone CLI and versioned HTTP API.
It owns durable projects, tasks, dependencies, worker assignments, decisions, execution attempts, and recovery.
OpenCode V2 runs the coordinator/worker sessions and retains its native session data.
Tenaxe and other applications consume the same API rather than running another scheduler.

**Work belongs to an instance, not a client connection.**
Start a task through one client, disconnect it, and inspect or continue that same task through another client connected to the same service.
Separate local/server instances have separate state; installation does not synchronize them.

## Requirements

- Go **1.27.3** for building.
- Rust **1.95.0** with rustfmt/Clippy and a C compiler for the bundled SQLite migration adapter.
- Git and OpenCode **2.1.0**, with a configured usable model.
- Worktrunk **0.80.0** for the default checkout allocator, or explicitly select `--allocator git` where project policy permits it.
- Node 24+ and npm only for building/checking the optional TypeScript plugin.
- A local macOS/Linux filesystem for SQLite state and locking, not NFS or a shared cross-host database.

OpenCode 2.1.0 is deliberately pinned, not automatically upgraded to the latest release.
The adapter contract is checked against that release's source and executable tests.
See [runtime compatibility](docs/OPENCODE.md) for the exact evidence and live-smoke instructions.

## Build and check

```sh
make prepare
make check
make test-race
make security
make prove-quality
make build
./bin/tandem --version
```

`make build` builds the current Go executable, optional plugin, and Rust migration libraries.
`make build-core` needs only Go and uses `CGO_ENABLED=0`, embedding SQLite through a pure-Go driver.
The race detector is a development check and needs its supported native C toolchain.
Go dependencies and tools are pinned in `go.mod`/`go.sum`; plugin dependencies use their own npm lockfile.
The native engine, service, and CLI are [migrating to Rust](docs/RUST_MIGRATION.md), preserving SQLite, the v1 HTTP API, and the TypeScript plugin.
The current executable is still Go; Rust libraries implement domain/storage and coordination over abstract runtime/workspace ports, not a replacement daemon or CLI yet.
Rust dependencies use `Cargo.toml`/`Cargo.lock`; `rust-toolchain.toml` pins the toolchain.
Python quality tooling requires Python 3.11+ with `venv` and pip (CI uses 3.11).
`make prepare` installs pinned Ruff from `tools/python-requirements.txt` into `.tmp/python-tools`, without changing global tools.
`make lint` checks formatting and focused Python lint for the maintained `tools/test-engine-safety.py` driver only; dependencies, generated files and private data are excluded.
`make format` applies safe Ruff fixes and formatting; `make prove-quality` also exercises disposable Python analyzer consumers.
`make check` includes Rust tests and real Go/Rust/Go and Rust/Go/Rust persistence/ownership tests against fresh disposable stores.
See the [verification contract](docs/VERIFICATION.md#routine-deterministic-checks) for coordination/oracle coverage and its acceptance limits.
No retained database, native service, or provider is needed for those checks.

## Connect a runtime and start an instance

First ensure your intended OpenCode 2.1.0 service is already running and healthy through its supported CLI.
The setup command uses OpenCode's supported pairing flow, not private authentication-cache inspection.
It stores only a service connection grant, never a copy of provider credentials.

```sh
export TANDEM_STATE="$HOME/.local/share/tandem/default"
./bin/tandem runtime-connect --state-dir "$TANDEM_STATE"
./bin/tandem serve --state-dir "$TANDEM_STATE" --allocator worktrunk
```

The daemon prints a JSON startup receipt containing its URL, stable instance ID, version, and client-token file path, but never a token value.
The default address is `http://127.0.0.1:7433`.
Keep the daemon running independently of task clients; for terminal-independent operation, use an OS service supervisor as described in [operations](docs/OPERATIONS.md).
There is no automatic global service installation.

Alternatively, supply an already-authorized runtime endpoint through `OPENCODE_URL` and `OPENCODE_PASSWORD`.
Use `serve --model provider/model#variant` to select an existing configured model when the native default is unsuitable for service-driven work.
For example, some free-tier providers reject API-driven sessions; that rejection is not proof of expired credentials.
No particular vendor is required, and Tandem never copies provider credentials or implements model inference itself.
The service refuses unauthenticated runtime connections.
Do not place either runtime grants or provider credentials in a repository or task instructions.

In a separate client shell, explicitly select that instance:

```sh
export TANDEM_URL="http://127.0.0.1:7433"
export TANDEM_TOKEN="$(cat "$TANDEM_STATE/client.token")"
./bin/tandem doctor
```

`client.token` is Tandem-owned, owner-only client configuration, not an OpenCode credential cache.
If you set `TANDEM_TOKEN` before starting the daemon, clients must use that configured token instead.
Authentication is distinct from network exposure: a public source repository does not make your execution service public.

## Register a project and run work

Choose the intended base ref explicitly; do not assume every repository uses the same default branch.
Registration reads repository/ancestor instructions and preserves existing canonical-checkout changes.
Use `--instruction-files /absolute/path/to/shared-policy.md` for shared guidance outside the repository that workers must receive without broad read permissions.
Re-register the same canonical path with a new operation ID to refresh its base ref/instructions while keeping its project identity; existing attempts retain their assignments.

```sh
./bin/tandem --json --operation-id register-example projects register \
  --name example --path /absolute/path/to/repository --base-ref master

./bin/tandem --json --operation-id first-task tasks create \
  --project PROJECT_ID --title "Fix the failing test" \
  --instructions "Reproduce the failure, fix it, and provide verification evidence."

./bin/tandem tasks show TASK_ID
```

Replace the returned IDs, and use a new stable operation ID for each distinct request.
Retry an ambiguous request with its original operation ID and exact input, never a new ID.
The CLI prints generated operation IDs before mutation dispatch when you do not supply one.

For conversational decomposition, create a task with `--role coordinator` and configure the thin OpenCode plugin's Tandem tools in that runtime location.
The coordinator proposes and dispatches bounded subtasks through validated engine operations.
The default engine limit is two workers plus one active coordinator execution.
It does not infer completion from a quiet or idle session.

## Reconnect from another client

Close the first CLI or browser, leaving the daemon independent.
In another shell or application, configure the same service URL and authorization, then:

```sh
./bin/tandem tasks list
./bin/tandem tasks show TASK_ID
./bin/tandem --operation-id followup-1 tasks followup TASK_ID \
  --text "Also cover the empty-input case."
```

This continues the current owned task/session when reconciliation permits it.
Restarting Tandem reloads the same store and reconciles existing workers rather than starting replacements speculatively.
When an outcome is uncertain, the task remains recovering and reserves capacity.

## Clients and integrations

- [HTTP schemas, commands, errors, SSE, and CLI](docs/API.md)
- [Architecture and state/recovery decisions](docs/ARCHITECTURE.md)
- [Operation, cancellation, permissions, cleanup, and security](docs/OPERATIONS.md)
- [OpenCode release contract and plugin](docs/OPENCODE.md)
- [Acceptance cases and fake/native/model verification boundaries](docs/VERIFICATION.md)

The intended Tenaxe path is Angular mobile web -> Tenaxe Spring backend -> Tandem API -> OpenCode.
The browser must not receive runtime/provider credentials or an unrestricted execution proxy.
Your existing coordinator in a multi-project directory can also be a client; it need not run inside Tandem's source checkout.
Existing sessions, validation gates, and services are never automatically imported or taken over.

## Limits

This MVP is single-host and single-user, for trusted local projects.
Worktrees isolate checkouts, not operating-system authority.
The client SSE stream is live-only and does not replay missed events; reconnecting clients fetch snapshots.
New clients use the additive byte-bounded `/v1/reads/` manifest, record pages, and complete-content pages rather than unrestricted inventories.
The CLI's `snapshot` is a lean manifest; `snapshot --full` explicitly streams complete retained records without assembling an unbounded State in memory.
Legacy HTTP read shapes remain available unchanged; see [the read contract](docs/API.md#bounded-reads-and-compatibility) for explicit migration and stale-cursor recovery.
The default retained-task bound is 128, configurable at startup; there is no task archival UI yet.
Result evidence is submitted for explicit review, not automatically treated as independently verified.
Acceptance is not permission to merge, publish, deploy, or remove artifacts.
There is no distributed scheduling, remote worker provisioning, cross-machine synchronization, billing, or multi-tenancy.

## License and attribution

MIT.
Tandem selectively adapts coordination ideas from [Kun Chen's Firstmate](https://github.com/kunchenguid/firstmate), not its distribution or V1 plugins.
See [third-party notices](THIRD_PARTY_NOTICES.md) for the inspected upstream revision and retained notice.
