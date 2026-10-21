import assert from "node:assert/strict"
import { test } from "node:test"
import type { ToolContext } from "@opencode/plugin/promise/tool"
import { currentWorker, TandemClient } from "../src/client.ts"
import { tools } from "../src/tools.ts"
import type { AssertPermission, NativeAccess } from "../src/authority.ts"

const state = {
  instanceId: "instance",
  tasks: {
    task: {
      id: "task",
      projectId: "project",
      attemptId: "attempt",
      state: "running",
      role: "worker",
      generation: 3,
    },
  },
  attempts: { attempt: { id: "attempt", taskId: "task", sessionRef: "ses_worker" } },
  decisions: {
    decision: { id: "decision", taskId: "task", attemptId: "attempt", kind: "form", status: "pending" },
  },
}

function context(sessionID = "ses_worker") {
  return {
    sessionID: sessionID as ToolContext["sessionID"],
    signal: new AbortController().signal,
    agent: "build" as ToolContext["agent"],
    messageID: "msg_test" as ToolContext["messageID"],
    id: "call_test" as ToolContext["id"],
  }
}

function fixture(snapshot: unknown = state, assertion: AssertPermission = () => Promise.resolve()) {
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
  const native: NativeAccess = {
    session: (id) =>
      Promise.resolve({
        id,
        metadata:
          id === "ses_worker"
            ? { tandem: { instanceId: "instance", taskId: "task", attemptId: "attempt" } }
            : {},
        location: { directory: "/work/task" },
      }),
    assert: assertion,
  }
  const registered = tools(client, native)
  function tool(name: string) {
    const found = registered.find((item) => item.name === name)
    if (!found) throw new Error(`Missing tool ${name}`)
    return found
  }
  return { client, calls, registered, tool, native }
}

await test("client uses versioned service routes, bearer auth, stable operation IDs, and cancellation", async () => {
  const { tool, calls, registered } = fixture()
  await tool("followup").execute({ operationId: "op_1", taskId: "task", text: "continue" }, context())
  const followup = calls.find((call) => call.path.endsWith("/followup"))
  assert.ok(followup)
  assert.equal(followup.path, "/v1/tasks/task/followup")
  assert.equal(followup.headers.get("Authorization"), "Bearer test-secret")
  assert.equal(followup.headers.get("X-Operation-ID"), "op_1")
  assert.deepEqual(followup.body, { text: "continue" })
  assert.ok(followup.signal)
  await tool("decision").execute(
    { operationId: "op_2", decisionId: "decision", answer: { answer: { approved: true } } },
    context(),
  )
  const reply = calls.find((call) => call.path.endsWith("/reply"))
  assert.equal(reply?.path, "/v1/decisions/decision/reply")
  assert.deepEqual(reply.body, { answer: { answer: { approved: true } } })
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
    {
      operationId: "op_result",
      generation: 2,
      summary: "done",
      evidence,
      taskId: "foreign",
      attemptId: "foreign",
    },
    context(),
  )
  assert.equal(calls[1]?.path, "/v1/tasks/task/results")
  assert.deepEqual(calls[1].body, { attemptId: "attempt", generation: 2, summary: "done", evidence })
  await assert.rejects(
    tool("result").execute(
      { operationId: "bad", generation: 2, summary: "done", evidence },
      context("ses_foreign"),
    ),
    /not a Tandem worker/u,
  )
  assert.equal(calls.filter((call) => call.path.endsWith("/results")).length, 1)
})

