import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { object, text, TandemClient, type ReadRecord } from "../src/client.ts"
import type { Context, NativeAccess } from "../src/authority.ts"
import { tools } from "../src/tools.ts"

let input = ""
process.stdin.setEncoding("utf8")
for await (const chunk of process.stdin) input += String(chunk)
const fixture = JSON.parse(input) as {
  url: string
  token: string
  task: { id: string; projectId: string; role: string }
  attempt: { id: string; sessionRef: string; workspace: { path: string } }
  text: string
  recover?: boolean
  expected?: Record<string, unknown>
  receipts?: Record<string, unknown>
  childId?: string
  decisionId?: string
}
const client = new TandemClient(fixture.url, fixture.token)
const signal = new AbortController().signal
const native: NativeAccess = {
  async session() {
    return {
      id: fixture.attempt.sessionRef,
      metadata: {
        tandem: {
          instanceId: text((await client.manifest(signal)).instanceId, "instanceId"),
          taskId: fixture.task.id,
          attemptId: fixture.attempt.id,
          directory: fixture.attempt.workspace.path,
          role: fixture.task.role,
        },
      },
      location: { directory: fixture.attempt.workspace.path },
    }
  },
  assert() {
    return Promise.reject(new Error("No permission forwarding authorized"))
  },
}
const status = tools(client, native).find((tool) => tool.name === "status")
assert.ok(status)
const context: Context = {
  sessionID: fixture.attempt.sessionRef as Context["sessionID"],
  signal,
  agent: "build" as Context["agent"],
  messageID: "msg_fixture" as Context["messageID"],
  id: "call_fixture" as Context["id"],
}
const read = async (args: object): Promise<Record<string, unknown>> =>
  object(JSON.parse((await status.execute(args, context)).content))
const invoke = async (name: string, args: object) => {
  const tool = tools(client, native).find((tool) => tool.name === name)
  assert.ok(tool)
  return object(JSON.parse((await tool.execute(args, context)).content))
}
let reference: ReadRecord
if (!fixture.recover) {
  const receipt = await invoke("followup", {
    taskId: fixture.task.id,
    text: fixture.text,
    operationId: "plugin-followup",
  })
  assert.equal(receipt.representation, "operation-receipt")
  reference = receipt.record as ReadRecord
  await read({ contentCursor: reference.content.cursor })
} else {
  const page = await read({ collection: "receipts", key: "plugin-followup" })
  const first = (page.records as ReadRecord[])[0]
  assert.ok(first)
  reference = first
}
let cursor = reference.content.cursor
let content = ""
do {
  const page = await read({ contentCursor: cursor })
  assert.equal(page.offset, Buffer.byteLength(content))
  assert.ok(Buffer.byteLength(text(page.text, "content")) <= 32768)
  content += text(page.text, "content")
  cursor = page.nextCursor === undefined ? "" : text(page.nextCursor, "nextCursor")
} while (cursor)
assert.equal(Buffer.byteLength(content), reference.content.bytes)
assert.equal(createHash("sha256").update(content).digest("hex"), reference.content.sha256)
const receipt = object(JSON.parse(content))
if (fixture.expected) assert.deepEqual(receipt, fixture.expected)
else assert.ok(text(receipt.instructions, "instructions").endsWith(fixture.text))
for (const [key, expected] of Object.entries(fixture.receipts ?? {})) {
  const records = (await read({ collection: "receipts", key })).records as ReadRecord[]
  assert.equal(records.length, 1)
  const first = records[0]
  assert.ok(first)
  let next = first.content.cursor
  let exact = ""
  do {
    const page = await read({ contentCursor: next })
    exact += text(page.text, "content")
    next = page.nextCursor === undefined ? "" : text(page.nextCursor, "nextCursor")
  } while (next)
  assert.deepEqual(JSON.parse(exact), expected)
}
await assert.rejects(read({ collection: "receipts" }))
const manifest = await client.manifest(signal)
const state = { instanceId: text(manifest.instanceId, "instanceId"), raw: manifest, tasks: {}, attempts: {} }
await assert.rejects(
  client.page(state, "receipts", signal, { key: "plugin-followup", projectId: "foreign-project" }),
)
await assert.rejects(client.content(state, reference.content.cursor, signal, "foreign-project"))
if (!fixture.recover) {
  const created = await invoke("create", {
    projectId: fixture.task.projectId,
    title: "Plugin-created child",
    instructions: "Bounded task",
    operationId: "plugin-create",
  })
  assert.equal(created.projectId, fixture.task.projectId)
  const consume = async (value: Record<string, unknown>) => {
    assert.equal(value.representation, "operation-receipt")
    const row = value.record as ReadRecord
    let next = row.content.cursor
    let complete = ""
    do {
      const page = await read({ contentCursor: next })
      assert.equal(page.offset, Buffer.byteLength(complete))
      complete += text(page.text, "content")
      next = page.nextCursor === undefined ? "" : text(page.nextCursor, "nextCursor")
    } while (next)
    assert.equal(Buffer.byteLength(complete), row.content.bytes)
    assert.equal(createHash("sha256").update(complete).digest("hex"), row.content.sha256)
    return object(JSON.parse(complete))
  }
  const current = await client.snapshot(signal, fixture.attempt.sessionRef)
  const currentTask = current.tasks[fixture.task.id]
  assert.ok(currentTask)
  const result = await consume(
    await invoke("result", {
      operationId: "plugin-result",
      generation: currentTask.generation,
      summary: "Independent fixture evidence",
      evidence: [{ name: "Unicode evidence", reference: fixture.text.repeat(3), passed: true }],
    }),
  )
  assert.deepEqual(result.evidence, [
    { name: "Unicode evidence", reference: fixture.text.repeat(3), passed: true },
  ])
  const decision = await consume(
    await invoke("decision", {
      operationId: "plugin-decision",
      decisionId: fixture.decisionId,
      answer: { answer: { field: "yes" } },
    }),
  )
  assert.equal(decision.status, "reply_pending")
  const control = await consume(
    await invoke("control", {
      operationId: "plugin-control",
      taskId: fixture.childId,
      action: "pause",
      cascade: false,
    }),
  )
  assert.equal(control.desired, "paused")
  process.stdout.write(
    JSON.stringify({
      "plugin-create": created,
      "plugin-followup": receipt,
      "plugin-result": result,
      "plugin-decision": decision,
      "plugin-control": control,
    }),
  )
}
