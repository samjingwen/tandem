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
Both `make test` (also used by `make check`) and `make test-race` use a shared, bounded 20-minute Go package timeout.
The complete race-instrumented suite includes multi-MiB receipt and API/CLI content traversals that can legitimately approach or exceed Go's default 10-minute aggregate package budget.
This package lifetime does not extend individual fixture, request, runtime, or regression deadlines, and Go still emits test/goroutine diagnostics on timeout.
CI invokes these same Makefile targets; targeted regression and opt-in native commands retain their separate deadlines.
No live model or shared native service is required by normal CI.
An absent opt-in fixture is reported skipped, never described as a live pass.

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
