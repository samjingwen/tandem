import assert from "node:assert/strict"
import { test } from "node:test"
import { TandemClient, type ReadPage, type Snapshot } from "../src/client.ts"
import { readFixture } from "./read-fixture.ts"

await test("bounded client traverses projected inventory and exact Unicode content without a global snapshot", async () => {
  const state = {
    instanceId: "read-fixture",
    revision: 7,
    tasks: Object.fromEntries(
      Array.from({ length: 65 }, (_, index) => [
        String(index),
        {
          id: String(index),
          projectId: "project",
          state: "queued",
          role: "worker",
          title: `Task ${String(index)}`,
          instructions: "界".repeat(43690) + "ab",
        },
      ]),
    ),
  }
  const calls: string[] = []
  const request: typeof fetch = (input) => {
    const url = input instanceof Request ? new URL(input.url) : new URL(input)
    calls.push(url.pathname)
    return Promise.resolve(readFixture(state, url))
  }
  const client = new TandemClient("http://localhost:7000", "fixture-token", request)
  const view: Snapshot = {
    instanceId: state.instanceId,
    raw: await client.manifest(new AbortController().signal),
    tasks: {},
    attempts: {},
  }
  const signal = new AbortController().signal
  const seen = new Set<string>()
  let cursor = ""
  let first: ReadPage | undefined
  do {
    const page = await client.page(view, "tasks", signal, {
      projectId: "project",
      ...(cursor ? { cursor } : {}),
    })
    first ??= page
    assert.ok(page.records.length <= 32)
    assert.ok(Buffer.byteLength(JSON.stringify(page)) <= 262144)
    for (const row of page.records) {
      assert.ok(!seen.has(row.key))
      assert.equal(row.summary.instructions, undefined)
      seen.add(row.key)
    }
    cursor = page.nextCursor ?? ""
  } while (cursor)
  assert.equal(seen.size, 65)
  assert.equal(
    calls.filter((path) => path === "/v1/reads/content").length,
    0,
    "metadata traversal must not hydrate every instruction",
  )
  const record = first.records[0]
  assert.ok(record)
  const decoded = await client.jsonRecord(view, "tasks", record.key, signal)
  assert.equal(decoded.instructions, state.tasks[record.key]?.instructions)
  assert.ok(calls.filter((path) => path === "/v1/reads/content").length > 1)
  assert.ok(!calls.includes("/v1/snapshot"))
})

await test("bounded client rejects mixed revisions and content damage instead of reconstructing silently", async () => {
  const state = {
    instanceId: "fixture",
    revision: 1,
    tasks: { task: { id: "task", projectId: "project", state: "queued", instructions: "界".repeat(20000) } },
  }
  let damage = false
  const request: typeof fetch = async (input) => {
    const url = input instanceof Request ? new URL(input.url) : new URL(input)
    const response = readFixture(state, url)
    if (damage && url.pathname === "/v1/reads/content") {
      const page = (await response.json()) as Record<string, unknown>
      return Response.json({ ...page, text: "corrupted" })
    }
    return response
  }
  const client = new TandemClient("http://localhost:7000", "fixture-token", request)
  const signal = new AbortController().signal
  const view: Snapshot = {
    instanceId: state.instanceId,
    raw: await client.manifest(signal),
    tasks: {},
    attempts: {},
  }
  damage = true
  await assert.rejects(client.jsonRecord(view, "tasks", "task", signal), /content|Content/u)
  damage = false
  state.revision++
  await assert.rejects(client.page(view, "tasks", signal), /mixed-revision/u)
})

await test("bounded response reader cancels oversized streams before decoding them", async () => {
  let canceled = false
  const client = new TandemClient("http://localhost:7000", "fixture-token", () =>
    Promise.resolve(
      new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new Uint8Array(262145))
          },
          cancel() {
            canceled = true
          },
        }),
      ),
    ),
  )
  await assert.rejects(client.manifest(new AbortController().signal), /Invalid Tandem JSON/u)
  assert.equal(canceled, true)
})
