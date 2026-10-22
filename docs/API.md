# Tandem HTTP API and CLI

The API is the shared coordination surface for HTTP clients.
It delegates command validation, durable operation ownership, idempotency, and external mutation policy to `core.Service`.
Neither the HTTP handler nor the CLI starts a scheduler, creates a worktree, or invokes a runtime directly.

## Integration

```go
handler := api.New(service, api.Options{Token: token})
```

The parent server owns listener, bind-address, TLS, shutdown, and authentication configuration policy.
The client default URL is `http://127.0.0.1:7433`.
The handler does not make an unauthenticated remote listener safe by itself.
Require a token and an appropriate TLS/network boundary for remote use.

The CLI entrypoint is `cli.Run(ctx, args, out, errOut) error`, where `args` excludes the executable name.
Tests and embedding callers can use `cli.Runner{In: reader, Getenv: lookup, HTTPClient: transportClient}.Run(ctx, args, out, errOut)`.
The exported entrypoint reads standard input and the named environment variables, but never environment files or authentication caches.
Main remains responsible for server startup and displaying a returned CLI error.

The hand-authored OpenAPI 3.1 contract is `internal/api/openapi.json`.
The authenticated `GET /v1/openapi.json` route serves that embedded contract from the executable, independent of the working directory.
It describes v1 routes, request/response objects, required operation IDs, positive result generations, evidence fields, core input budgets, and live-only event semantics.

## Authentication and request safety

When `Options.Token` is configured, every `/v1/` route requires `Authorization: Bearer TOKEN`, including health, version, readiness, diagnostics, events, and unknown routes.
The handler never returns the configured token.
All responses use `Cache-Control: no-store`.
The API does not enable CORS.

Browser mutations with a cross-origin or opaque `Origin` are rejected.
`Sec-Fetch-Site: cross-site` and `same-site` are also rejected for mutations.
Non-browser clients may omit these headers.
Origin comparison uses the actual request Host and TLS state, not forwarded headers.
A reverse proxy must preserve the external origin or enforce its own same-origin boundary.

Every mutation requires exactly one `X-Operation-ID` header.
IDs contain 1-128 ASCII letters, digits, `.`, `_`, `:`, or `-`, beginning with a letter or digit.
The response echoes a valid operation ID, including on command errors.
The caller owns and retains the ID before dispatch.
Retry only with the same ID and exact command body; never generate a replacement ID to resolve an unknown outcome.
Core decides whether a repeated operation is a replay, conflict, or reconciliation boundary.
The API does not add a second idempotency store or retry service calls.

Mutation bodies require `Content-Type: application/json` and contain exactly one object.
Unknown fields, null bodies, trailing JSON, and bodies exceeding 1 MiB are rejected.
Unknown nested evidence fields are rejected too; decision `answer` remains arbitrary JSON by contract.
Accept and cleanup also allow an empty body without a content type, or `{}` with JSON content type.
Handler contexts are bounded to 30 seconds, including event streams.
Service implementations must honor cancellation; cancellation is not evidence that a mutation did not occur.

## Routes

All paths below start with `/v1`.
Successful command responses use HTTP 200, including replays delegated to core.
Legacy list responses are ID-sorted JSON arrays, including `[]` when empty.
Their existing shapes remain unchanged; new clients should use the additive bounded read surface below, not unrestricted inventories.

