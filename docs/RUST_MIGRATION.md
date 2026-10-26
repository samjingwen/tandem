# Rust migration

## Decision and current status

The native coordination engine, HTTP service, and CLI will move from Go to Rust.
SQLite, the v1 HTTP API and its OpenAPI contract, the TypeScript OpenCode plugin, and OpenCode's ownership of model/tool execution remain unchanged.
This is a compatibility migration, not a new product, parallel scheduler, or permission to start/adopt existing native services.

**Current milestone: domain and SQLite foundation.**
The production executable and scheduler are still Go.
The Rust crates are not a replacement daemon and do not expose command admission, runtime dispatch, HTTP endpoints, or a production CLI yet.
Do not point them at a retained state directory as a cutover experiment.

## Boundaries

| Layer | Rust boundary | Status |
| --- | --- | --- |
| Domain/storage contract | `crates/tandem-core` | Typed retained records, raw JSON outcomes, errors, Store/Revision interface |
| SQLite adapter | `crates/tandem-store` | Transactional schema-1 store, lifetime OS lock, append-only history and immutable accepted receipts |
| Scheduling and recovery | Runtime-independent core state transitions and explicit effects boundary | Not migrated |
| OpenCode and worktrees | Concrete runtime/filesystem adapters behind core interfaces | Not migrated |
| HTTP service | Same `/v1` contract, auth, bounded reads/receipts, cursors, live-only SSE | Not migrated |
| CLI and composition root | Same client receipts/options; daemon alone owns scheduling | Not migrated |
| Plugin | Existing TypeScript package | Remains TypeScript and API-only |

Use a pinned Cargo workspace and Rust 1.95.0, with unsafe Rust forbidden in maintained crates.
Concrete adapters must never become dependencies of the core.
The service/runtime implementation can use Tokio, Axum and reqwest, and the CLI can use clap when those layers are ported; add exact dependency versions and executable contract tests at that point, not empty crates or placeholder handlers now.
Bundled SQLite needs a build-time C compiler but no separately provisioned SQLite service or library at runtime.

## Compatibility rules

- Keep all stable project/task/attempt/session/workspace identities and instruction generations.
- Preserve desired versus observed state, uncertainty, cancellation tombstones, pending replies, bounded dispatch/notification budgets, and ownership before external mutation.
- Session idleness is not completion evidence or permission for destructive cleanup.
- Every accepted mutation must retain the same caller operation identity, normalized fingerprint and exact immutable outcome, committed with its state change.
- Go's request serialization is part of existing fingerprint compatibility: ordinary Rust JSON serialization is not automatically byte-identical, especially for field order, omitted/zero fields, HTML characters, numbers and raw answers.
- Do not admit real Rust commands until cross-language replay/conflict tests cover those fingerprint rules.
- Preserve complete content and exact receipts, including arbitrary JSON and integer/decimal precision, rather than silently truncating or replacing an old receipt with current task state.
- Preserve all legacy HTTP shapes, strict request budgets, typed failures, authentication/browser boundaries, no mutation redirect/retry, bounded read filters and byte caps, process-scoped cursors, and live-only SSE semantics.
- One canonical state directory has one lifetime OS lock and one scheduling owner, including across Go and Rust processes and symlink aliases.
- Keep schema migrations append-only and data-preserving; the initial Rust store reuses the original SQL migration instead of creating a competing schema lineage.

The Rust store serializes snapshots/transactions behind one mutex and reads each snapshot at one SQLite transaction point.
An unchanged transaction does not change revision or append history.
Unchanged persisted rows retain their exact stored JSON, including accepted operation bodies; only changed records append history at the new atomic revision.
Unknown record fields and original timestamp spellings survive unrelated Rust updates.
An existing operation receipt cannot be changed or removed, retained records cannot be deleted through the generic update boundary, and caller attempts to change the instance/revision are rejected.
These fail-closed restrictions protect persistence; the full engine must still enforce graph, dispatch, generation, control and acceptance invariants.

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
`make rust-parity` builds the test-only `store-compat` example and supplies its explicit absolute path to the Go tests; ordinary Go-only tests report this cross-language fixture skipped when not opted in.
The aggregate check and CI execute the opted-in fixture, so those skips are not substituted for interoperability evidence.
All compatibility stores, leases and forced-crash processes belong to newly created disposable fixtures, with no retained application data, runtime session, provider credential, real model, or shared service involved.
The Rust quality proof runs the formatter, Clippy and compiler against conforming and deliberately violating fixtures and retains non-secret output in `.tmp`.
`make test-race` remains Go race-detector evidence, not a Rust sanitizer result.

The first milestone proves storage/model behavior only, not migration completion, live OpenCode compatibility, production cutover, external consumer acceptance or permission to publish/deploy an executable.
