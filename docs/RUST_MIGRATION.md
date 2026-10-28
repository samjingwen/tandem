# Rust migration

## Decision and current status

The native coordination engine, HTTP service, and CLI will move from Go to Rust.
SQLite, the v1 HTTP API and its OpenAPI contract, the TypeScript OpenCode plugin, and OpenCode's ownership of model/tool execution remain unchanged.
This is a compatibility migration, not a new product, parallel scheduler, or permission to start/adopt existing native services.

**Current milestone: domain, SQLite, coordination, controlled HTTP runtime and concrete workspace-adapter libraries.**
The production executable and scheduler are still Go.
`tandem-engine` implements command admission and reconciliation over runtime/workspace interfaces, exercised with controlled fixtures.
`tandem-opencode` implements the pinned OpenCode 2.1.0 HTTP/SSE Runtime port, verified against disposable HTTP fixtures and the executable Go adapter, not a native OpenCode process.
`tandem-workspace` implements canonical registration, recorded-ownership Git/Worktrunk allocation and guarded cleanup, verified with disposable repositories and the Go reference.
No Rust HTTP service or production CLI is delivered by these libraries.
Do not point them at a retained state directory as a cutover experiment.

## Boundaries

| Layer | Rust boundary | Status |
| --- | --- | --- |
| Domain/storage contract | `crates/tandem-core` | Typed retained records, strict command inputs, cancellation context, runtime/workspace/service ports, raw JSON outcomes, Store/Revision interface |
| SQLite adapter | `crates/tandem-store` | Transactional schema-1 store, lifetime OS lock, append-only history and immutable accepted receipts |
| Scheduling and recovery | `crates/tandem-engine` | Admission, controls, reconciliation, bounded coordinator signals and cleanup over abstract ports; controlled-runtime verification, not native acceptance |
| OpenCode HTTP/SSE | `crates/tandem-opencode` behind the Runtime port | Pinned wire/ownership/recovery adapter with controlled HTTP verification only; no native acceptance |
| Worktrees | `crates/tandem-workspace` behind Workspaces | Read-only registration, pure planning, owned allocation/recovery and guarded branch-preserving cleanup; disposable Git/Worktrunk verification |
| HTTP service | Same `/v1` contract, auth, bounded reads/receipts, cursors, live-only SSE | Not migrated |
| CLI and composition root | Same client receipts/options; daemon alone owns scheduling | Not migrated |
| Plugin | Existing TypeScript package | Remains TypeScript and API-only |

Use a pinned Cargo workspace and Rust 1.95.0, with unsafe Rust forbidden in maintained crates.
Concrete adapters must never become dependencies of the core or engine (except explicit test dependencies).
The core's cooperative context uses no executor; Tokio and tracked service-owned tasks belong to the engine crate.
The runtime adapter uses Tokio and reqwest; the service can use Axum and the CLI can use clap when those layers are ported, with exact dependency versions and executable contract tests rather than empty crates or placeholder handlers.
Bundled SQLite needs a build-time C compiler but no separately provisioned SQLite service or library at runtime.

## Compatibility rules

- Keep all stable project/task/attempt/session/workspace identities and instruction generations.
- Preserve desired versus observed state, uncertainty, cancellation tombstones, pending replies, bounded dispatch/notification budgets, and ownership before external mutation.
- Session idleness is not completion evidence or permission for destructive cleanup.
- Every accepted mutation must retain the same caller operation identity, normalized fingerprint and exact immutable outcome, committed with its state change.
- Go's request serialization is part of existing fingerprint compatibility: ordinary Rust JSON serialization is not automatically byte-identical, especially for field order, omitted/zero fields, HTML characters, numbers and raw answers.
- Cross-language replay/conflict tests cover those fingerprint rules before any production Rust command surface is introduced.
- Preserve complete content and exact receipts, including arbitrary JSON and integer/decimal precision, rather than silently truncating or replacing an old receipt with current task state.
- Preserve all legacy HTTP shapes, strict request budgets, typed failures, authentication/browser boundaries, no mutation redirect/retry, bounded read filters and byte caps, process-scoped cursors, and live-only SSE semantics.
- One canonical state directory has one lifetime OS lock and one scheduling owner, including across Go and Rust processes and symlink aliases.
- Keep schema migrations append-only and data-preserving; the initial Rust store reuses the original SQL migration instead of creating a competing schema lineage.

The Rust store serializes snapshots/transactions behind one mutex and reads each snapshot at one SQLite transaction point.
An unchanged transaction does not change revision or append history.
Unchanged persisted rows retain their exact stored JSON, including accepted operation bodies; only changed records append history at the new atomic revision.
Unknown record fields and original timestamp spellings survive unrelated Rust updates.
Timestamp loading matches Go's RFC3339 JSON behavior, including legacy single-digit hours, comma fractions, long fractions and boundary offsets; sorting compares instants rather than the retained strings.
An existing operation receipt cannot be changed or removed, retained records cannot be deleted through the generic update boundary, and caller attempts to change the instance/revision are rejected.
These fail-closed restrictions protect persistence; the full engine must still enforce graph, dispatch, generation, control and acceptance invariants.

## Coordination library

The engine persists attempt/workspace/session ownership before allocating or invoking execution, and journals accepted commands atomically with their original immutable outcomes.
Its tick lock serializes reconciliation; a separate effects lock serializes external mutation admission and control/cleanup barriers.
An ambiguous prompt is reconciled read-only by its existing command/session reference, never retried as a new mutation.
Uncertainty retains capacity even when the runtime is foreground-idle.
Automatic dispatch failures stop at eight, while explicit resume preserves the attempt and resets its delivery budget.
Coordinators have a separate single-slot limit and a default 32-notification budget; unchanged evidence is quiet and current signal storage coalesces by child.