| Method | Path | Input or query | Response |
| --- | --- | --- | --- |
| GET | `/health` | None | `{"status":"ok"}` for process liveness |
| GET | `/ready` | None | Diagnostics; HTTP 503 when runtime is incompatible |
| GET | `/version` | None | `{"version":"0.1.0","api":"v1"}` |
| GET | `/runtime` | None | Core `RuntimeInfo` |
| GET | `/doctor` | None | Diagnostics, including `ready: false` and runtime details |
| GET | `/openapi.json` | None | Embedded OpenAPI 3.1 contract |
| GET | `/projects` | None | Core `Project[]` |
| POST | `/projects` | `RegisterProject` | Core `Project` |
| GET | `/tasks` | Optional `projectId`, `state` | Core `Task[]` |
| POST | `/tasks` | `CreateTask` | Core `Task` |
| GET | `/tasks/{id}` | None | Task detail described below |
| POST | `/tasks/{id}/followup` | `{"text":"..."}` | Core `Task` |
| POST | `/tasks/{id}/control` | `{"action":"pause","cascade":false}` | Core `Task` |
| GET | `/tasks/{id}/results` | None | Current attempt/generation `Result`, or 404 if absent |
| POST | `/tasks/{id}/results` | `SubmitResult` | Core `Result` |
| POST | `/tasks/{id}/accept` | Empty or `{}` | Core `Task` |
| POST | `/tasks/{id}/cleanup` | Empty or `{}` | Core `Workspace` |
| GET | `/decisions` | Optional `taskId`, `status` | Core `Decision[]` |
| POST | `/decisions/{id}/reply` | `{"answer": JSON_VALUE}` | Core `Decision` |
| GET | `/snapshot` | None | Core `State`, excluding `Operations` |
| GET | `/events` | None | Live-only SSE revision hints |
| GET | `/reads/snapshot` | None | Fixed-size instance/revision manifest and six collection counts |
| GET | `/reads/records` | `collection`, `instanceId`, `revision`, exact filters; or issued `cursor` | One bounded projected record page |
| GET | `/reads/content` | Issued content `cursor` | One UTF-8-safe chunk of the complete retained JSON record |

`RegisterProject` contains `name`, `path`, `baseRef`, optional `instructions`, and optional `instructionFiles` (up to 16 explicit Markdown paths).
Registration of an existing canonical path with a new operation ID refreshes its configuration while preserving its stable project ID and already assigned worktrees/sessions.
Existing accepted operation IDs still replay their original receipt instead of rereading changed files.
Instruction content is snapshotted for future worker prompts; shared policies outside a worktree should be supplied explicitly rather than granting broad runtime filesystem access.
`CreateTask` contains `projectId`, `title`, `instructions`, optional `role`, optional `parentId`, and optional `dependencies` (an ID array).
`SubmitResult` requires `attemptId`, a positive integer `generation`, `summary`, and nonempty `evidence`.
Each evidence record contains `name`, `reference`, and `passed`.
An evidence reference is a reference, not a fetched transcript or proof fabricated by the transport.

Task titles have a 500-byte UTF-8 budget, and task instructions/followups have a 128 KiB UTF-8 budget.
Roles are `worker` (default) or `coordinator`.
The engine's configured task/decomposition/concurrency limits remain authoritative; defaults are 128 tasks, depth 3, two workers, and one active coordinator.
Controls support `pause`, `resume`, `cancel`, and `retry`; retry requires a conclusively failed task.

Task detail contains `task`, ID-sorted `attempts` and `decisions`, `commands` sorted by durable sequence then ID, optional current `result`, and all retained `results` sorted by generation then attempt ID.
The current result matches both `task.attemptId` and `task.generation`.
Older attempts or generations remain visible in `results` but never become the current result or acceptance evidence for newer work.
Snapshot result maps use the key `attemptId + "/" + generation`, not the task ID.
Decision IDs are safe `dec_` plus hexadecimal IDs; runtime request references remain opaque data, not URL path components.
The API preserves desired/observed state, command uncertainty, decision status, result acceptance, late-result flags, and evidence status exactly as supplied by core.
An idle runtime session does not become a completed or accepted task.
Accept does not imply merge or publication, and cleanup remains subject to core ownership checks.
Cleanup returns a stable request receipt after persisting intent, not proof of deletion.
Inspect the attempt's `cleanup_pending`/`cleaned` phase and error for the observed outcome.
Task cleanup resolves and verifies the task's current owned attempt from a snapshot, then passes the attempt ID to core.
Core rechecks terminal task state, runtime ownership/quiescence, clean checkout, and operation fingerprint before changing a workspace.
The cleanup receipt binds the resolved attempt; a later changed task binding can conflict on retry rather than silently cleaning a replacement attempt.

## Bounded reads and compatibility

