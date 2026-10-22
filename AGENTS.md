# Tandem

Tandem is an open-source persistent coordination engine, not an agent harness.
Use Go 1.27.3, SQLite, and the versioned HTTP API shared by all clients.
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

`make check` runs formatting checks, vet, static analysis, and tests.
`make test-race` runs the race detector.
`make build` produces the native CLI/service binary.
`make format` is the separate formatting fix.

Dependency versions belong in go.mod/go.sum and plugin/package-lock.json.
Generated files must be regenerated, not hand-edited.
