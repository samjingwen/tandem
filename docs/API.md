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
List responses are ID-sorted JSON arrays, including `[]` when empty.

| Method | Path | Input or query | Response |
| --- | --- | --- | --- |
| GET | `/health` | None | `{"status":"ok"}` for process liveness |
| GET | `/ready` | None | Diagnostics; HTTP 503 when runtime is incompatible |
| GET | `/version` | None | `{"version":"0.1.0","api":"v1"}` |
| GET | `/runtime` | None | Core `RuntimeInfo` |
| GET | `/doctor` | None | Diagnostics, including `ready: false` and runtime details |
| GET | `/projects` | None | Core `Project[]` |
| POST | `/projects` | `RegisterProject` | Core `Project` |
| GET | `/tasks` | Optional `projectId`, `state` | Core `Task[]` |
| POST | `/tasks` | `CreateTask` | Core `Task` |
| GET | `/tasks/{id}` | None | Task detail described below |
| POST | `/tasks/{id}/followup` | `{"text":"..."}` | Core `Task` |
| POST | `/tasks/{id}/control` | `{"action":"pause|resume|cancel","cascade":false}` | Core `Task` |
| GET | `/tasks/{id}/results` | None | Core `Result`, or 404 if absent |
| POST | `/tasks/{id}/results` | `SubmitResult` | Core `Result` |
| POST | `/tasks/{id}/accept` | Empty or `{}` | Core `Task` |
| POST | `/tasks/{id}/cleanup` | Empty or `{}` | Core `Workspace` |
| GET | `/decisions` | Optional `taskId`, `status` | Core `Decision[]` |
| POST | `/decisions/{id}/reply` | `{"answer": JSON_VALUE}` | Core `Decision` |
| GET | `/snapshot` | None | Core `State`, excluding `Operations` |
| GET | `/events` | None | Live-only SSE revision hints |

`RegisterProject` contains `name`, `path`, `baseRef`, and optional `instructions`.
`CreateTask` contains `projectId`, `title`, `instructions`, optional `role`, optional `parentId`, and optional `dependencies` (an ID array).
`SubmitResult` contains `attemptId`, `summary`, and `evidence`.
Each evidence record contains `name`, `reference`, and `passed`.
An evidence reference is a reference, not a fetched transcript or proof fabricated by the transport.

Task detail contains `task`, ID-sorted `attempts`, `commands`, `decisions`, and optional `result`.
The API preserves desired/observed state, command uncertainty, decision status, result acceptance, late-result flags, and evidence status exactly as supplied by core.
An idle runtime session does not become a completed or accepted task.
Accept does not imply merge or publication, and cleanup remains subject to core ownership checks.

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
Fetch `/v1/snapshot` to reconcile authoritative state after connecting, receiving a hint, or reconnecting.
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
List commands use terminal-control-sanitized tables without `--json`; other responses use formatted JSON.

```text
projects register --name NAME --path PATH --base-ref REF [--instructions TEXT]
projects list
tasks create --project ID --title TITLE --instructions TEXT [--role ROLE] [--parent ID] [--depends-on ID,ID]
tasks list [--project ID] [--state STATE]
tasks show ID
tasks followup ID --text TEXT
tasks pause|resume|cancel ID [--cascade]
tasks accept|cleanup ID
tasks result ID
tasks result ID --attempt ID --summary TEXT [--evidence JSON_ARRAY]
decisions list [--task ID] [--status STATUS]
decisions reply ID --answer JSON
snapshot
doctor
version
```

`tasks result ID` reads a result.
Supplying result submission options requires both `--attempt` and `--summary` and performs a mutation.
`--evidence` accepts an array such as `[{"name":"tests","reference":"artifact://report","passed":true}]`.
`--answer` accepts any valid JSON value; core/runtime validation remains authoritative.
`--instructions -`, `--text -`, `--answer -`, and `--evidence -` read bounded stdin without opening a file.
Unknown and duplicate options fail before dispatch.

## Verification boundary

The API, client, and CLI tests exercise the HTTP contract through fake `core.Service` implementations and disposable `httptest` servers.
They cover operation-ID propagation, auth, browser mutation rejection, strict bounded JSON, typed errors, preserved statuses/evidence, live notifications, CLI dispatch receipts, stdin, redirects, and ambiguous disconnects without mutation retries.
They do not establish acceptance against a real OpenCode runtime, persistent engine, native Worktrunk allocation, or production server.