The `/v1/reads/` surface is additive: no legacy HTTP read silently changes from a full record/array/State to a projection.
Existing consumers can migrate explicitly, using the served OpenAPI contract.
Legacy full reads can still exceed a consumer's safe response limit; they are not the large-inventory traversal interface.
Valid 128-KiB task instructions, valid followups, configured inventory limits, and all persisted content remain supported without an aggregate admission restriction.
No task, followup, decision, result, or evidence is truncated or discarded to fit a response.

Start with `GET /v1/reads/snapshot`:

```json
{"instanceId":"instance","revision":42,"collections":{"projects":1,"tasks":65,"attempts":2,"commands":2,"decisions":0,"results":0},"maxPageBytes":262144,"contentBytes":32768,"maxRecords":64}
```

The manifest has six counts, not record inventories, so its field count is independent of the configured task count and accumulated history/content.
Every bounded response is at most 256 KiB, including JSON escaping, envelope fields, and final newline.
Collection pages default to 32 records and accept `limit=1..64`; the byte budget may end a page earlier.
Invalid limits are rejected rather than clamped.

For example, request `/v1/reads/records?collection=tasks&instanceId=instance&revision=42&projectId=PROJECT_ID&limit=16`.
Supported collections are `projects`, `tasks`, `attempts`, `commands`, `decisions`, and `results`.
Optional exact filters are `key`, `projectId`, and `taskId` (not for projects), plus task-only `state`, decision-only `status`, and attempt-only `sessionRef`.
Project filtering also applies to attempts, commands, decisions, and results through their owning task.
The result key remains `attemptId + "/" + generation`.
Commands sort by durable sequence then key, results by generation then key, and other collections by key.
Each record page contains `instanceId`, `revision`, `collection`, `records`, and an optional `nextCursor`.
Each record is `{"key":"...","summary":{...},"content":{"cursor":"...","bytes":12345,"sha256":"..."}}`.

`summary` is an explicitly incomplete navigation/status projection, never a substitute core record or result evidence.
Its allowed scalar fields are `id`, `projectId`, `taskId`, `parentId`, `attemptId`, `generation`, `role`, `state`, `desired`, `status`, `kind`, `sequence`, `accepted`, `late`, `phase`, `observed`, `sessionRef`, `title`, `name`, `path`, and `baseRef`.
String fields exceeding 1024 UTF-8 bytes are omitted entirely from this projection; an attempt can additionally expose `workspacePath` up to 4096 bytes.
All omitted fields and original values remain available in the referenced complete JSON record, including dependencies, instructions, command text, errors, decision details/replies, result summaries, and every evidence entry.
An empty page uses `records: []`; an exact key lookup that does not exist returns 404.

Fetch `/v1/reads/content?cursor=CONTENT_CURSOR` to read the complete JSON record in byte-bounded chunks.
Each response contains `instanceId`, `revision`, `collection`, `key`, byte `offset`, `totalBytes`, `text`, `sha256`, and optional `nextCursor`.
`text` contains at most 32 KiB of original JSON UTF-8 bytes and never splits a UTF-8 code point.
A chunk need not be independently parseable JSON; concatenate its text bytes in contiguous offset order, verify the declared byte count and SHA-256, then decode the complete record.
This preserves escaped/control characters, Unicode, arbitrary nested decision JSON, evidence arrays, and single records larger than 8 MiB.
Stream explicit full output when appropriate instead of retaining a complete inventory in client memory.

Cursors are opaque authenticated claims bound to the instance, global store revision, collection, exact filters, limit, offset, and record/content mode.
Follow the issued cursor unchanged, using the same endpoint.
Repeated scope parameters are accepted only when they exactly match the cursor; unknown/repeated query parameters are rejected.
Changing collection, filter, instance, revision, or limit cannot widen or repurpose a cursor.
Each page reads one atomic durable revision, not an indefinitely retained historical snapshot.
Any instance/revision change returns HTTP 409 `stale_read`; invalid, modified, or process-restarted cursors return HTTP 400 `invalid_cursor`.
Discard the entire partial traversal, fetch a new manifest, and start again; never append a new revision to old partial output.
Cursor-signing keys rotate when the handler/process restarts, even when the durable instance and revision remain unchanged.
Cursors are not bearer-token replacements or authorization grants, and every page still requires normal API authentication.

