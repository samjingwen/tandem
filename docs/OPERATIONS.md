# Operating a Tandem instance

## Identity and data

An instance is a particular canonical local state directory and its SQLite database.
Its stable instance ID survives daemon restarts.
Client URLs select a running instance; two installations do not synchronize their stores.
Never open the same state directory from different hosts or network filesystems.

The daemon holds an OS file lock for its lifetime.
A second service process fails rather than becoming another scheduler.
Do not delete the lock file to force takeover; stop a verified owner through its normal process supervisor or investigate the concrete blocker.
Process death releases OS locking; an old file alone is not a live lock.

State includes `tandem.db`, its WAL/SHM files, append-only history, private client/runtime configuration, and retained worker checkouts.
Keep the entire directory private and outside source control.
For a consistent backup, stop the verified daemon or use SQLite's supported backup facility.
Do not copy only a live database file while ignoring its WAL.
Restore into a separate directory and inspect runtime/workspace ownership before dispatching work.
Never run a restored/copied store concurrently with its original, because the copy contains the original runtime bindings and is not a distributed failover replica.

## Independent daemon lifetime

Task clients never start their own scheduler and their disconnection does not interrupt service-owned work.
Run `tandem serve` separately from the CLI, under launchd or systemd for normal unattended operation.
The provided launchd example is a template, not an installed service.
Replace its absolute paths and choose logs outside the checkout before installing it through the OS-supported interface.
Do not install a second service pointed at the same store.

Stopping Tandem releases the scheduling owner but does not automatically cancel OpenCode workers.
The next owner reconciles the same session references and worktrees.
Stopping OpenCode itself is a separate, explicit runtime operation and may terminate execution.
Never restart a shared runtime merely to repair one task.
The initial adapter reconnects to its configured endpoint and does not automatically retarget credentials or unfinished bindings to another URL.
Use a stable explicitly provisioned runtime endpoint for unattended operation; an endpoint/binding change requires operator reconciliation rather than speculative worker replacement.

## Runtime pairing

`runtime-connect` checks for a running OpenCode 2.1.0 service, requests a one-time pairing grant through its native CLI, and redeems it over its documented loopback HTTP interface.
The resulting grant lives in owner-only `opencode-connection.json`.
The command refuses to overwrite an existing connection.
It does not request upgrades/restarts or copy provider secrets.
OpenCode's native `api` command can still perform its normal autostart discovery if the service disappears between preflight and the request.
If that race is unacceptable, supply an explicitly provisioned endpoint/grant through the named environment variables instead.
Pairing grants have the runtime's lifetime/revocation rules; authentication failures are reported as failures, not automatically diagnosed as expired credentials.

## Recovery

Startup and reconnect inspect stored attempts, assigned worktrees, exact runtime references, pending forms/permissions, and desired states before eligible dispatch.
OpenCode notifications are live-only and may be lost; periodic inspection is the backstop.
The engine never asks a model repeatedly whether work has finished.
It wakes a coordinator only for newly recorded child outcomes or questions, using persisted signal IDs so restart does not repeat the same notification.
The default automatic-notification budget is 32 per coordinator attempt; after that, inspect the task's warning and use explicit follow-ups rather than an unbounded model loop.
External dispatch writes have a finite automatic retry budget; uncertainty does not become a speculative new worker.
Automatic mutation retries stop after eight recorded attempt failures, including runtime-availability or inspection failures as well as failed writes.
Read-only reconciliation and local pause/cancel settlement remain available after that budget is exhausted.
An explicit resume reconciles the existing binding before admitting a continuation.
Pending prompt/reply delivery retains an exhausted-budget diagnostic until explicit recovery or successful delivery.
If cancellation exhausts its budget, repeat cancel with a new operation ID to request another bounded cancellation attempt; cancelled tasks cannot resume.
Missing sessions, conflicting metadata, changed allocator/runtime bindings, or missing/unowned worktrees remain recovering until their ownership is resolved.
Do not manually rewrite runtime references or database rows to make diagnostics green.

## Pause, resume, and cancel

Controls commit a barrier serialized with external admission.
An already admitted/in-flight request can have produced work before that barrier takes effect.
The engine accounts for it, interrupts applicable execution, and confirms quiescence before reporting paused/cancelled.
An owned idle or failed native observation can settle a control; a historical runtime failure need not become idle and is never completion evidence.
If interruption cannot be confirmed, the task remains recovering with its requested desired state.

