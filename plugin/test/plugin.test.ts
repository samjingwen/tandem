import assert from "node:assert/strict"
import { test } from "node:test"
import type { ToolContext } from "@opencode/plugin/promise/tool"
import { currentWorker, TandemClient } from "../src/client.ts"
import { tools } from "../src/tools.ts"

const state = {
  instanceId: "instance",
  tasks: { task: { id: "task", projectId: "project", attemptId: "attempt", state: "running" } },
  attempts: { attempt: { id: "attempt", taskId: "task", sessionRef: "ses_worker" } },
}

function context(sessionID = "ses_worker") {
  return {
    sessionID: sessionID as ToolContext["sessionID"],
    signal: new AbortController().signal,
  }
}

function fixture(snapshot: unknown = state) {
  const calls: { path: string; headers: Headers; body: unknown; signal: AbortSignal | null | undefined }[] =
    []
  const request: typeof fetch = (input, init) => {
    const url = input instanceof Request ? new URL(input.url) : new URL(input)
    calls.push({
      path: url.pathname,
      headers: new Headers(init?.headers),
      body: typeof init?.body === "string" ? (JSON.parse(init.body) as unknown) : undefined,
      signal: init?.signal,
    })
    return Promise.resolve(Response.json(url.pathname === "/v1/snapshot" ? snapshot : { ok: true }))
  }
  const client = new TandemClient("http://127.0.0.1:7000", "test-secret", request)
  const registered = tools(client, (id) =>
    Promise.resolve({
      id,
      metadata: { tandem: { instanceId: "instance", taskId: "task", attemptId: "attempt" } },
    }),
  )
  function tool(name: string) {
    const found = registered.find((item) => item.name === name)
    if (!found) throw new Error(`Missing tool ${name}`)
    return found
  }
  return { client, calls, registered, tool }
}

await test("client uses versioned service routes, bearer auth, stable operation IDs, and cancellation", async () => {
  const { tool, calls, registered } = fixture()
  await tool("followup").execute({ operationId: "op_1", taskId: "task/one", text: "continue" }, context())
  assert.equal(calls[0]?.path, "/v1/tasks/task%2Fone/followup")
  assert.equal(calls[0].headers.get("Authorization"), "Bearer test-secret")
  assert.equal(calls[0].headers.get("X-Operation-ID"), "op_1")
  assert.deepEqual(calls[0].body, { text: "continue" })
  assert.ok(calls[0].signal)
  await tool("decision").execute(
    { operationId: "op_2", decisionId: "decision", answer: { decision: "once" } },
    context(),
  )
  assert.equal(calls[1]?.path, "/v1/decisions/decision/reply")
  assert.deepEqual(calls[1].body, { answer: { decision: "once" } })
  assert.deepEqual(
    registered.map((item) => item.name),
    ["create", "status", "followup", "decision", "result"],
  )
  assert.ok(registered.every((item) => !/accept|merge|cleanup/u.test(item.name)))
})

await test("worker proposals are parented to their current task and cannot change project", async () => {
  const { tool, calls } = fixture()
  const proposal = { operationId: "op", projectId: "project", title: "child", instructions: "do work" }
  await tool("create").execute(proposal, context())
  assert.deepEqual(calls[1]?.body, {
    projectId: "project",
    parentId: "task",
    title: "child",
    instructions: "do work",
  })
  await assert.rejects(
    tool("create").execute({ ...proposal, projectId: "foreign" }, context()),
    /own project/u,
  )
  await assert.rejects(
    tool("create").execute({ ...proposal, parentId: "foreign" }, context()),
    /current task/u,
  )
  assert.equal(calls.filter((call) => call.path === "/v1/tasks").length, 1)
})

await test("coordinator may create an unparented root task", async () => {
  const { tool, calls } = fixture()
  await tool("create").execute(
    { operationId: "op", projectId: "project", title: "root", instructions: "plan" },
    context("ses_coordinator"),
  )
  assert.deepEqual(calls[1]?.body, { projectId: "project", title: "root", instructions: "plan" })
})

await test("structured result derives task/attempt only from current native ownership", async () => {
  const { tool, calls } = fixture()
  const evidence = [{ name: "test", reference: "go test ./...", passed: true }]
  await tool("result").execute(
    { operationId: "op_result", summary: "done", evidence, taskId: "foreign", attemptId: "foreign" },
    context(),
  )
  assert.equal(calls[1]?.path, "/v1/tasks/task/results")
  assert.deepEqual(calls[1].body, { attemptId: "attempt", summary: "done", evidence })
  await assert.rejects(
    tool("result").execute({ operationId: "bad", summary: "done", evidence }, context("ses_foreign")),
    /not a Tandem worker/u,
  )
  assert.equal(calls.filter((call) => call.path.endsWith("/results")).length, 1)
})

await test("stale, ambiguous, malformed, and native metadata collisions reject results", async () => {
  const stale = structuredClone(state)
  stale.tasks.task.attemptId = "new_attempt"
  assert.throws(() => currentWorker(stale, "ses_worker"), /not the current/u)
  const ambiguous = {
    ...state,
    attempts: { ...state.attempts, duplicate: { ...state.attempts.attempt, id: "duplicate" } },
  }
  assert.throws(() => currentWorker(ambiguous, "ses_worker"), /Ambiguous/u)
  const { client, calls } = fixture()
  const collision = tools(client, (id) =>
    Promise.resolve({
      id,
      metadata: { tandem: { instanceId: "foreign", taskId: "task", attemptId: "attempt" } },
    }),
  ).find((item) => item.name === "result")
  assert.ok(collision)
  await assert.rejects(
    collision.execute({ operationId: "bad", summary: "done", evidence: [] }, context()),
    /does not match/u,
  )
  assert.equal(calls.filter((call) => call.path.endsWith("/results")).length, 0)
  const malformed = fixture({ ...state, tasks: [] })
  await assert.rejects(malformed.client.snapshot(context().signal), /JSON object/u)
})

await test("uncertain writes are never retried and error text never reveals token or payload", async () => {
  let requests = 0
  const client = new TandemClient("http://localhost:7000", "secret", () => {
    requests++
    return Promise.reject(new Error("secret and private prompt"))
  })
  await assert.rejects(
    client.request("/v1/tasks", context().signal, { private: "prompt" }, "op"),
    (error: unknown) =>
      error instanceof Error &&
      /SAME operationId/u.test(error.message) &&
      !/secret|private/u.test(error.message),
  )
  assert.equal(requests, 1)
  await assert.rejects(client.request("/v1/tasks", context().signal, {}), /stable operationId/u)
  assert.equal(requests, 1)
  assert.throws(() => new TandemClient("http://remote.example", "secret"), /HTTPS/u)
  assert.throws(() => new TandemClient("https://user:secret@example.com", "secret"), /embedded credentials/u)
})