### Large exact mutation receipts

Mutation request bodies, operation fingerprints, durable outcomes, replay identity, and default legacy response shapes are unchanged.
An accumulated task can also make a mutation's original outcome large, so clients may explicitly send `Prefer: return=bounded`.
When that exact outcome exceeds 256 KiB, the response is a bounded `{"instanceId":"...","revision":42,"representation":"operation-receipt","operationId":"...","record":{...}}` with `Preference-Applied: return=bounded`.
The record's content references the exact durable operation outcome, not a later current task substituted for an old replay.
Small negotiated responses keep their original shape.
Receipt content uses the same chunk interface; `collection=receipts` additionally permits an exact caller-retained operation `key`, never journal enumeration or fingerprint access.
The six-count manifest does not expose the operation journal.
As with any other uncertain mutation response, a failed receipt traversal retains the original operation ID and exact input for explicit reconciliation; it never authorizes an automatic POST retry or replacement operation ID.

Diagnostics contain `ready`, `version`, `instanceId`, `revision`, and `runtime`.
Readiness requires a successful durable snapshot and compatible runtime information.
Doctor retains an incompatible runtime's diagnostics in an HTTP 200 response so clients can inspect them.
Storage/runtime query errors still use the typed error response.

## Errors

```json
{"error":{"code":"uncertain","message":"external outcome cannot be established; reconciliation required"}}
```

| HTTP status | Typical code |
| --- | --- |
| 400 | `invalid_request`, `invalid_operation_id` |
| 401 | `unauthorized` |
| 403 | `cross_origin`, `forbidden` |
| 404 | `not_found` |
| 405 | `method_not_allowed` |
| 408 | `canceled` |
| 409 | `uncertain` and other typed core command conflicts |
| 413 | `body_too_large` |
| 415 | `unsupported_media_type` |
| 500 | `internal` |
| 503 | `unavailable`, `runtime_unavailable`, `incompatible`, or readiness diagnostics |
| 504 | `timeout` |

Unexpected backend and decoder errors are sanitized rather than echoing private internals or submitted body content.
Typed core error codes and messages are preserved, with unknown core codes treated as command conflicts (409).
A timeout, disconnect, cancellation, malformed response, or server failure does not authorize a speculative repeat with a new operation ID.

## Live notifications

`GET /v1/events` returns `Content-Type: text/event-stream` and `X-Tandem-Events: live-only`.
It immediately emits the current instance/revision and polls snapshots once per second for changes.

```text
event: revision
data: {"instanceId":"...","revision":7}

```

These are invalidation hints, not a durable event log.
There are no SSE `id` fields and no replay guarantee.
`Last-Event-ID` is ignored.
Changes may coalesce, and a notification may be lost on disconnect.
Fetch `/v1/reads/snapshot` and reconcile authoritative revision-bound pages after connecting, receiving a hint, or reconnecting.
Legacy consumers may continue using `/v1/snapshot` only when their inventory fits their existing response budget.
The stream closes on cancellation, storage failure, or its 30-second context deadline.
Writes are bounded by a five-second deadline capped at the request deadline.
Clients must reconnect and refetch state rather than treating stream continuity as completion evidence.

## HTTP client

```go
c, err := client.New(client.Options{URL: serverURL, Token: token})
// Handle err before use.
err = c.Do(ctx, http.MethodPost, "/v1/tasks", operationID, input, &task)
```

