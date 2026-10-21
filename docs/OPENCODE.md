# OpenCode adapter and thin plugin

Tandem coordinates persistent work; OpenCode owns model execution, tools, native sessions, and transcripts.
This integration supports **OpenCode 2.1.0 only**, verified against upstream revision [`60c68d145a14127ce633e26aa5fdfb882a7baa6d`](https://github.com/anomalyco/opencode/tree/60c68d145a14127ce633e26aa5fdfb882a7baa6d).
The current [V2 API](https://opencode.ai/v2/docs/api), [client](https://opencode.ai/v2/docs/build/client), and [plugin](https://opencode.ai/v2/docs/build/plugins) guides describe the architecture but can differ from this pinned release.
In particular, do not copy their generic Bearer-authentication or Promise-client permission-reply examples into this adapter's HTTP requests.

## Go integration

`internal/opencode.New(opencode.Options)` returns `(*opencode.Runtime, error)` implementing `core.Runtime` without non-stdlib adapter dependencies.
The available options are `URL`, `Password`, `ExpectedVersion`, optional `Model *opencode.Model`, and optional native `Agent`.
`Model` contains `ID`, `ProviderID`, and optional `Variant`.
`ExpectedVersion` defaults to `2.1.0`; changing it does not enable an unverified runtime version.
The task's descriptive role is not automatically interpreted as a native OpenCode agent name.

Supply the endpoint and password through an approved handoff or explicit configuration.
An empty `Password` uses the explicitly provided `OPENCODE_PASSWORD` environment variable.
The adapter never discovers services, reads authentication files, pairs automatically, or starts/restarts OpenCode.
Use HTTPS outside a trusted loopback connection.
URLs cannot contain embedded credentials, query parameters, or fragments.

Requests use HTTP Basic authentication with username `opencode` and the supplied password.
`Info` reads the direct `/api/info` response, reports exact compatibility, and derives a stable runtime ID from the normalized endpoint URL, never the password.
Every runtime operation checks the live version before using the pinned contract.
No mutation is automatically retried, and redirects are never followed.
Ordinary requests have a 30-second deadline; the event stream has no overall client timeout and ends when its caller cancels it.

### Session ownership and uncertain creation

`Reference(attemptID)` deterministically produces a valid `ses_` identifier from a SHA-256 digest.
Persist the attempt and reference before allocating a workspace or calling the runtime.
`Ensure` first gets the exact session reference.
Only HTTP 404 authoritatively permits creation with that same preallocated identifier.
Creation includes an explicit `location.directory` and `metadata.tandem` containing `instanceId`, `taskId`, and `attemptId`, with no native `parentID`.
OpenCode's duplicate-create behavior returns an existing session without comparing the new payload, so both existing and create responses are checked for reference, metadata ownership, and the exact directory.
No session is moved, relabeled, adopted, or overwritten to resolve a collision.

A lost create response returns `core.ErrUncertain`, even if the server created the session.
Recovery gets the same reference and verifies ownership rather than allocating a fresh identifier.
The adapter maintains no in-memory admission ledger, so this recovery also works after an adapter-process restart.

### Prompt admission, pause, and cancellation

`Prompt(ctx, spec, commandID, text)` derives a stable `msg_` ID and records the command ID alongside Tandem ownership metadata.
It looks for a projected message, then pending inbox admission, then rechecks projection to cover delivery between those reads.
Previously admitted text and ownership must match exactly.
Only a genuinely absent command is posted, using `delivery: "steer"` and `resume: true`.
The returned admission is checked because OpenCode's first successful same-ID admission wins even when a later caller supplies different text.
Prompt transforms that change canonical text or Tandem metadata therefore fail closed rather than silently weakening recovery checks.

**Never re-post a known admission as a wake retry.**
In 2.1.0 a same-ID prompt retry with `resume: true` can wake an already delivered or parked session.
After a lost response, matching inbox or message evidence establishes admission without waking it again.
The engine must serialize dispatch and preserve uncertain operations rather than dispatching concurrent speculative retries.
`ReconcilePrompt(ctx, spec, commandID, text)` is a read-only concrete-adapter extension for previously uncertain dispatches.
It returns success only for matching admission evidence and otherwise preserves `core.ErrUncertain` without posting, since an absent message/inbox item might still be in native preparation.
Use this recovery-only method after uncertainty; do not treat another call to the initial-dispatch `Prompt` method as a proof that reposting is safe.
The interface has no separate resume method; explicit continuation must be a new persisted command ID with explicit continuation text, not reuse of an old message ID.

`Interrupt` posts `/api/session/{id}/interrupt?resume=false` and checks the direct `{ "interrupted": boolean }` response.
Both an interrupted active drain and an idle no-op are valid acknowledgments.
Pause parks pending steers and queued prompts without deleting either.
Another explicit prompt admission can subsequently wake pending work.

`CancelPending(ctx, spec)` is an optional concrete-adapter extension, not a `core.Runtime` method.
Use it only after durable engine cancellation, not for pause.
It first interrupts with `resume=false`, then deletes only pending user items whose session, deterministic message ID, command ID, and Tandem ownership all match.
Foreign user input, native compaction controls, moves, and synthetic items are preserved.
Canceled command IDs must remain canceled in Tandem and must never be re-dispatched after their native inbox item disappears.
An engine using only `Interrupt` has parked work, not full pending-input cancellation.
Interrupting a native drain cannot undo an external side effect already performed by one of its tools.

### Observation and decisions

`Inspect` verifies ownership, reads `/api/session/active` as `{ "data": { "ses_...": { "type": "running" } } }`, and reads the session's pending permission and form lists.
A failed session outcome is reported only when its session is absent from the active map.
An inactive successful outcome is still reported as idle, never as task completion.
Structured result evidence and the engine's acceptance policy determine completion, not session idleness.

Permission replies use the native HTTP body:

```json
{ "decision": "once", "message": "Approved for this action" }
```

The allowed decisions are `once`, `always`, and `reject`; `reply` is not an HTTP body field in 2.1.0.
Form replies use typed keys rather than positional arrays:

```json
{ "answer": { "approved": true, "count": 2, "choices": ["one"] } }
```

`Reply` receives those complete native bodies as `json.RawMessage`, verifies the decision belongs to the current task and attempt, and checks that it is pending before posting.
A missing decision after an uncertain reply is not evidence of the same answer being applied and returns `core.ErrUncertain` without another write.
Permission approvals do not authorize publication, merging, or cleanup in Tandem.

### Events

`Events` consumes one live-only `/api/event` SSE subscription across all locations.
Each frame contains a JSON object decoded once, not double-encoded JSON.
The parser supports comments, CRLF, and multiline data, bounds line/frame sizes, and never dispatches incomplete frames.
Callbacks receive only a wake signal, never event payloads, session text, credentials, or foreign-location data.
The engine must provide a fast coalescing callback, reconnect/backoff after EOF or failure, and reconcile durable state after reconnecting.
There is no replay cursor or automatic reconnect in this adapter.

## Thin plugin

The package lives in `plugin/`, deliberately outside automatic `.opencode/plugins/` discovery.
It pins `@opencode/plugin` and `@opencode/client` to `2.1.0` with an npm-generated lockfile.
It uses `Plugin.define`, `ctx.tool.transform`, native execution cancellation, and `ctx.shell.hook("create.before", ...)`.
It does not start a scheduler, create native child sessions, or own session transcripts.

Build before configuring an explicitly approved OpenCode instance:

```sh
cd plugin
npm ci --ignore-scripts
npm run check
npm run build
```

The small `server.js` loader is intentional: the pinned local-directory loader resolves a server entrypoint rather than the package's compiled `dist` exports alone.
Load the package directory explicitly in the intended instance's configuration:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "plugins": ["/absolute/path/to/tandem/plugin"]
}
```

This is a configuration example, not an instruction to change or restart a personal shared service.
Provide `TANDEM_URL` and `TANDEM_TOKEN` to that instance through an approved environment handoff.
The plugin requires HTTPS for non-loopback Tandem endpoints and uses Tandem's Bearer token, not OpenCode's Basic-auth password.
Do not put either credential into Git or plugin configuration examples.
The shell hook clears the inherited `TANDEM_TOKEN` value before native non-interactive shell execution.
This reduces accidental credential exposure; it is not a sandbox or protection against other trusted plugins, arbitrary code execution, PTYs, or access to the service process.

The registered tools are `tandem_create`, `tandem_status`, `tandem_followup`, `tandem_decision`, and `tandem_result`.
Every mutation requires an explicit stable `operationId`, sent as `X-Operation-ID`.
On an uncertain transport or response outcome, preserve that ID and the exact payload rather than inventing another ID.
The client never retries mutations itself.

Tools use these service routes:

- `GET /v1/snapshot`
- `POST /v1/tasks`
- `POST /v1/tasks/{id}/followup`
- `POST /v1/tasks/{id}/results`
- `POST /v1/decisions/{id}/reply`

Worker task proposals are parented to their current Tandem task and constrained to its project; the service remains authoritative for project and parent validation.
Results derive task/attempt IDs from `context.sessionID`, the current snapshot's `attempts[].sessionRef`, and the task's current `attemptId`.
They additionally verify native session metadata against the service instance, task, and attempt.
Unknown sessions, stale attempts, duplicate mappings, and metadata collisions cannot submit another worker's result.
Result inputs do not select a target task or attempt.
The plugin exposes no accept, merge, publication, or workspace-cleanup tools.
It is a trusted service client, not a replacement for server authorization or human-review policy.

## Verification

The Go tests use `httptest` to cover exact envelopes, Basic auth, version rejection, ownership collisions, lost-response recovery across fresh adapter instances, prompt read/delivery races, decisions, interrupt/cancellation semantics, malformed responses, and bounded SSE parsing.
The plugin tests exercise service routes, operation IDs, token handling, proposal scoping, and result ownership checks.
Executable quality probes verify that representative formatter, ESLint, and TypeScript violations fail while conforming fixtures pass.

```sh
go test -race ./internal/opencode
go vet ./internal/opencode
cd plugin
npm run check
npm run build
npm audit
```

An opt-in native smoke test launches only the explicitly supplied binary with `serve --hostname 127.0.0.1 --port 0 --stdio`.
It uses a disposable working directory, sanitized HOME/XDG roots, disabled project/Claude discovery, disabled model fetching, no inherited model credentials, and an ephemeral password.
It does not discover, stop, restart, or mutate a shared service.
Without explicit opt-in it is skipped.

```sh
TANDEM_OPENCODE_SMOKE_BINARY=/absolute/path/to/2.1.0/opencode \
TANDEM_OPENCODE_SMOKE_PLUGIN=/absolute/path/to/tandem/plugin \
go test -v ./internal/opencode -run TestLiveIsolated210 -count=1
```

The smoke test checks exact runtime version, session creation/recovery, owned observation, parked prompt recovery without waking, interruption, owned pending-input cancellation, typed form replies, and SSE cancellation.
When the optional built plugin path is supplied, it also verifies real plugin activation and runs a native shell command proving the inherited Tandem token was cleared.
The subprocess exits when its private stdin closes; only that owned subprocess is force-terminated if graceful shutdown fails.
Live model execution is explicitly skipped because the isolated environment intentionally has no model credentials.
No model-provider success, external coding-work acceptance, personal-service compatibility beyond the exact pin, or publication authority is claimed by this smoke test.
