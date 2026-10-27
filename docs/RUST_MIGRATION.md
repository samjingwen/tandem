# Rust migration

## Decision and current status

The native coordination engine, HTTP service, and CLI will move from Go to Rust.
SQLite, the v1 HTTP API and its OpenAPI contract, the TypeScript OpenCode plugin, and OpenCode's ownership of model/tool execution remain unchanged.
This is a compatibility migration, not a new product, parallel scheduler, or permission to start/adopt existing native services.

**Current milestone: domain, SQLite, and abstract-port coordination libraries.**
The production executable and scheduler are still Go.
`tandem-engine` implements command admission and reconciliation over runtime/workspace interfaces, exercised with controlled fixtures.
No concrete Rust OpenCode adapter, checkout allocator, HTTP service, or production CLI is delivered by these libraries.
Do not point them at a retained state directory as a cutover experiment.

## Boundaries

| Layer | Rust boundary | Status |
| --- | --- | --- |
| Domain/storage contract | `crates/tandem-core` | Typed retained records, strict command inputs, cancellation context, runtime/workspace/service ports, raw JSON outcomes, Store/Revision interface |
| SQLite adapter | `crates/tandem-store` | Transactional schema-1 store, lifetime OS lock, append-only history and immutable accepted receipts |
| Scheduling and recovery | `crates/tandem-engine` | Admission, controls, reconciliation, bounded coordinator signals and cleanup over abstract ports; controlled-runtime verification, not native acceptance |
| OpenCode and worktrees | Concrete runtime/filesystem adapters behind core interfaces | Not migrated |
| HTTP service | Same `/v1` contract, auth, bounded reads/receipts, cursors, live-only SSE | Not migrated |
| CLI and composition root | Same client receipts/options; daemon alone owns scheduling | Not migrated |
| Plugin | Existing TypeScript package | Remains TypeScript and API-only |

Use a pinned Cargo workspace and Rust 1.95.0, with unsafe Rust forbidden in maintained crates.
Concrete adapters must never become dependencies of the core or engine (except explicit test dependencies).
The core's cooperative context uses no executor; Tokio and tracked service-owned tasks belong to the engine crate.
The service/runtime implementation can use Tokio, Axum and reqwest, and the CLI can use clap when those layers are ported; add exact dependency versions and executable contract tests at that point, not empty crates or placeholder handlers now.
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
Explicit cancellation or shutdown signals its cooperative context, while the tracked operation retains the store and effects barrier through independent five-second outcome bookkeeping.
Shutdown closes admission atomically with task registration, stops scheduler reads/event listening, and waits for owned tasks without stopping external workers.
A storage lock may delay return beyond the bookkeeping deadline; its callback must then refuse the expired change, and ownership remains retained until the blocking storage task actually returns.
Runtime ports must honor cancellation and classify proven non-delivery separately from uncertainty.
Cleanup requires terminal intent, settled dispatch, current owned foreground quiescence and the runtime's separate background-resource quiescence capability.

The executable Go oracle verifies request field order/omission, HTML and U+2028/U+2029 escaping, raw-answer compaction/order/number spelling, all eight command kinds' old receipts, operation conflicts, decision identifiers and coordinator signal bodies.
An actual Rust-created task/receipt is reopened and replayed by the Go service.
The Rust suite separately exercises lifetime/cancellation barriers, admission-before-effects, same-reference recovery, generations, role/dependency bounds, finite budgets, quiet polls, explicit decisions and cleanup barriers.
These are library/SQLite contracts with simulated external execution, not native adapter behavior or HTTP/CLI parity.

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
`make rust-safety` builds a disposable JSON-lines Rust Service/SQLite consumer and drives it with Python 3, independently asserting durable rows, pre-effect ownership, uncertainty, generations, limits, cleanup and blocked-bookkeeping cancellation.
It also runs under `make rust-parity`; external runtime/workspace execution is simulated, not native acceptance.
All compatibility stores, leases and forced-crash processes belong to newly created disposable fixtures, with no retained application data, runtime session, provider credential, real model, or shared service involved.
The Rust quality proof runs the formatter, Clippy and compiler against conforming and deliberately violating fixtures and retains non-secret output in `.tmp`.
`make test-race` remains Go race-detector evidence, not a Rust sanitizer result.

The current milestone proves storage/model and abstract-port coordination behavior only, not migration completion, live OpenCode compatibility, production cutover, external consumer acceptance or permission to publish/deploy an executable.
