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
The task's `worker` or `coordinator` role selects its managed capability profile, not a native OpenCode agent name.

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
Creation includes an explicit `location.directory` and `metadata.tandem` containing `instanceId`, `taskId`, `attemptId`, the immutable assigned `directory`, and the logical `role`, with no native `parentID`.
An empty role is normalized to `worker`, and prompt admissions carry the same directory/role binding alongside their command ID.
OpenCode's duplicate-create behavior returns an existing session without comparing the new payload, so both existing and create responses are checked for reference, metadata ownership, and the exact directory.
No session is moved, relabeled, adopted, or overwritten to resolve a collision.
The exact pinned session permission list is also checked on every ownership verification.
An older owned session without this policy, or a session whose rules were edited externally, fails closed instead of being silently rewritten.
Missing or changed assignment metadata, a role collision, or a native move to another directory likewise fails closed without rewriting or adopting the session.

A lost create response returns `core.ErrUncertain`, even if the server created the session.
Recovery gets the same reference and verifies ownership rather than allocating a fresh identifier.
The adapter maintains no in-memory admission ledger, so this recovery also works after an adapter-process restart.

### Managed native capabilities

Session creation supplies an explicit deny-by-default `permissions` list.
Workers can read and edit ordinary paths inside their assigned Location; coordinators can read but cannot use native edit/write/patch tools.
Absolute external paths, `..` paths into other checkouts, `external_directory`, unmanaged `subagent` delegation, and unspecified actions are denied.
Private `.env` files are excluded; `.env.example` remains available inside the workspace.
Only the required Tandem tool names, Code Mode, native file operations, filename search, planning questions, and interactive shell/content-search checks are enabled.
There are no granted MCP, provider-management, native session-management, or worktree-administration capabilities.

