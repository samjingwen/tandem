# Verification contract

Deterministic integration tests use the real engine, SQLite, HTTP API, CLI parsing/client, and disposable Git worktrees.
Only execution is simulated where a controlled fake runtime is named.
Fake-runtime success is not evidence of model/provider availability.
Previously recorded native/model evaluations predate the dispatch, capacity, cleanup, graph, and generation corrections and do not verify those revised paths.
They also predate the isolated daemon-store and exact Sum/Product aggregate-oracle corrections; neither changed live evaluation has been rerun as part of review.
They also predate the bounded read and plugin pagination changes and do not verify that revised live path.
The subsequent admission-evidence retention, failed-session control settlement, and paused-child continuation corrections likewise have no new native/model acceptance.
Native/model revalidation remains opt-in using the commands below; review-phase regression checks use disposable fake-runtime and HTTP fixtures only.

## Required acceptance cases

| Case | Executable evidence |
| --- | --- |
| Concurrent duplicate submission | `TestConcurrentDuplicateSubmission`; `TestIndependentClientsDuplicateCreateDisconnectAndRestart` |
| Restart preserves session/worktree | `TestRestartReattachesExistingWorker`; independent-client HTTP restart test |
| Session-create crash window | `TestCrashAfterSessionCreationRecoversExactReference`; adapter lost-response tests |
| Dropped events converge | `TestDroppedEventsConvergeWithoutReplay` |
| One scheduler per store | `TestSecondServiceProcessCannotOwnState`; `TestOwnerCrashReleasesLockAndPreservesCommittedState`; opt-in actual-daemon test |
| Separate owned modifying checkouts | `TestDifferentOwnedWorktreesAndBoundedConcurrency`; `TestWorktrunkAllocationAndRecordedAuthority` |
| Pause/cancel races and late work | `TestCancelRacesWithInflightCreationAndRetainsLateResult`; `TestCancelRemainsUncertainUntilLateAdmissionIsReconciled` |
| Resume/retry reconciles first | `TestPauseBeforeDispatchAndResumeReconcile`; `TestRetryCannotForkAnAlreadyResumedFailedAttempt` |
| Idle is not completion | `TestIdleIsNotCompletionAndAcceptanceNeedsEvidence` |
| Another client answers a decision | `TestPendingDecisionIsDurableAndOwnershipChecked`; `TestAnotherClientAnswersDecisionAndAcceptsCurrentGeneration` |
| Disconnect does not stop work | Independent-client HTTP test; opt-in actual-daemon and model-backed delegation tests |
| Dirty/unowned/live-process cleanup refused | `TestDirtyAndUnownedWorktreesRefuseCleanup`; `TestCleanupDoesNotRemoveWorkspaceWithLiveProcess`; `TestTaskCleanupResolvesOwnedAttempt` |
| Real supported OpenCode contract | Opt-in `TestLiveIsolated210`; separate model-backed evaluation |
| CLI/API and external clients share contract | `internal/integration` HTTP/CLI tests; OpenAPI consumer validation; external consumer acceptance |
| Large inventory and complete content traversal | `TestLargeInventoryCLIReads`; `TestAccumulatedContentAndExactMutationReceipts`; `TestReadCursorBindingAndRecovery` |

