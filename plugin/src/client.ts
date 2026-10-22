export interface Task extends Record<string, unknown> {
  id: string
  projectId: string
  attemptId?: string
  state: string
  role: string
  // Informational snapshot value, never a default for submitted evidence.
  generation?: number
}

export interface Evidence {
  name: string
  reference: string
  passed: boolean
}

export interface SubmitResult {
  attemptId: string
  generation: number
  summary: string
  evidence: Evidence[]
}

export interface Attempt extends Record<string, unknown> {
  id: string
  taskId: string
  sessionRef: string
}

export interface Snapshot {
  instanceId: string
  tasks: Record<string, Task>
  attempts: Record<string, Attempt>
  raw: Record<string, unknown>
}

export interface ReadRecord {
  key: string
  summary: Record<string, unknown>
  content: { cursor: string; bytes: number; sha256: string }
}

export interface ReadPage {
  instanceId: string
  revision: number
  collection: string
  records: ReadRecord[]
  nextCursor?: string
}

export const operationPattern = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/u

export function operationID(value: unknown): string {
  if (typeof value !== "string" || !operationPattern.test(value)) {
    throw new Error("Mutations require a stable operationId matching [A-Za-z0-9][A-Za-z0-9._:-]{0,127}")
  }
  return value
}

export function object(value: unknown): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("Expected a JSON object")
  }
  return value as Record<string, unknown>
}

export function text(value: unknown, name: string): string {
  if (typeof value !== "string" || value.length === 0) throw new Error(`Missing ${name}`)
  return value
}

async function boundedJSON(response: Response, limit: number): Promise<unknown> {
  const reader = response.body?.getReader()
  if (!reader) throw new Error("Missing response body")
  const decoder = new TextDecoder("utf-8", { fatal: true })
  let size = 0
  let data = ""
  try {
    for (;;) {
      const part = await reader.read()
      if (part.done) break
      size += part.value.byteLength
      if (size > limit) throw new Error("Tandem response exceeds its byte budget")
      data += decoder.decode(part.value, { stream: true })
    }
    data += decoder.decode()
  } finally {
    await reader.cancel()
  }
  return JSON.parse(data) as unknown
}

function taskGeneration(value: unknown): number | undefined {
  if (value === undefined) return undefined
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1)
    throw new Error("Invalid snapshot task generation")
  return value
}

function snapshot(value: unknown): Snapshot {
  const data = object(value)
  const tasks: Record<string, Task> = Object.create(null) as Record<string, Task>
  const attempts: Record<string, Attempt> = Object.create(null) as Record<string, Attempt>
  for (const [key, value] of Object.entries(object(data.tasks))) {
    const task = object(value)
    const id = text(task.id, "task.id")
    const generation = taskGeneration(task.generation)
    if (id !== key) throw new Error("Invalid Tandem task map")
    tasks[key] = {
      ...task,
      id,
      projectId: text(task.projectId, "task.projectId"),
      state: text(task.state, "task.state"),
      role: typeof task.role === "string" ? task.role : "worker",
      ...(generation === undefined ? {} : { generation }),
      ...(task.attemptId ? { attemptId: text(task.attemptId, "task.attemptId") } : {}),
    }
  }
  for (const [key, value] of Object.entries(object(data.attempts))) {
    const attempt = object(value)
    const id = text(attempt.id, "attempt.id")
    if (id !== key) throw new Error("Invalid Tandem attempt map")
    attempts[key] = {
      ...attempt,
      id,
      taskId: text(attempt.taskId, "attempt.taskId"),
      sessionRef: typeof attempt.sessionRef === "string" ? attempt.sessionRef : "",
    }
  }
  return { instanceId: text(data.instanceId, "instanceId"), tasks, attempts, raw: data }
}

export class TandemClient {
  readonly #url: URL
  readonly #token: string
  readonly #fetch: typeof fetch

