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
      return (await response.json()) as unknown
    } catch {
      throw new Error(
        body === undefined
          ? "Invalid Tandem JSON response"
          : "Tandem outcome uncertain after invalid JSON; preserve the SAME operationId and payload",
      )
    }
  }

  async snapshot(signal: AbortSignal): Promise<Snapshot> {
    return snapshot(await this.request("/v1/snapshot", signal))
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