`client.FromEnv()` opts into `TANDEM_URL` and `TANDEM_TOKEN`; `client.New` uses explicit options only.
URLs must be HTTP(S) server roots without user information, paths, queries, or fragments.
`Do` accepts root-relative `/v1/` paths, optional query strings, GET or POST, and JSON input/output values.
POST requests with nil input send `{}` and disable transport body replay, including for bodyless commands.
The default request timeout is 35 seconds, with an optional caller-supplied `HTTPClient`.
Responses are bounded to 8 MiB.
The `/v1/reads/` client boundary is stricter at 256 KiB.
`Manifest`, `ReadPage`, `Walk`, and `WriteRecord` expose explicit bounded traversal without constructing a full State or inventory.
`WriteMutation` opts into bounded exact receipts and streams large outcomes after one POST, with no mutation retry.
An accepted mutation followed by a failed content read returns `*client.ReceiptReadError`, preserving the original `OperationID` and wrapped read failure instead of presenting the GET as a rejected mutation.
The client never logs credentials, follows redirects, or automatically retries a failed mutation.
Structured failures are `*client.Error`, retaining HTTP status, code, message, and caller operation ID.
Transport failures are `*client.TransportError`, retaining the cause for `errors.Is`, the operation ID, and a conservative mutation `Uncertain` flag without printing transport internals.
The generic JSON client is not an SSE consumer; use a separate authenticated streaming request for events.

## CLI

Global options can appear before or after the command: `--url URL`, `--json`, `--operation-id ID`.
`--url` overrides `TANDEM_URL`; `TANDEM_TOKEN` is sent as a bearer token and is not accepted on the command line.
No token is printed.
Mutation output starts with `Operation ID: ID` on stderr before any network dispatch; stdout remains valid JSON with `--json`.
If no ID is supplied, the CLI generates a cryptographically random ID and prints it first.
Keep that receipt and the exact command data for any manual retry, including stdin data.
If the receipt cannot be written, no mutation is dispatched.
List commands use terminal-control-sanitized projected tables without `--json`.
JSON lists, task details/results, and explicit full snapshots stream complete records through bounded content pages, not an unbounded in-memory aggregate.
Default `snapshot` returns the lean manifest; `snapshot --full` explicitly requests a streamed full State-shaped export.
Unlike a successful buffered command, a failed streaming command may leave partial output; discard it on a nonzero exit, including `stale_read`, and restart the whole read rather than appending.
CLI mutations negotiate bounded exact receipts but still print the original complete outcome, streamed when necessary.

```text
projects register --name NAME --path PATH --base-ref REF [--instructions TEXT]
projects list
tasks create --project ID --title TITLE --instructions TEXT [--role ROLE] [--parent ID] [--depends-on ID,ID]
tasks list [--project ID] [--state STATE]
tasks show ID
tasks followup ID --text TEXT
tasks pause|resume|cancel|retry ID [--cascade]
tasks accept|cleanup ID
tasks result ID
tasks result ID --attempt ID --generation N --summary TEXT [--evidence JSON_ARRAY]
decisions list [--task ID] [--status STATUS]
decisions reply ID --answer JSON
snapshot [--full]
doctor
version
```

`tasks result ID` reads a result.
Supplying result submission options requires `--attempt`, `--generation`, and `--summary` and performs a mutation.
`--generation` must be an integer greater than or equal to 1 and must identify the instruction generation actually verified, not merely the latest task generation observed later.
`--evidence` accepts an array such as `[{"name":"tests","reference":"artifact://report","passed":true}]`.
`--answer` accepts any valid JSON value; core/runtime validation remains authoritative.
`--instructions -`, `--text -`, `--answer -`, and `--evidence -` read bounded stdin without opening a file.
Unknown and duplicate options fail before dispatch.

## Verification boundary

The API/client/CLI unit tests exercise the transport contract through fake `core.Service` implementations and disposable `httptest` servers.
They cover operation-ID propagation, auth, browser mutation rejection, strict bounded JSON, typed errors, preserved statuses/evidence, live notifications, CLI dispatch receipts, stdin, redirects, and ambiguous disconnects without mutation retries.
`internal/integration` additionally runs real engine commands through two independent HTTP/CLI clients against SQLite and disposable Git repositories/worktrees, with only the runtime simulated.
It verifies concurrent duplicate creation yields one attempt/session/worktree, client disconnects do not stop workers, closing/reopening SQLite and recreating the server preserves the instance/session/worktree and retained files, another client can answer a decision, and only the current verified generation can be accepted.
It also reproduces and covers result lookup and task-to-attempt cleanup routing, and uses a typed OpenAPI consumer to validate references, mutation requirements, real response payloads, and invalid generation inputs against the served schema.
These tests do not establish acceptance against real OpenCode, native Worktrunk allocation, or a production listener.