  constructor(url: string, token: string, request: typeof fetch = fetch) {
    try {
      this.#url = new URL(url)
    } catch {
      throw new Error("Invalid explicit Tandem HTTP(S) URL")
    }
    if (
      !["http:", "https:"].includes(this.#url.protocol) ||
      this.#url.username ||
      this.#url.password ||
      this.#url.href.includes("?") ||
      this.#url.href.includes("#") ||
      !token
    ) {
      throw new Error("Tandem requires an HTTP(S) URL without embedded credentials and a token")
    }
    if (this.#url.protocol === "http:" && !["127.0.0.1", "localhost", "[::1]"].includes(this.#url.hostname)) {
      throw new Error("Non-loopback Tandem connections require HTTPS")
    }
    this.#token = token
    this.#fetch = request
  }

  async request(path: string, signal: AbortSignal, body?: unknown, operationId?: string): Promise<unknown> {
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.#token}`,
      Accept: "application/json",
    }
    if (body !== undefined) {
      headers["X-Operation-ID"] = operationID(operationId)
      headers["Content-Type"] = "application/json"
      headers.Prefer = "return=bounded"
    }
    const url = `${this.#url.href.replace(/\/$/u, "")}${path}`
    let response: Response
    try {
      response = await this.#fetch(url, {
        method: body === undefined ? "GET" : "POST",
        headers,
        signal: AbortSignal.any([signal, AbortSignal.timeout(15_000)]),
        redirect: "error",
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      })
    } catch {
      throw new Error(
        body === undefined
          ? "Tandem request failed"
          : "Tandem outcome uncertain; reconcile or retry with the SAME operationId and payload",
      )
    }
    if (!response.ok)
      throw new Error(`Tandem HTTP ${String(response.status)}; preserve the operationId on retries`)
    if (response.status === 204) return null
    try {
      const limit = path.startsWith("/v1/reads/") ? 256 * 1024 : 8 * 1024 * 1024
      return await boundedJSON(response, limit)
    } catch {
      throw new Error(
        body === undefined
          ? "Invalid Tandem JSON response"
          : "Tandem outcome uncertain after invalid JSON; preserve the SAME operationId and payload",
      )
    }
  }

  async manifest(signal: AbortSignal): Promise<Record<string, unknown>> {
    const value = object(await this.request("/v1/reads/snapshot", signal))
    text(value.instanceId, "instanceId")
    if (!Number.isSafeInteger(value.revision) || Number(value.revision) < 0 || value.maxPageBytes !== 262144)
      throw new Error("Invalid bounded read manifest")
    return value
  }

  async page(
    state: Snapshot,
    collection: string,
    signal: AbortSignal,
    filters: Record<string, string> = {},
  ): Promise<ReadPage> {
    const query = new URLSearchParams({
      collection,
      instanceId: state.instanceId,
      revision: String(state.raw.revision),
      ...filters,
    })
    const data = object(await this.request(`/v1/reads/records?${query.toString()}`, signal))
    if (
      data.instanceId !== state.instanceId ||
      data.revision !== state.raw.revision ||
      data.collection !== collection ||
      !Array.isArray(data.records) ||
      data.records.length > 64
    )
      throw new Error("Invalid or mixed-revision record page")
    const records = data.records.map((value: unknown): ReadRecord => {
      const row = object(value)
      const content = object(row.content)
      if (!Number.isSafeInteger(content.bytes) || Number(content.bytes) < 1)
        throw new Error("Invalid content length")
      return {
        key: text(row.key, "record key"),
        summary: object(row.summary),
        content: {
          cursor: text(content.cursor, "content cursor"),
          bytes: Number(content.bytes),
          sha256: text(content.sha256, "content checksum"),
        },
      }
    })
    return {
      instanceId: state.instanceId,
      revision: Number(state.raw.revision),
      collection,
      records,
      ...(data.nextCursor ? { nextCursor: text(data.nextCursor, "nextCursor") } : {}),
    }
  }

  async record(state: Snapshot, collection: string, key: string, signal: AbortSignal): Promise<ReadRecord> {
    const page = await this.page(state, collection, signal, { key, limit: "1" })
    const record = page.records[0]
    if (!record || record.key !== key || page.records.length !== 1 || page.nextCursor)
      throw new Error("Record is not available")
    return record
  }

  async task(state: Snapshot, id: string, signal: AbortSignal): Promise<Task> {
    const row = (await this.record(state, "tasks", id, signal)).summary
    const task = snapshot({ instanceId: state.instanceId, tasks: { [id]: row }, attempts: {} }).tasks[id]
    if (!task) throw new Error("Task is not available")
    return task
  }

  async content(
    state: Snapshot,
    cursor: string,
    signal: AbortSignal,
    projectId?: string,
  ): Promise<Record<string, unknown>> {
    const query = new URLSearchParams({ cursor, ...(projectId === undefined ? {} : { projectId }) })
    const page = object(await this.request(`/v1/reads/content?${query.toString()}`, signal))
    if (
      page.instanceId !== state.instanceId ||
      page.revision !== state.raw.revision ||
      typeof page.text !== "string" ||
      new TextEncoder().encode(page.text).length > 32768
    )
      throw new Error("Invalid or mixed-revision content page")
    return page
  }

  async jsonRecord(
    state: Snapshot,
    collection: string,
    key: string,
    signal: AbortSignal,
  ): Promise<Record<string, unknown>> {
    const record = await this.record(state, collection, key, signal)
    if (record.content.bytes > 8 * 1024 * 1024)
      throw new Error(
        "Record exceeds the tool's JSON budget; inspect bounded status content pages and answer through the native user interface",
      )
    let cursor = record.content.cursor
    let offset = 0
    let data = ""
    while (cursor) {
      const page = await this.content(state, cursor, signal)
      if (
        page.key !== key ||
        page.offset !== offset ||
        page.totalBytes !== record.content.bytes ||
        page.sha256 !== record.content.sha256 ||
        typeof page.text !== "string" ||
        page.text === "" ||
        page.nextCursor === cursor
      )
        throw new Error("Invalid content traversal")
      data += page.text
      offset += new TextEncoder().encode(page.text).length
      if (offset > record.content.bytes) throw new Error("Content exceeds its declared length")
      cursor = page.nextCursor === undefined ? "" : text(page.nextCursor, "nextCursor")
    }
    const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(data))
    const checksum = Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")
    if (offset !== record.content.bytes || checksum !== record.content.sha256)
      throw new Error("Incomplete or damaged content")
    return object(JSON.parse(data))
  }

  async snapshot(signal: AbortSignal, sessionId: string): Promise<Snapshot> {
    const manifest = await this.manifest(signal)
    const state: Snapshot = {
      instanceId: text(manifest.instanceId, "instanceId"),
      tasks: {},
      attempts: {},
      raw: manifest,
    }
    // At most two rows are needed to reject ambiguous ownership. Do not build a
    // global inventory, even when the configured task count is very large.
    const page = await this.page(state, "attempts", signal, { sessionRef: sessionId, limit: "2" })
    if (page.records.length > 1 || page.nextCursor) throw new Error("Ambiguous native session ownership")
    const row = page.records[0]
    if (row) {
      const attempt = object(row.summary)
      if (attempt.id !== row.key) throw new Error("Invalid Tandem attempt map")
      state.attempts[row.key] = {
        ...attempt,
        id: row.key,
        taskId: text(attempt.taskId, "attempt.taskId"),
        sessionRef: text(attempt.sessionRef, "attempt.sessionRef"),
        workspace: { path: text(attempt.workspacePath, "workspacePath") },
      }
      const task = await this.task(state, text(attempt.taskId, "attempt.taskId"), signal)
      state.tasks[task.id] = task
    }
    return state
  }
}

export function currentWorker(
  state: Snapshot,
  sessionId: string,
): { task: Task; attempt: Attempt } | undefined {
  const matching = Object.values(state.attempts).filter((attempt) => attempt.sessionRef === sessionId)
  if (matching.length === 0) return undefined
  if (matching.length !== 1) throw new Error("Ambiguous native session ownership")
  const attempt = matching[0]
  if (!attempt) throw new Error("Missing worker attempt")
  const task = state.tasks[attempt.taskId]
  if (!task || task.attemptId !== attempt.id)
    throw new Error("Native session is not the current task attempt")
  return { task, attempt }
}