Additional tests cover instruction-generation staleness, ancestor dependency cycles, finite write/model-notification budgets, read-only uncertain-prompt recovery, preserved native permission choices, explicit shared-policy delivery, and quiet persistence when observations do not change.
`TestEffectsBookkeepingAfterExpiry`, `TestExpiredExternalFailuresBoundMutations`, and `TestSuccessfulDispatchAfterExpiry` inject cancelled/deadline-expired external calls against disposable SQLite/Git fixtures and check durable outcomes and bounded writes.
`TestDispatchBudgetSurvivesIdleAndResume` exercises API/CLI prompt/reply exhaustion, stable idle diagnostics, and same-attempt recovery through explicit resume.
`TestPauseSettlesAfterInterruptBudget` covers individual and cascade read-only pause settlement after eight failed interrupts, including parked continuations.
`TestCancellationPreservesUnreconciledInbox` checks that a lost admission response and transient read failure cannot cause cancellation to delete its only admission evidence.
`TestFailedNativeControlSettlement` and `TestControlRequiresOwnedQuiescentReadback` cover failed-but-quiescent control settlement without accepting missing, foreign, or active sessions.
`TestPausedAdmissionRetainsStartedChild` covers read-only admission recovery while paused, then explicit continuation under a held ancestor without a duplicate initial prompt.
`TestDaemonFixtureLeavesPopulatedSourceUntouched` runs production daemon processes against a fresh store with a fake HTTP runtime, checking that supplied populated state remains byte-for-byte unchanged and no task/session operation occurs.
`TestArithmeticWorkloadCoverage` rejects missing, duplicate, and ambiguous Sum/Product coverage, while `TestArithmeticBehavioralOracles` executes independent Go checks against correct and deliberately incorrect arithmetic fixtures without models.
The bounded-read regression first reproduces CLI failure after admitting 65 actual valid 128-KiB Unicode tasks through the API, without invoking the fake runtime's mutation methods.
It then traverses all task content through byte-budgeted API pages and checks exact admitted IDs and text, not only a lean count or source shape.
The accumulated-record fixture seeds a valid retained single-task history above 8 MiB, continues it through the real CLI, verifies exact replay receipts after a later followup, and checks complete task/command/decision/result/evidence content.
Cursor checks cover unchanged-revision continuation, scope tampering, malformed and restarted cursors, mutation invalidation, and recovery from a new manifest.
These disposable fake-runtime/API/CLI fixtures are not evidence of new native/model/cloud acceptance or compatibility validation by a separately owned integration.
Formatter, analyzer, and compiler enforcement probes execute their real consumers against conforming and violating disposable fixtures.
They are Lint-phase proof rather than string searches over configuration files.

## Routine deterministic checks

```sh
make prepare
make check
make test-race
make prove-quality
make security
make build
```