Without cascade, a paused or cancelled ancestor blocks first execution of unstarted descendants but does not interrupt descendants that have already started.
An explicitly resumed, already-started descendant continues its existing attempt even while that ancestor remains held.
Use `--cascade` to apply the control to the task tree.
Resume never silently creates a replacement attempt.
A conclusively failed attempt requires the explicit retry control to create another attempt; its prior history and worktree remain retained.
Cancelled/completed tasks are terminal rather than silently reopened by follow-up text.
Late results remain recorded and cannot override cancellation or a newer instruction generation.

## Questions and permissions

Read pending decisions from the task detail or decisions endpoint, including from another authorized client on the same instance.
Replies are checked against the task's current attempt and request identity.
Permission replies use the runtime's actual once/always/reject semantics; Tandem does not auto-approve a request.
An always-allow decision can affect runtime policy, so expose it deliberately rather than treating it as an ordinary text answer.
Native forms/permissions may be memory-resident and can disappear after runtime restart.
Disappearance does not prove approval.
A lost reply outcome stays uncertain and is not automatically sent again or labeled answered.

## Results and cleanup

Results must name their attempt and instruction generation and include explicit evidence references.
The reviewer must inspect those references; a reported passed flag is an assertion, not proof produced by the transport.
Explicit acceptance requires matching current-generation evidence, no unresolved decisions/dispatch, completed subtasks, and runtime-confirmed quiescence.
It does not merge or publish a branch.

Completed work is retained by default.
Cleanup is an explicit durable request and requires terminal task ownership and runtime quiescence.
The returned operation receipt acknowledges the request; inspect the attempt's cleanup phase to confirm completion.
Removal checks actual repository/branch ownership and all tracked, untracked, and ignored material.
Dirty or unowned checkouts are refused, not reset, stashed, or forcibly removed.
The branch is retained even after a clean checkout is removed.
If removal succeeds before its record commits, recovery can reconcile the recorded pending cleanup with the directory's absence.
Task/run history remains intact.

### Workspace adapter boundary

The Rust `tandem-workspace` library follows the same recorded Git/Worktrunk authority as the Go executable; it does not switch the executable or adopt retained state.
Registration resolves the canonical repository root, requires an explicit commit-resolving base reference, and leaves the canonical checkout unchanged, including dirty and ignored files.
Policy text keeps inline instructions, root-to-project `AGENTS.md` order and explicitly requested Markdown files in caller order, including duplicates, within the 256-KiB total and 16-explicit-file limits.
Environment-file names and Markdown aliases resolving to those names are refused.
Registration canonicalizes repository and permitted Markdown aliases before access; subsequent Unix descriptor-relative traversal rejects symlink directory components and opens policy files as bounded, regular, no-follow descriptors.
New allocation parents are private; existing directories and worker commits are not reset or permission-rewritten.

Git and Worktrunk commands receive only the basic environment whitelist in [`process::run`](../crates/tandem-workspace/src/process.rs), with Git hooks and fsmonitor disabled and no inherited control/provider credentials.
Worktrunk also receives `--no-hooks`; registration never authorizes startup hooks or ignored-file copying.
Each command has a 30-second maximum deadline and independent 1-MiB stdout/stderr bounds; public errors do not contain command diagnostics.
Cooperative cancellation stops the isolated command group created by this adapter and joins its child before returning, retaining any already-created checkout for read-only reconciliation.
It does not signal runtime workers or cleanup holders.
The engine retains operation ownership when an API caller drops its wait; callers must not abandon an adapter future as a substitute for that lifecycle contract.

Cleanup checks tracked, untracked and ignored material, then inventories same-user process cwd ownership through Linux `/proc` metadata or bounded five-second macOS `lsof` output.
Unreadable or ambiguous ownership fails closed; unsupported platforms refuse cleanup.
On Linux, a missing cwd alone does not prove process exit, including when the main thread has exited but other threads remain alive.
On macOS, ambiguous `lsof` caret encodings, including literal caret text indistinguishable from encoded controls, refuse cleanup even when the reported cwd appears unrelated to the workspace.
No process is stopped to make removal succeed, and no branch is deleted.
These checks complement runtime quiescence, not a security sandbox or protection against a concurrent same-user process changing cwd or filesystem bindings after inspection.

## Exposure and authority

The default listener is loopback and uses a service token.
Non-loopback listening requires `--allow-remote`, an explicitly configured strong token, and TLS or an asserted trusted TLS proxy boundary.
Do not expose this privileged single-user execution API directly to an untrusted network.
Tenaxe must keep Tandem access and project authorization in its backend.
The engine is not a multi-tenant authentication system or sandbox.

Worktree allocation suppresses implicit project hooks and does not copy ignored/private files.
Git/Worktrunk subprocess environments omit Tandem and runtime control credentials.
Workers still run with their runtime user's OS authority; configured permissions and instructions are narrower controls, not non-bypassable OS isolation.
Do not use this MVP as a hostile-code hosting service.
