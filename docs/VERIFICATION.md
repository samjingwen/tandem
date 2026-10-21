# Verification contract

Deterministic integration tests use the real engine, SQLite, HTTP API, CLI parsing/client, and disposable Git worktrees.
Only execution is simulated where a controlled fake runtime is named.
Fake-runtime success is not evidence of model/provider availability.

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

Additional tests cover instruction-generation staleness, ancestor dependency cycles, finite write/model-notification budgets, read-only uncertain-prompt recovery, preserved native permission choices, explicit shared-policy delivery, and quiet persistence when observations do not change.
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

```sh
TANDEM_LIVE_CONNECTION_DIR=/absolute/path/to/connection \
TANDEM_LIVE_BINARY=/absolute/path/to/tandem/bin/tandem \
go test -v ./internal/integration -run '^TestLiveDaemonProcessOwnershipAndClientLifetime$' -count=1
```

The separate delegation evaluation spends configured model capacity and requires a fresh, nonexistent output directory.
It submits conversational intake through the actual standalone CLI process, lets that process exit, and uses another client to inspect and continue the same task/coordinator session.
A real coordinator dispatches two bounded modifying workers through Tandem tools into different owned worktrees.
The harness performs a narrow safety preflight on generated arithmetic-only demo artifacts and runs independent Go test oracles without injected control/provider environment variables or module-network access.
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

## External application boundary

Tenaxe's integration is delivered separately from this public engine repository.
Its controlled acceptance uses the real Go API and SQLite with a deliberately simulated execution runtime, a minimal Spring context without native bridge beans, and browser reconnect checks.
That proves the request/projection/reconnect contract, not native-model execution through the full application, remote authentication, production deployment, or real-phone behavior.
Private application source, runtime data, credentials, and machine-local acceptance handoffs do not belong in this repository.