Maintained Go package patterns explicitly exclude upstream Go sources shipped inside npm dependencies.
The full Go suite in both `make test` (also used by `make check`) and `make test-race` uses a shared, bounded 20-minute Go package timeout.
The complete race-instrumented suite includes multi-MiB receipt and API/CLI content traversals that can legitimately approach or exceed Go's default 10-minute aggregate package budget.
This package lifetime does not extend individual fixture, request, runtime, or regression deadlines, and Go still emits test/goroutine diagnostics on timeout.
CI invokes these same Makefile targets; targeted regression and opt-in native commands retain their separate deadlines.
No live model or shared native service is required by normal CI.
An absent opt-in fixture is reported skipped, never described as a live pass.
`make test` also runs the Rust workspace tests and `make rust-parity`; the latter invokes a separate Go storage/engine-oracle test run with Go's default package timeout and explicit Rust fixture executables.
These checks cover [Rust domain/SQLite, coordination, controlled runtime and disposable workspace-adapter libraries](RUST_MIGRATION.md#verification-commands), not native OpenCode, retained-workspace, HTTP-service or CLI acceptance.
`crates/tandem-engine/tests/coordination.rs` executes real Rust admission/reconciliation against SQLite and controlled runtime/workspace ports, including abandoned-client, shutdown, uncertainty, same-reference recovery, generation, role/dependency bounds, capacity, finite notifications, quiet polls, explicit decisions and cleanup barriers.
Reply regressions distinguish explicitly proven rejection from an unmarked `invalid_request`, which must retain uncertainty and prevent resend.
The five-second bookkeeping fixture blocks only its owned storage callback, verifies that controls cannot cross the effects barrier, then releases it and rejects the expired bookkeeping change without fabricating an admission receipt.
`crates/tandem-engine/tests/support/bookkeeping.rs` also checks that shutdown, explicit cancellation and deadlines stop event subscriptions and reconnection before blocked bookkeeping releases ownership.
`TestRustEngineGoFingerprintOracle`, `TestRustEngineReplaysActualGoOperationsAndGoReplaysRustCreate`, and `TestRustEngineGoDecisionAndCoordinatorSignalOracle` use actual Go serialization/service behavior as an independent oracle, not Rust-generated expected values.
They cover request field order/omission, HTML and U+2028/U+2029 escaping, raw-answer compaction/order/number spelling, all eight command kinds' old receipts, operation conflicts, decision identifiers and coordinator signal bodies, including Go replay of an actual Rust-created task/receipt.
Timestamp parity includes malformed-date refusal and accepted legacy Go spellings while retaining their original text and comparing actual instants.
The separate `tools/test-engine-safety.py` driver exercises the actual Rust Service, scheduler, lifecycle and SQLite through the disposable JSON-lines `engine-safety` consumer.
It independently asserts durable rows, pre-effect ownership, uncertainty without resend or premature cleanup, generations, task/depth/role limits, finite delivery/notification budgets and blocked-bookkeeping cancellation.
Only runtime/workspace execution is simulated; this is not concrete native adapter, model/provider, external consumer or HTTP/CLI acceptance.
The driver can retain requests/responses, SQLite operation rows, effect/event observations and source/binary hashes with `--evidence PATH`; fixture data remains disposable.
`make rust-runtime-parity` builds the separate `runtime-compat` executable and independently drives both concrete HTTP adapters against the same controlled request/response cases, including exact create policy/assignment and configured native model, lost-response recovery across fresh adapter instances, no speculative prompt resend, reply rejection proof and stale decision refusal, selective inbox cancellation, foreground/background quiescence, redirects, read limits and live-only SSE cancellation/bounds.
`TestRustOpenCodeGoEndpointIdentityOracle` obtains the expected persisted runtime identity from the executable Go adapter, including escaped and Unicode endpoint paths; it does not substitute a second Rust implementation for the oracle.
`TestRustOpenCodeGoAuthorityIdentityOracle` checks the shared authority cases against actual Go constructor/Info behavior; Rust's `go_authority_identity_oracle` checks its constructor/Info against those same cases, including Unicode and percent-escaped hosts and explicit ports.
The authority tests redirect only their test-local transport to loopback fixtures after construction, preserving the configured identity without contacting public hostnames.
The separate Rust executable receives only explicitly supplied fixture configuration, runs with a fresh HOME and no inherited control/provider credentials, and never starts a native runtime.
Ordinary Go-only test invocations report this consumer skipped when not supplied; `make rust-parity` and CI supply it explicitly, and the opt-in runtime parity invocation enables the Go race detector.
These fixtures do not start native OpenCode or establish plugin/provider compatibility or retained-state cutover acceptance.

### Workspace adapter checks

`make rust-workspace-parity`, also invoked by `make rust-parity` and aggregate CI, builds the actual Rust `workspace-compat` consumer and drives it alongside the executable Go reference against real disposable Git and Worktrunk repositories.
`make prepare` and this focused target provide Worktrunk 0.81.0 through project-local, SHA-256-verified official macOS or static Linux-musl binaries rather than requiring a newer Rust compiler to build that external CLI.
Preparation rechecks the archive and replaces the local binary from verified bytes even when a cached executable reports the expected version.
Offline installer tests execute a conforming fixture binary and reject checksum corruption, nonregular archive members, symlink destinations and unexpected versions; explicit Ruff ownership and real negative enforcement probes cover both maintained Python tools.
There is no global installation, automatic runtime discovery or private credential/configuration read.

Workspace oracles compare registration text/order, malformed policy UTF-8 as actually JSON-encoded by Go, stable pure plans, both allocators, same-repository/branch ownership, current-HEAD recovery and branch-preserving cleanup.
They refuse invalid policy, foreign bindings, symlinks, tracked/untracked/ignored dirt and live cwd holders without stopping those holders.
Allocator tests disable actual Git/Worktrunk hooks and include a deliberate disposable-hook positive control.
The ambiguity fixture executes the real allocator, injects only a failed post-execution command result, then verifies that read-only recovery performs no second allocation.
Credential tests use synthetic control/provider values and a Git wrapper that rejects inherited settings.
Rust command-consumer tests exercise cancellation/deadlines, bounded stdout/stderr, private diagnostic suppression, joined command groups and retained partial effects.
`crates/tandem-workspace/tests/coordination.rs` composes the real Rust engine, SQLite and Git adapter, asserting durable ownership before allocation/runtime effects and cleanup, exact cleanup receipt replay and retained branches; only runtime execution is simulated.
Go-only test invocations without the explicit Rust consumer report these parity tests skipped; the focused/aggregate opted-in run supplies it and requires actual Worktrunk rather than skipping a missing allocator.
No retained checkout, native OpenCode/provider session or default-binary cutover is part of this evidence.

## Isolated native contract

Use the exact supported OpenCode 2.1.0 executable and built plugin.
This test creates fresh HOME/XDG directories and an owned stdio-leased server, without inheriting provider credentials or modifying a shared service.
It exercises real create/inspect, parked prompt admission/recovery, interrupt/cancellation, forms, SSE, plugin activation, and native permission evaluations.
Its model-execution subtest is intentionally skipped.

```sh
TANDEM_OPENCODE_SMOKE_BINARY=/absolute/path/to/2.1.0/opencode \
TANDEM_OPENCODE_SMOKE_PLUGIN=/absolute/path/to/tandem/plugin \
go test -v ./internal/opencode -run '^TestLiveIsolated210$' -count=1
```

## Actual daemon and model-backed evaluation

First provision a Tandem-owned runtime connection using the documented setup operation.
Do not copy or inspect provider authentication caches.
The daemon-process test starts no tasks/models, verifies readiness through independent clients, and refuses a second actual service process against the same store.
Both processes use one fresh owner-only disposable state directory containing only the supplied explicit connection configuration, never the supplied store or its worktrees.

```sh
TANDEM_LIVE_CONNECTION_DIR=/absolute/path/to/connection \
TANDEM_LIVE_BINARY=/absolute/path/to/tandem/bin/tandem \
go test -v ./internal/integration -run '^TestLiveDaemonProcessOwnershipAndClientLifetime$' -count=1
```

The separate delegation evaluation spends configured model capacity and requires a fresh, nonexistent output directory.
Set optional `TANDEM_LIVE_MODEL=provider/model#variant` to select an already-configured eligible model instead of the native default.
It submits conversational intake through the actual standalone CLI process, lets that process exit, and uses another client to inspect and continue the same task/coordinator session.
A real coordinator dispatches two bounded modifying workers through Tandem tools into different owned worktrees.
The harness performs a narrow safety preflight on generated arithmetic-only demo artifacts and runs independent Go test oracles without injected control/provider environment variables or module-network access.
It requires exactly one Sum worker and one Product worker, each with its requested implementation/test artifacts, before running the matching arithmetic oracle and rechecking the same worker coverage after continuation.
This preflight is not an OS sandbox or a general hostile-code validator.
All records, code, and verification logs are retained for inspection; owned runtime sessions are interrupted during teardown, never globally stopped.

```sh
TANDEM_LIVE_CONNECTION_DIR=/absolute/path/to/connection \
TANDEM_LIVE_BINARY=/absolute/path/to/tandem/bin/tandem \
TANDEM_LIVE_PLUGIN=/absolute/path/to/tandem/plugin \
TANDEM_LIVE_DEMO_DIR=/absolute/path/to/fresh-retained-demo \
go test -v ./internal/integration -run '^TestLiveCoordinatorDelegationAcceptance$' -count=1 -timeout=8m
```

Missing/failed model access, pending permission requests, timeout, or incomplete evidence is not a passing evaluation.
The test never approves permission requests merely to obtain a green result.
For deliberate operator-reviewed execution, set `TANDEM_LIVE_MANUAL_DECISIONS=1` and use the owner-only `client-handoff.json` metadata to connect a separate CLI while the evaluation waits.
Inspect the actual request and generated fixture code before making an explicit once/reject decision; the harness itself never answers it.

## External application boundary

Tenaxe's integration is delivered separately from this public engine repository.
Its controlled acceptance uses the real Go API and SQLite with a deliberately simulated execution runtime, a minimal Spring context without native bridge beans, and browser reconnect checks.
That earlier acceptance predates the additive bounded-read contract in [API.md](API.md#bounded-reads-and-compatibility); compatibility with that contract requires validation by the separate consumer owner.
It does not prove native-model execution through the full application, remote authentication, production deployment, or real-phone behavior.
Private application source, runtime data, credentials, and machine-local acceptance handoffs do not belong in this repository.