Dropping an API/client future abandons only that wait, not the owned operation.
Explicit cancellation, deadline expiry or shutdown signals its cooperative context, while the tracked operation retains the store and effects barrier through independent five-second outcome bookkeeping.
Workspace calls await actual subprocess or blocking-filesystem settlement before outcome bookkeeping, including registration and acceptance inspection; cancellation does not release ownership by dropping the adapter future.
Shutdown closes admission atomically with task registration, stops scheduler reads/event listening, and waits for owned tasks without stopping external workers.
The event listener shares the owned run's cancellation and deadline, so event reads and reconnection stop even while outcome bookkeeping is blocked.
A storage lock may delay return beyond the bookkeeping deadline; its callback must then refuse the expired change, and ownership remains retained until the blocking storage task actually returns.
Runtime ports must honor cancellation; only an explicit `Error::not_delivered` classification proves a failed write can safely return to pending delivery.
An `invalid_request` reply error without that proof retains uncertainty, rather than clearing the answer for another reply.
Cleanup requires terminal intent, settled dispatch, current owned foreground quiescence and the runtime's separate background-resource quiescence capability.

See the [verification contract](VERIFICATION.md#routine-deterministic-checks) for executable oracle and controlled-consumer coverage and its acceptance limits.

## Concrete runtime adapter

See the [OpenCode integration contract](OPENCODE.md#rust-runtime-migration) for Rust configuration, stable identities, permission isolation, uncertain recovery, cancellation and quiescence behavior.
Neither the Go binary nor a retained state directory is switched to these libraries by compiling or testing them.

## Concrete workspace adapter

The composition root supplies a canonical state/worktree root; construction and planning do not create directories or launch commands.
The adapter validates recorded allocator, owner, path, branch and common repository before adopting an existing checkout, and checks ownership before creating a missing checkout.
Recovery returns the existing checkout's current HEAD, matching Go rather than resetting worker commits to the original base.
Command failure or cancellation never triggers speculative allocation retry or checkout rollback.
The engine remains responsible for persisting ownership before allocation and proving terminal intent and runtime quiescence before cleanup.
See [operations](OPERATIONS.md#workspace-adapter-boundary) for filesystem, subprocess, credential and cleanup limits, and [verification](VERIFICATION.md#workspace-adapter-checks) for actual executable consumers.

## Delivery sequence and cutover gates

1. **Persistence foundation:** typed domain, schema-1 storage, Go/Rust bidirectional reopen, cross-process/alias ownership, rollback, crash recovery, exact raw receipts, complete content, and deterministic quality/security checks.
2. **Engine parity:** port admission/fingerprints, graph/concurrency limits, coordinator notifications, explicit control barriers, effects bookkeeping, cleanup and reconciliation.
   Execute both implementations against the same scenario corpus with an independently asserted result and failure-injected fake runtime, covering all cases in [VERIFICATION.md](VERIFICATION.md).
3. **Adapter parity:** port exact supported OpenCode HTTP/SSE behavior, canonical workspace planning/allocation, permission isolation, pending-admission recovery and quiescence checks.
   Native compatibility remains a separate explicitly authorized fresh-fixture boundary; old Go evidence does not verify Rust code.
4. **Client parity:** port HTTP, byte-bounded traversal/receipt recovery, CLI and daemon startup/shutdown/token/locking behavior.
   Exercise the actual Rust executable with independent clients, the unchanged plugin and the served OpenAPI consumer, not only internal mocks.
5. **Cutover:** make Rust the default only after engine, transport, persistence and adapter evidence is current, required checks/reviews pass, and separate active gates/retained-runtime custody permit the transition.
   Keep the Go implementation/reference tests until rollback and the initial Rust release are verified.
   Removing Go and changing trusted gate command bindings is a separately reviewed final step, not an incidental consequence of the first Cargo workspace.

A separately in-flight runtime-version upgrade must retain its own branch/gate and evidence.
Do not copy unreviewed gate commits or silently combine a language migration with a runtime upgrade.
Until that change is delivered, the canonical [runtime compatibility document](OPENCODE.md) remains authoritative.

## Verification commands

Run the [routine deterministic checks](VERIFICATION.md#routine-deterministic-checks) for this milestone.

Focused Rust checks are `make rust-test rust-parity rust-lint rust-security rust-build`.
`make prepare` installs pinned cargo-audit into task-local `.tmp/rust-tools`, not a global executable directory.
`make rust-parity` builds the test-only `store-compat` and `engine-compat` examples and supplies their explicit absolute paths to the Go tests; ordinary Go-only tests report these cross-language fixtures skipped when not opted in.
The aggregate check and CI execute the opted-in fixture, so those skips are not substituted for interoperability evidence.
The explicit-directory `engine-compat` consumer disables native effects; its `observe` mode accepts synthetic read-only observations/inspection and fails if any write is attempted, even when reconciliation handles the per-attempt error.
`make rust-safety` builds the separate `engine-safety` consumer and drives it with Python 3; it also runs under `make rust-parity`.
Runtime-adapter commands and executable oracle coverage are documented in the [verification contract](VERIFICATION.md#routine-deterministic-checks).
All compatibility stores, leases and forced-crash processes belong to newly created disposable fixtures, with no retained application data, runtime session, provider credential, real model, or shared service involved.
The Rust quality proof runs the formatter, Clippy and compiler against conforming and deliberately violating fixtures and retains non-secret output in `.tmp`.
`make test-race` remains Go race-detector evidence, not a Rust sanitizer result.

The current milestone proves storage/model, coordination, controlled HTTP runtime and disposable Git/Worktrunk adapter behavior only, not migration completion, live OpenCode compatibility, production cutover, external consumer acceptance or permission to publish/deploy an executable.