These names/resources were checked against pinned [`file-access.ts`](https://github.com/anomalyco/opencode/blob/60c68d145a14127ce633e26aa5fdfb882a7baa6d/packages/core/src/file-access.ts), [`permission.ts`](https://github.com/anomalyco/opencode/blob/60c68d145a14127ce633e26aa5fdfb882a7baa6d/packages/core/src/permission.ts), and the built-in tool sources, not inferred from newer documentation.
Internal read/edit resources are Location-relative, including `../` resources for neighboring paths inside the same native project.
Shell resources are scanner-produced commands, while grep/glob resources are search patterns rather than search paths.

**Load and verify this thin plugin at the assigned Location before dispatching managed model work.**
Session rules alone do not close every 2.1.0 permission gap.
Native shell and grep profiles initially ask, and the native user's real saved `always` choices remain authoritative.
The plugin does not force another shell/grep ask after the user saved an approval and never relaxes an existing native `deny` into an ask.
Grep initially asks because it authorizes a regular expression rather than each returned file; filename globbing does not read matching contents.
Content-search permissions are not an OS sandbox or a per-file confidentiality boundary.
The plugin validates canonical paths, including symlinks and search roots, before native access and checks pending new-file parents.
Guards anchor to immutable assignment metadata, not the current native location, and reject a moved or malformed binding before execution.
They use native session data rather than fetching Tandem state for each file operation, so valid local work can continue during a coordination-service outage.
It blocks model mutation of `.opencode`, native configuration documents, `.git` administration, and the loaded plugin package.
Its native tool-entrypoint allowlist also blocks worktree removal, which the pinned runtime advertises under the broader `edit` permission instead of its own tool name.
The parent bootstrap owns installing/loading the approved package; this adapter does not modify a runtime's configuration automatically.

Worktrees, these rules, and these hooks are **not OS security sandboxes**.
Approved shell commands still execute with the host user's filesystem, process, and network authority.
Other trusted plugins and concurrent filesystem changes are outside this model-level boundary; use a separately authorized OS sandbox when stronger containment is required.

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
Saved request details must still match the native pending request, so a reused ID with different contents cannot receive a stale answer.
A missing decision after an uncertain reply is not evidence of the same answer being applied and returns `core.ErrUncertain` without another write.
Native HTTP 400 write rejection is mapped to `core.Invalid`, allowing invalid form answers to be corrected instead of being stuck as uncertain sends.
`KnownRejected(err)` additionally classifies deterministic validation/authentication rejection without exposing native response bodies.
Timeouts, missing or already-settled replies, and ambiguous server outcomes remain uncertain and never count as known rejection.
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
It uses `Plugin.define`, `ctx.tool.transform`, the pinned native permission assertion/evaluation hooks, native execution cancellation, and `ctx.shell.hook("create.before", ...)`.
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
  "plugins": [
    {
      "package": "/absolute/path/to/tandem/plugin",
      "options": {
        "url": "http://127.0.0.1:7777",
        "tokenFile": "/absolute/private/runtime/path/tandem-token"
      }
    }
  ]
}
```

This is a configuration example, not an instruction to change or restart a personal shared service.
Optional `ctx.options.url`, `ctx.options.tokenFile`, and `ctx.options.coordinatorDirectory` support explicit location configuration on an already-running runtime without changing its global service environment or restarting it.
Do not confuse configuration scope with the pinned `/api/location/reload` endpoint: that endpoint rebuilds every loaded location and cancels pending permissions/forms.
It must not be used as an allegedly targeted reload on a shared runtime without explicit approval for those broader effects.
An omitted URL uses `TANDEM_URL`; an omitted token-file option uses `TANDEM_TOKEN`.
An explicitly supplied but unsafe/unreadable token file fails setup instead of falling back to an environment token.
No auth file or cache is discovered automatically.

The default is **managed-only**, not implicit global coordinator access for every unbound native session.
For a trusted development hub, explicitly set `coordinatorDirectory` to that hub's canonical absolute existing directory, such as the canonical path corresponding to the user's `~/code/dev`.
Only an unbound session whose actual native location equals that exact directory receives global hub authority.
Ancestor configuration inheritance does not grant those rights to descendant project locations, and symlink aliases, relative paths, missing directories, and prefix matching are not accepted.
An unbound session outside the configured hub is rejected from Tandem tools before the plugin fetches the global snapshot.
Managed or malformed/stale/foreign metadata never falls back to hub authority, even at the hub location.
Omit this option when no global conversational hub is desired.

The token file is plain token text with an optional trailing newline, at an explicit absolute path outside the plugin Location and managed workspace.
It must be a readable regular file owned by the current POSIX user, with no group/other permissions and only one hardlink.
`0600` and `0400` are suitable modes.
Symlink files and symlink parent aliases are rejected; the loader uses no-follow/nonblocking open, checks the opened inode/owner/mode, and bounds reads to 16 KiB.
Keep the credential and runtime state outside Git, and create them only through the approved bootstrap or credential handoff.
Tests load only owned temporary fixtures, never real private token files.
The plugin requires HTTPS for non-loopback Tandem endpoints and uses Tandem's Bearer token, not OpenCode's Basic-auth password.
Do not put either credential into Git or plugin configuration examples.
The shell hook clears the inherited `TANDEM_TOKEN` value before native non-interactive shell execution.
This reduces accidental credential exposure; it is not a sandbox or protection against other trusted plugins, arbitrary code execution, PTYs, or access to the service process.

The registered tools are `tandem_create`, `tandem_status`, `tandem_followup`, `tandem_decision`, `tandem_result`, and `tandem_control`.
Every mutation requires an explicit stable `operationId`, sent as `X-Operation-ID`, matching `^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$` with a maximum length of 128 characters.
On an uncertain transport or response outcome, preserve that ID and the exact payload rather than inventing another ID.
The client never retries mutations itself.

Tools use these service routes:

- `GET /v1/snapshot`
- `POST /v1/tasks`
- `POST /v1/tasks/{id}/followup`
- `POST /v1/tasks/{id}/control`
- `POST /v1/tasks/{id}/results`
- `POST /v1/decisions/{id}/reply`

Managed task proposals are parented to their current Tandem task and constrained to its project, including dependencies.
Ordinary workers cannot propose a coordinator role as a privilege escalation.
The service remains authoritative for project, parent, role, and dependency validation.
Managed status responses contain only the owned project's projects/tasks/attempts/commands/decisions/results, preserving composite attempt/generation result keys while dropping unknown global extensions.
Only an explicitly opted-in unbound hub may read the complete snapshot or create unparented root tasks.
Missing, stale, or conflicting managed native metadata never falls back to unbound coordinator authority.
API authority verifies the native binding before fetching state, then checks its assigned directory against persisted `attempt.workspace.path` and its logical role against the task role.
Ordinary workers can follow up only their own task and cannot answer any decisions, including their own planning form/question.
Managed coordinators can route followups and control tasks only within their owned project, and can answer descendant task forms but never their own user questions.
The explicitly opted-in hub can route decisions and controls across projects; other unbound sessions cannot use these tools.
Controls send a body such as `{ "action": "pause", "cascade": false }` to the task control route, with an explicit action, cascade boolean, and stable operation ID.
The allowed actions are `pause`, `resume`, `cancel`, and `retry`.
Ordinary workers cannot pause, resume, cancel, or retry tasks through the plugin.

**The model cannot approve native permissions merely because the plugin has a service token.**
Ordinary managed workers cannot answer their own or another worker's permission requests, including rejection replies.
For every coordinator permission-kind reply, the plugin first awaits `ctx.permission.assert` with action `tandem.permission.reply`, original request details, the exact cloned answer, operation ID, and native tool-call source metadata.
For this separate permission-forwarding action, the evaluation hook requires a fresh user ask even under a broad native allow or saved approval; no saved pattern is proposed.
Existing native denial remains denial, and ordinary workers cannot invoke the forwarding gate.
Nothing is forwarded until that assertion succeeds, and native user rejection propagates unchanged.
After confirmation, coordinator ownership/project scope and the pending decision are checked again; cancellation or a stale binding cannot reuse an earlier ask to forward a reply.
This gate is separate from the original runtime request and does not grant accept, merge, publication, or cleanup authority.

Results derive task/attempt IDs from `context.sessionID`, the current snapshot's `attempts[].sessionRef`, and the task's current `attemptId`.
All tools verify native session metadata against the service instance, task, and attempt before treating a session as managed.
Unknown sessions, stale attempts, duplicate mappings, and metadata collisions cannot submit another worker's result.
Result inputs do not select a target task or attempt.
Result inputs must explicitly supply an integer `generation >= 1` from the instructions whose evidence is being reported.
That generation is forwarded unchanged, never inferred or upgraded from a newer snapshot, so the engine can reject stale evidence and keep results keyed by attempt/generation.
The plugin exposes no accept, merge, publication, or workspace-cleanup tools.
The plugin is a model privilege boundary layered over the single-user administrative service API, not a replacement for server validation, OS containment, or human-review policy.

## Verification

The Go tests use `httptest` to cover exact envelopes, Basic auth, version rejection, ownership collisions, lost-response recovery across fresh adapter instances, prompt read/delivery races, decisions, interrupt/cancellation semantics, malformed responses, and bounded SSE parsing.
The plugin tests exercise service routes, exact operation-ID constraints, secure owned token fixtures, explicit hub opt-in and descendant denial before snapshot fetch, project filtering across every map, immutable role/directory bindings, controls, self-answer denial, generation forwarding, result ownership, and blocking/rejected native confirmations.
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

The smoke test checks exact runtime version, session creation/recovery, owned observation, parked prompt recovery without waking, interruption, owned pending-input cancellation, typed form replies, deterministic invalid-answer rejection, and SSE cancellation.
When the optional built plugin path is supplied, it starts without that plugin and hot-loads the package from an owned explicit config using URL/token-file options, without inheriting a service URL or restarting/changing the process environment.
The all-location reload used for this proof is limited to the test's single disposable runtime, after its earlier pending work has been settled.
It then runs a harmless native shell command proving the inherited token sentinel was cleared.
Native permission evaluations then check worker/coordinator read/edit scope, moved/malformed bindings, symlink/search-root escapes, denial of native delegation/MCP/admin actions, preservation of explicit denial, and preservation of saved shell approval, without executing those tools or commands.
A disposable prompt-admission fixture calls the compiled production confirmation helper against the real pinned `ctx.permission.assert` API with model execution disabled.
It verifies that broad native allows and saved approvals still produce a pending user ask with original request/answer metadata, and that admission remains blocked until an explicit native reply.
The subprocess exits when its private stdin closes; only that owned subprocess is force-terminated if graceful shutdown fails.
Live model execution is explicitly skipped because the isolated environment intentionally has no model credentials.
No model-provider success, external coding-work acceptance, personal-service compatibility beyond the exact pin, or publication authority is claimed by this smoke test.