await test("stale, ambiguous, malformed, and native metadata collisions reject results", async () => {
  const stale = structuredClone(state)
  stale.tasks.task.attemptId = "new_attempt"
  assert.throws(() => currentWorker({ ...stale, raw: stale }, "ses_worker"), /not the current/u)
  const ambiguous = {
    ...state,
    attempts: { ...state.attempts, duplicate: { ...state.attempts.attempt, id: "duplicate" } },
  }
  assert.throws(() => currentWorker({ ...ambiguous, raw: ambiguous }, "ses_worker"), /Ambiguous/u)
  const { client, calls } = fixture()
  const collision = tools(client, {
    session: (id) =>
      Promise.resolve({
        id,
        metadata: { tandem: { instanceId: "foreign", taskId: "task", attemptId: "attempt" } },
        location: { directory: "/work/task" },
      }),
    assert: () => Promise.resolve(),
  }).find((item) => item.name === "result")
  assert.ok(collision)
  await assert.rejects(
    collision.execute({ operationId: "bad", generation: 2, summary: "done", evidence: [] }, context()),
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
  assert.throws(() => new TandemClient("http://localhost:7000?", "secret"), /embedded credentials/u)
})

await test("managed model status must not reveal another project's task instructions", async () => {
  const scoped = {
    ...state,
    tasks: {
      ...state.tasks,
      foreign: {
        id: "foreign",
        projectId: "private-project",
        state: "queued",
        instructions: "PRIVATE OTHER PROJECT",
      },
    },
  }
  const { tool } = fixture(scoped)
  const result = await tool("status").execute({}, context())
  assert.ok(!result.content.includes("PRIVATE OTHER PROJECT"))
})

await test("managed ordinary workers cannot steer an unrelated task", async () => {
  const { tool } = fixture()
  await assert.rejects(
    tool("followup").execute({ operationId: "op_foreign", taskId: "foreign", text: "steer" }, context()),
  )
})

await test("result generation and operation ID bounds are enforced at the plugin boundary", async () => {
  const { tool, client } = fixture()
  await assert.rejects(
    tool("result").execute({ operationId: "op", summary: "done", evidence: [] }, context()),
  )
  await assert.rejects(client.request("/v1/tasks", context().signal, {}, "x".repeat(129)))
  for (const id of [" leading", "x/y", "_leading", "x\n", "é", ""]) {
    await assert.rejects(client.request("/v1/tasks", context().signal, {}, id))
  }
  for (const id of ["x".repeat(128), "A.b_c:d-9"]) await client.request("/v1/tasks", context().signal, {}, id)
  for (const generation of [0, -1, 1.5, "3", undefined]) {
    await assert.rejects(
      tool("result").execute({ operationId: "op", generation, summary: "done", evidence: [] }, context()),
      /explicit integer generation/u,
    )
  }
})

await test("project filtering covers every task-bearing map and drops unknown global extensions", async () => {
  const foreignTask = {
    id: "foreign",
    projectId: "private-project",
    state: "queued",
    instructions: "PRIVATE OTHER PROJECT",
  }
  const snapshot = {
    ...state,
    projects: {
      project: { id: "project", instructions: "OWN PROJECT" },
      foreign: { id: "foreign", instructions: "PRIVATE OTHER PROJECT" },
    },
    tasks: { ...state.tasks, foreign: foreignTask },
    attempts: {
      ...state.attempts,
      foreign: {
        id: "foreign",
        taskId: "foreign",
        sessionRef: "ses_other",
        workspace: { path: "PRIVATE OTHER PROJECT" },
      },
    },
    commands: { foreign: { taskId: "foreign", text: "PRIVATE OTHER PROJECT" } },
    decisions: { ...state.decisions, foreign: { taskId: "foreign", details: "PRIVATE OTHER PROJECT" } },
    results: {
      "foreign/7": { taskId: "foreign", summary: "PRIVATE OTHER PROJECT" },
      "attempt/2": { taskId: "task", summary: "OWN RESULT" },
    },
    futureGlobalExtension: "PRIVATE OTHER PROJECT",
  }
  const f = fixture(snapshot)
  const managed = await f.tool("status").execute({}, context())
  assert.ok(!managed.content.includes("PRIVATE OTHER PROJECT"))
  assert.ok(managed.content.includes("OWN PROJECT"))
  assert.ok(managed.content.includes("OWN RESULT"))
  const unbound = await f.tool("status").execute({}, context("ses_dev_coordinator"))
  assert.ok(unbound.content.includes("PRIVATE OTHER PROJECT"))
})

await test("missing or stale native bindings never fall back to global coordinator authority", async () => {
  const f = fixture({ ...state, attempts: {} })
  await assert.rejects(f.tool("status").execute({}, context()), /not a current Tandem/u)
  const stale = structuredClone(state)
  stale.tasks.task.attemptId = "new"
  const old = fixture(stale)
  await assert.rejects(old.tool("status").execute({}, context()), /not the current/u)
})

await test("ordinary workers cannot escalate roles, cross project dependencies, or answer approvals", async () => {
  let assertions = 0
  const f = fixture(
    {
      ...state,
      tasks: {
        ...state.tasks,
        peer: { id: "peer", projectId: "project", state: "queued" },
        foreign: { id: "foreign", projectId: "other", state: "queued" },
      },
      decisions: {
        ...state.decisions,
        permission: {
          id: "permission",
          taskId: "task",
          attemptId: "attempt",
          kind: "permission",
          status: "pending",
        },
      },
    },
    () => {
      assertions++
      return Promise.resolve()
    },
  )
  const proposal = { operationId: "op", projectId: "project", title: "child", instructions: "do work" }
  await assert.rejects(
    f.tool("create").execute({ ...proposal, role: "coordinator" }, context()),
    /coordinator privileges/u,
  )
  await assert.rejects(
    f.tool("create").execute({ ...proposal, dependencies: ["foreign"] }, context()),
    /Dependencies/u,
  )
  await assert.rejects(
    f.tool("followup").execute({ operationId: "op", taskId: "peer", text: "steer" }, context()),
    /Ordinary workers/u,
  )
  for (const decision of ["once", "always", "reject"]) {
    await assert.rejects(
      f
        .tool("decision")
        .execute({ operationId: "op", decisionId: "permission", answer: { decision } }, context()),
      /cannot answer permission/u,
    )
  }
  assert.equal(assertions, 0)
  assert.equal(f.calls.filter((call) => call.body !== undefined).length, 0)
})

await test("managed coordinators can steer only their project and must await native confirmation", async () => {
  const coordinator = {
    ...state,
    tasks: {
      task: { ...state.tasks.task, role: "coordinator" },
      peer: { id: "peer", projectId: "project", state: "running" },
      foreign: { id: "foreign", projectId: "other", state: "running" },
    },
    decisions: {
      approval: {
        id: "approval",
        taskId: "peer",
        attemptId: "peer-attempt",
        kind: "permission",
        status: "pending",
        details: { id: "per_original", action: "shell", resources: ["git status"] },
      },
      foreign: {
        id: "foreign",
        taskId: "foreign",
        attemptId: "other",
        kind: "permission",
        status: "pending",
      },
    },
  }
  let release = () => {}
  let started = () => {}
  const waiting = new Promise<void>((resolve) => {
    release = resolve
  })
  const notified = new Promise<void>((resolve) => {
    started = resolve
  })
  const assertions: Parameters<AssertPermission>[0][] = []
  const f = fixture(coordinator, (input) => {
    assertions.push(input)
    started()
    return waiting
  })
  await f.tool("followup").execute({ operationId: "follow", taskId: "peer", text: "route" }, context())
  await assert.rejects(
    f.tool("followup").execute({ operationId: "bad", taskId: "foreign", text: "route" }, context()),
    /owned project/u,
  )
  await assert.rejects(
    f
      .tool("decision")
      .execute({ operationId: "bad", decisionId: "foreign", answer: { decision: "once" } }, context()),
    /owned project/u,
  )
  const answer = { decision: "once" }
  const pending = f
    .tool("decision")
    .execute({ operationId: "approve", decisionId: "approval", answer }, context())
  await notified
  assert.equal(f.calls.filter((call) => call.path.endsWith("/reply")).length, 0)
  assert.equal(assertions[0]?.action, "tandem.permission.reply")
  assert.deepEqual(assertions[0].save, [])
  assert.deepEqual(assertions[0].metadata?.request, coordinator.decisions.approval.details)
  assert.deepEqual(assertions[0].metadata.answer, { decision: "once" })
  assert.deepEqual(assertions[0].source, { type: "tool", messageID: "msg_test", id: "call_test" })
  answer.decision = "always"
  release()
  await pending
  const forwarded = f.calls.find((call) => call.path.endsWith("/approval/reply"))
  assert.deepEqual(forwarded?.body, { answer: { decision: "once" } })
})

await test("native rejection propagates without forwarding; unbound coordinators cannot answer decisions", async () => {
  const denied = new Error("native user rejected")
  const f = fixture(
    {
      ...state,
      tasks: { task: { ...state.tasks.task, role: "coordinator" } },
      decisions: {
        approval: {
          id: "approval",
          taskId: "task",
          attemptId: "attempt",
          kind: "permission",
          status: "pending",
        },
      },
    },
    () => Promise.reject(denied),
  )
  await assert.rejects(
    f
      .tool("decision")
      .execute({ operationId: "reject", decisionId: "approval", answer: { decision: "once" } }, context()),
    (error) => error === denied,
  )
  await assert.rejects(
    f
      .tool("decision")
      .execute(
        { operationId: "unbound", decisionId: "approval", answer: { decision: "once" } },
        context("ses_dev_coordinator"),
      ),
    /project-bound/u,
  )
  assert.equal(f.calls.filter((call) => call.path.endsWith("/reply")).length, 0)
})

await test("a canceled native call or stale coordinator cannot forward after user confirmation", async () => {
  const stateDuringAsk = {
    ...state,
    tasks: { task: { ...state.tasks.task, role: "coordinator" } },
    decisions: {
      approval: {
        id: "approval",
        taskId: "task",
        attemptId: "attempt",
        kind: "permission",
        status: "pending",
      },
    },
  }
  const f = fixture(stateDuringAsk, () => {
    stateDuringAsk.tasks.task.attemptId = "replacement"
    return Promise.resolve()
  })
  await assert.rejects(
    f
      .tool("decision")
      .execute({ operationId: "op", decisionId: "approval", answer: { decision: "once" } }, context()),
    /not the current/u,
  )
  assert.equal(f.calls.filter((call) => call.path.endsWith("/reply")).length, 0)
  const signal = new AbortController()
  const canceled = fixture(
    {
      ...state,
      tasks: { task: { ...state.tasks.task, role: "coordinator" } },
      decisions: stateDuringAsk.decisions,
    },
    () => {
      signal.abort()
      return Promise.resolve()
    },
  )
  await assert.rejects(
    canceled
      .tool("decision")
      .execute(
        { operationId: "op", decisionId: "approval", answer: { decision: "once" } },
        { ...context(), signal: signal.signal },
      ),
  )
  assert.equal(canceled.calls.filter((call) => call.path.endsWith("/reply")).length, 0)
})

await test("changed request details cannot reuse an earlier native confirmation", async () => {
  const changed = {
    ...state,
    tasks: { task: { ...state.tasks.task, role: "coordinator" } },
    decisions: {
      approval: {
        id: "approval",
        taskId: "task",
        attemptId: "attempt",
        kind: "permission",
        status: "pending",
        details: { action: "shell", resources: ["original"] },
      },
    },
  }
  const f = fixture(changed, () => {
    changed.decisions.approval.details.resources = ["different"]
    return Promise.resolve()
  })
  await assert.rejects(
    f
      .tool("decision")
      .execute({ operationId: "op", decisionId: "approval", answer: { decision: "once" } }, context()),
    /Decision changed/u,
  )
  assert.equal(f.calls.filter((call) => call.path.endsWith("/reply")).length, 0)
})
