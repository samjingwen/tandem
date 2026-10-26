# Tandem

Tandem is an open-source persistent coordination engine, not an agent harness.
The native engine, service, and CLI are migrating to Rust 1.95.0 under `docs/RUST_MIGRATION.md`.
Keep Go 1.27.3 as the executable/reference implementation until behavioral cutover criteria pass.
Preserve SQLite compatibility, the versioned HTTP API shared by all clients, and the TypeScript OpenCode plugin.
OpenCode V2 owns model/tool execution and its native session data.

## Engineering

- Keep the core independent of concrete runtime and SQLite packages.
- Preserve dispatch uncertainty; never retry an ambiguous external mutation speculatively.
- Session idleness is not completion evidence.
- Persist ownership before allocating workspaces or invoking a runtime.
- Worktrees are checkout isolation, not security sandboxes.
- Keep runtime state, credentials, transcripts, and machine-specific handoffs outside Git.
- Never read private environment/authentication files.
- Use dedicated Worktrunk worktrees; preserve other workers and gates.
- Do not merge/publish a managed task merely because its worker reports success.

## Commands

`make check` runs Go/Rust/plugin checks and Go/Rust storage interoperability tests.
`make test-race` runs the Go reference race detector, not a Rust race detector.
`make build` produces the current Go CLI/service binary, Rust migration libraries, and plugin.
`make format` is the separate formatting fix.
`make rust-test rust-parity` checks the current Rust migration milestone.

Rust toolchains are pinned in rust-toolchain.toml and dependencies in Cargo.toml/Cargo.lock.
Reference Go and plugin versions remain in go.mod/go.sum and plugin/package-lock.json.
Generated files must be regenerated, not hand-edited.
