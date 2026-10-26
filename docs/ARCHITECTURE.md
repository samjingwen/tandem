# Architecture and decisions

Tandem is an open-source persistent coordination engine for coding agents.
It is independently usable and is also an engine that applications such as Tenaxe consume.

## Boundaries

```text
CLI -----------+
OpenCode tools +--> Tandem /v1 API --> coordination engine --> SQLite
Tenaxe Spring -+                            |
                                           +--> worktree allocator
                                           +--> OpenCode HTTP/SSE adapter
                                                     |
                                             coordinator and workers
```

The initial deployment is one Go executable, one single-user daemon, and one local SQLite store.
The selected replacement is one Rust executable with the same daemon/store ownership and client contracts; see the [staged migration](RUST_MIGRATION.md).
Go remains the current executable and differential reference until the Rust engine, adapters, API, and CLI satisfy the cutover gates.
The public HTTP API is the initial integration contract; the Go package is not yet a separately versioned stable embedding SDK.
The core depends on narrow Store, Runtime, and Workspaces interfaces, not OpenCode SDK objects.
The standalone CLI, thin OpenCode plugin, and Tenaxe backend issue the same operations.
Only the daemon owns the scheduler.
OpenCode remains responsible for its native sessions, configured providers/models, permissions, and tool loop.
Tandem does not implement a competing model loop.

## Language migration and SQLite

The project began empty.
After discussing TypeScript and Java/Spring/GraalVM, the user selected the Go setup for the standalone engine.
Go 1.27.3 matches the verified installed toolchain.
The service uses the standard HTTP/JSON libraries and database/sql with modernc SQLite for CGO-free release builds.
Dependency versions and tools are pinned in go.mod, with integrity checksums in go.sum.
The subsequent user decision is to migrate the native engine, service, and CLI to Rust, not to replace SQLite or the HTTP/plugin contracts.
The Rust core stays independent of concrete storage/runtime packages; the storage crate uses rusqlite with bundled SQLite and the original version-1 SQL migration.
Its compiler/tooling and Cargo dependency graph are separately pinned, with runtime cutover blocked on behavioral and cross-language persistence evidence.
Tenaxe retains Angular and Java/Spring.
SQLite is local-host storage, not a distributed coordination mechanism.
No Redis, external workflow engine, distributed queue, or cross-machine state replication is introduced.

## State and transactions

Stable project and task IDs are distinct from execution-attempt IDs and opaque runtime session references.
Each attempt records its runtime binding, assigned worktree, branch/base, generation, phase, and observations.
Task desired state is distinct from observed execution state.
SQLite migrations are append-only, transactional, and version-checked.
The store has typed document tables, path/session uniqueness indexes, a durable operation journal, and append-only history.
The first MVP loads the bounded task inventory when applying changes; this favors a small auditable transaction boundary over an ORM.
This is not intended as an unbounded multi-tenant task warehouse.
The additive HTTP read interface keeps the manifest and every record/content response byte-bounded independently of inventory and retained text sizes.
It never silently projects legacy HTTP responses; clients migrate explicitly and traverse complete records with revision-bound cursors.
A capped per-handler immutable-content cache avoids reloading complete state for every small chunk; a storage-independent revision interface rechecks instance/revision atomically before cached content is served.
Cache eviction changes performance only, not content availability, revision validation, or admission limits.

Every mutating API request requires a caller-stable operation ID.
The operation's normalized input fingerprint and outcome commit with the state transition.
Identical retries return the original receipt; conflicting reuse fails.
External calls and commits are not one transaction.
Dispatch intent, worktree ownership, and runtime references are persisted before their external effects.
Recoverable external calls use the same verified identifiers, never a speculative replacement session.
After an external call returns, effects-owned outcome/failure bookkeeping uses a separate five-second context before releasing effect ownership, so cancellation of the call context does not erase its outcome.

## Ownership and concurrency

The state directory is canonicalized before acquiring an OS-held lock.
The lock is held for the daemon lifetime and is not stolen based on PID age or heartbeat expiry.
A second service must fail and connect as a client instead.
The default bound is two workers plus one active coordinator execution.
Uncertain attempts reserve capacity until reconciled.
Task count and decomposition depth are bounded.
Dependencies must exist in the same project; new dependency edges only point from a new task to existing tasks.

The coordinator uses judgment to route work and propose subtasks through validated operations.
New child results, failures, and actual pending questions generate durable, deduplicated coordinator notifications.
Notifications are coalesced, version the coordinator's current context, and are capped per attempt; idleness and unchanged progress never trigger a model turn.
The engine, not the model, validates ownership, eligibility, hierarchy, and concurrency.
Native unmanaged delegation must remain disabled in the configured task runtime.

## Recovery and events

On startup the engine checks runtime compatibility, inspects unfinished bindings and worktrees, reconciles desired/observed state, and only then dispatches eligible work.
Events are coalesced wake hints, not current-state truth.
Runtime event reconnection uses bounded backoff and independent periodic state inspection.
OpenCode 2.1.0's event stream has no replay guarantee.
Tandem's initial client SSE stream is also live-only; reconnecting clients refresh a versioned snapshot.
No browser or terminal connection owns worker lifetime.
Different service instances have different stores and do not automatically synchronize.

## Completion and controls

Task states include queued, dispatching, running, waiting_for_input, paused, ready_for_review, completed, failed, cancelled, and recovering.
An idle session is never task-completion evidence.
Worker result submissions identify the attempt and instruction generation and include explicit evidence references.
Evidence is a worker assertion awaiting review, not a trusted check merely because it is reported as passing.
Explicit acceptance requires current evidence, confirmed quiescence, resolved decisions, and completed subtasks.
Acceptance is not authorization to merge, deploy, publish, or delete artifacts.

Pause/cancel controls serialize their committed barrier with external mutation admission.
An already in-flight mutation may finish before the barrier takes effect; its resulting execution is reconciled and interrupted.
Acknowledged control intent is not proof that asynchronous runtime interruption has settled.
Late or stale-generation results are retained without completing newer or cancelled work.
Resume inspects the same session before admitting a new continuation command.
Cancellation tombstones prevent re-admitting cancelled pending commands.
See [control semantics](OPERATIONS.md#pause-resume-and-cancel) for cascade and ancestor-hold behavior.

Permission replies remain the runtime's decisions and are never automatically approved.
A reply intent is persisted before delivery.
A lost reply remains uncertain rather than inferring approval from disappearance of the runtime's memory-resident request.

## Workspace and security limits

The worktree allocator is the single checkout-allocation authority; OpenCode's worktree allocator is not used alongside it.
Each modifying attempt gets an owned branch and separate checkout.
Worktrunk allocation is available for workspaces requiring that policy; its startup hooks and ignored-file copying are not automatically authorized.
Independent runtime sessions are created at assigned worktrees rather than using parentID, because OpenCode children inherit their parent's location.
Project and ancestor instructions are loaded separately from Tandem coordinator instructions.
Completed artifacts are retained by default.
Cleanup requires ownership, quiescence, and a clean checkout including ignored/untracked material, and never deletes the retained branch.

Worktrees and tool permissions are not OS security sandboxes.
This single-user MVP coordinates trusted local projects, not hostile arbitrary code or a multi-tenant SaaS workload.
Runtime/provider credentials, execution endpoints, and machine-local state must not reach a mobile browser.
Non-local exposure requires explicit configuration, authentication, and TLS through an appropriate trusted endpoint.
Additional harnesses, provisioning, distributed scheduling, billing, and synchronization are deferred.
