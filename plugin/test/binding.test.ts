import assert from "node:assert/strict"
import { mkdir, mkdtemp, realpath, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { test } from "node:test"
import type { ToolContext } from "@opencode/plugin/promise/tool"
import type { NativeAccess, NativeSession } from "../src/authority.ts"
import { TandemClient } from "../src/client.ts"
import { validateCoordinatorDirectory } from "../src/credentials.ts"
import { permissionGuard, toolGuard } from "../src/guards.ts"
import { tools } from "../src/tools.ts"

function context(sessionID: string) {
  return {
    sessionID: sessionID as ToolContext["sessionID"],
    signal: new AbortController().signal,
    agent: "build" as ToolContext["agent"],
    messageID: "msg_binding" as ToolContext["messageID"],
    id: "call_binding" as ToolContext["id"],
  }
}

async function fixture(optIn = false) {
  const root = await mkdtemp(path.join(await realpath(tmpdir()), "tandem-binding-test-"))
  const hub = path.join(root, "hub")
  const workspace = path.join(hub, "managed-project")
  const descendant = path.join(hub, "unregistered-project")
  const other = path.join(root, "other-project")
  await mkdir(hub)
  await Promise.all([mkdir(workspace), mkdir(descendant), mkdir(other)])
  const sessions = new Map<string, NativeSession>([
    ["ses_hub", { id: "ses_hub", metadata: {}, location: { directory: hub } }],
    ["ses_child", { id: "ses_child", metadata: {}, location: { directory: descendant } }],
    [
      "ses_managed",
      {
        id: "ses_managed",
        metadata: {
          tandem: {
            instanceId: "instance",
            taskId: "task",
            attemptId: "attempt",
            directory: workspace,
            role: "worker",
          },
        },
        location: { directory: workspace },
      },
    ],
  ])
  const state = {
    instanceId: "instance",
    tasks: {
      task: {
        id: "task",
        projectId: "project",
        attemptId: "attempt",
        role: "worker",
        state: "running",
        generation: 1,
      },
      child: { id: "child", projectId: "project", role: "worker", state: "running", parentId: "task" },
      peer: { id: "peer", projectId: "project", role: "worker", state: "running" },
      foreign: {
        id: "foreign",
        projectId: "foreign-project",
        role: "worker",
        state: "running",
        instructions: "FOREIGN PRIVATE INSTRUCTIONS",
      },
    },
    attempts: {
      attempt: { id: "attempt", taskId: "task", sessionRef: "ses_managed", workspace: { path: workspace } },
    },
    decisions: {
      own: { id: "own", taskId: "task", attemptId: "attempt", kind: "form", status: "pending" },
      child: { id: "child", taskId: "child", attemptId: "child-attempt", kind: "form", status: "pending" },
      peer: { id: "peer", taskId: "peer", attemptId: "peer-attempt", kind: "form", status: "pending" },
      permission: {
        id: "permission",
        taskId: "child",
        attemptId: "child-attempt",
        kind: "permission",
        status: "pending",
        details: { id: "per_original", action: "shell", resources: ["fixture-command-not-executed"] },
      },
    },
  }
  const calls: { path: string; body: unknown; operation: string | null }[] = []
  let down = false
  const request: typeof fetch = (input, options) => {
    const url = input instanceof Request ? new URL(input.url) : new URL(input)
    calls.push({
      path: url.pathname,
      body: typeof options?.body === "string" ? (JSON.parse(options.body) as unknown) : undefined,
      operation: new Headers(options?.headers).get("X-Operation-ID"),
    })
    if (down) return Promise.reject(new Error("Tandem unavailable"))
    return Promise.resolve(Response.json(url.pathname === "/v1/snapshot" ? state : { ok: true }))
  }
  const client = new TandemClient("http://127.0.0.1:7777", "owned-fixture-token", request)
  const confirmations: Parameters<NativeAccess["assert"]>[0][] = []
  const native: NativeAccess = {
    session: (id) => {
      const session = sessions.get(id)
      if (!session) return Promise.reject(new Error("Missing fixture native session"))
      return Promise.resolve(session)
    },
    assert: async (input) => {
      const event = {
        sessionID: input.sessionID as ToolContext["sessionID"],
        action: input.action,
        resources: input.resources,
        effect: "allow" as "allow" | "ask" | "deny",
      }
      await permissionGuard(native.session, undefined, undefined, native.coordinatorDirectory)(event)
      assert.equal(event.effect, "ask", "forwarding must wait for a distinct explicit native confirmation")
      confirmations.push(input)
    },
    ...(optIn ? { coordinatorDirectory: await validateCoordinatorDirectory(hub) } : {}),
  }
  const registered = tools(client, native)
  async function call(name: string, input: unknown, sessionID: string, hook = true) {
    const tool = registered.find((value) => value.name === name)
    if (!tool) throw new Error("Missing fixture tool")
    if (hook)
      await toolGuard(
        native.session,
        undefined,
        undefined,
        native.coordinatorDirectory,
      )({ tool: `tandem_${name}`, sessionID, input })
    return tool.execute(input, context(sessionID))
  }
  function setManaged(role: "worker" | "coordinator", directory = workspace, actual = directory) {
    sessions.set("ses_managed", {
      id: "ses_managed",
      metadata: { tandem: { instanceId: "instance", taskId: "task", attemptId: "attempt", directory, role } },
      location: { directory: actual },
    })
  }
  return {
    root,
    hub,
    workspace,
    descendant,
    other,
    state,
    native,
    sessions,
    calls,
    confirmations,
    call,
    setManaged,
    setDown: () => {
      down = true
    },
    cleanup: () => rm(root, { recursive: true, force: true }),
  }
}

await test("managed-only defaults and exact hub opt-in deny descendant tools before a snapshot fetch", async () => {
  for (const optIn of [false, true]) {
    const f = await fixture(optIn)
    try {
      const unauthorized = optIn ? "ses_child" : "ses_hub"
      for (const hook of [true, false]) {
        await assert.rejects(
          f.call("status", {}, unauthorized, hook),
          /explicitly configured coordinator directory/u,
        )
        await assert.rejects(
          f.call(
            "create",
            { operationId: "root", projectId: "project", title: "root", instructions: "work" },
            unauthorized,
            hook,
          ),
        )
      }
      assert.equal(f.calls.length, 0)
      if (optIn) {
        const global = await f.call("status", {}, "ses_hub")
        assert.ok(global.content.includes("FOREIGN PRIVATE INSTRUCTIONS"))
        const scoped = await f.call("status", {}, "ses_managed")
        assert.ok(!scoped.content.includes("FOREIGN PRIVATE INSTRUCTIONS"))
      }
    } finally {
      await f.cleanup()
    }
  }
})

await test("API bindings reject moved locations and persisted workspace/role collisions; local work survives service downtime", async () => {
  const f = await fixture(true)
  try {
    f.setManaged("worker", f.workspace, f.other)
    await assert.rejects(f.call("status", {}, "ses_managed", false), /immutable assigned directory/u)
    await assert.rejects(
      toolGuard(f.native.session)({ tool: "read", sessionID: "ses_managed", input: { path: "private.txt" } }),
      /moved/u,
    )
    assert.equal(f.calls.length, 0)
    f.setManaged("worker", f.other)
    await assert.rejects(f.call("status", {}, "ses_managed", false), /does not match/u)
    f.setManaged("coordinator")
    await assert.rejects(f.call("status", {}, "ses_managed", false), /does not match/u)
    f.setManaged("worker")
    f.setDown()
    await assert.rejects(f.call("status", {}, "ses_managed"), /request failed/u)
    const prior = f.calls.length
    await toolGuard(f.native.session)({
      tool: "write",
      sessionID: "ses_managed",
      input: { path: "offline-file.go" },
    })
    assert.equal(f.calls.length, prior, "local file guarding must not fetch Tandem state")
    // Even a hub location cannot turn stale/foreign managed metadata into global authority.
    f.setManaged("worker", f.hub)
    await assert.rejects(f.call("status", {}, "ses_managed", false))
  } finally {
    await f.cleanup()
  }
})

await test("controls preserve wire format and scope for coordinators/hub, never ordinary workers", async () => {
  const f = await fixture(true)
  try {
    const input = { operationId: "control-worker", taskId: "task", action: "pause", cascade: false }
    await assert.rejects(f.call("control", input, "ses_managed"), /Ordinary workers/u)
    await assert.rejects(f.call("control", input, "ses_managed", false), /Only managed coordinators/u)
    assert.equal(f.calls.filter((call) => call.path.endsWith("/control")).length, 0)
    f.state.tasks.task.role = "coordinator"
    f.setManaged("coordinator")
    for (const [index, action] of ["pause", "resume", "cancel", "retry"].entries()) {
      await f.call(
        "control",
        { operationId: `control-${String(index)}`, taskId: "child", action, cascade: index === 2 },
        "ses_managed",
      )
      assert.deepEqual(f.calls.at(-1), {
        path: "/v1/tasks/child/control",
        body: { action, cascade: index === 2 },
        operation: `control-${String(index)}`,
      })
    }
    await assert.rejects(f.call("control", { ...input, taskId: "foreign" }, "ses_managed"), /owned project/u)
    await f.call("control", { ...input, operationId: "hub-control", taskId: "foreign" }, "ses_hub")
    assert.equal(f.calls.at(-1)?.path, "/v1/tasks/foreign/control")
    for (const invalid of [
      { ...input, action: "accept" },
      { ...input, cascade: undefined },
    ])
      await assert.rejects(f.call("control", invalid, "ses_hub"))
  } finally {
    await f.cleanup()
  }
})

await test("workers cannot self-answer forms; coordinators answer only child forms; opted-in hub confirms permissions", async () => {
  const f = await fixture(true)
  try {
    const input = { operationId: "form", decisionId: "own", answer: { answer: { choice: "yes" } } }
    await assert.rejects(f.call("decision", input, "ses_managed"), /Ordinary workers/u)
    await assert.rejects(f.call("decision", input, "ses_managed", false), /Only managed coordinators/u)
    f.state.tasks.task.role = "coordinator"
    f.setManaged("coordinator")
    await assert.rejects(f.call("decision", input, "ses_managed"), /own user questions/u)
    await assert.rejects(
      f.call("decision", { ...input, decisionId: "peer" }, "ses_managed"),
      /only child forms/u,
    )
    await f.call("decision", { ...input, decisionId: "child" }, "ses_managed")
    assert.equal(f.calls.at(-1)?.path, "/v1/decisions/child/reply")
    assert.equal(f.confirmations.length, 0)
    await f.call(
      "decision",
      { operationId: "hub-approval", decisionId: "permission", answer: { decision: "once" } },
      "ses_hub",
    )
    assert.equal(f.confirmations.length, 1)
    assert.deepEqual(f.confirmations[0]?.save, [])
    assert.deepEqual(f.confirmations[0].metadata?.request, f.state.decisions.permission.details)
    assert.equal(f.calls.at(-1)?.path, "/v1/decisions/permission/reply")
  } finally {
    await f.cleanup()
  }
})
