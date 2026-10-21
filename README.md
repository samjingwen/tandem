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

Release builds use `CGO_ENABLED=0`; SQLite is embedded through a pure-Go driver.
`make build` builds both the Go executable and optional plugin; `make build-core` needs only Go.
The race detector is a development check and needs its supported native C toolchain.
Go dependencies and tools are pinned in `go.mod`/`go.sum`; plugin dependencies use their own npm lockfile.

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

The intended Tenaxe path is Angular mobile web -> Tenaxe Spring backend -> Tandem API -> OpenCode.
The browser must not receive runtime/provider credentials or an unrestricted execution proxy.
Your existing coordinator in a multi-project directory can also be a client; it need not run inside Tandem's source checkout.
Existing sessions, validation gates, and services are never automatically imported or taken over.

## Limits

This MVP is single-host and single-user, for trusted local projects.
Worktrees isolate checkouts, not operating-system authority.
The client SSE stream is live-only and does not replay missed events; reconnecting clients fetch snapshots.
The default retained-task bound is 128, configurable at startup; there is no task archival UI yet.
Result evidence is submitted for explicit review, not automatically treated as independently verified.
Acceptance is not permission to merge, publish, deploy, or remove artifacts.
There is no distributed scheduling, remote worker provisioning, cross-machine synchronization, billing, or multi-tenancy.

## License and attribution

MIT.
Tandem selectively adapts coordination ideas from [Kun Chen's Firstmate](https://github.com/kunchenguid/firstmate), not its distribution or V1 plugins.
See [third-party notices](THIRD_PARTY_NOTICES.md) for the inspected upstream revision and retained notice.
