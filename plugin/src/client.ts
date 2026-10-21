export interface Task {
  id: string
  projectId: string
  attemptId?: string
  state: string
}

export interface Attempt {
  id: string
  taskId: string
  sessionRef: string
}

export interface Snapshot {
  instanceId: string
  tasks: Record<string, Task>
  attempts: Record<string, Attempt>
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

function snapshot(value: unknown): Snapshot {
  const data = object(value)
  const tasks: Record<string, Task> = Object.create(null) as Record<string, Task>
  const attempts: Record<string, Attempt> = Object.create(null) as Record<string, Attempt>
  for (const [key, value] of Object.entries(object(data.tasks))) {
    const task = object(value)
    const id = text(task.id, "task.id")
    if (id !== key) throw new Error("Invalid Tandem task map")
    tasks[key] = {
      id,
      projectId: text(task.projectId, "task.projectId"),
      state: text(task.state, "task.state"),
      ...(task.attemptId ? { attemptId: text(task.attemptId, "task.attemptId") } : {}),
    }
  }
  for (const [key, value] of Object.entries(object(data.attempts))) {
    const attempt = object(value)
    const id = text(attempt.id, "attempt.id")
    if (id !== key) throw new Error("Invalid Tandem attempt map")
    attempts[key] = {
      id,
      taskId: text(attempt.taskId, "attempt.taskId"),
      sessionRef: text(attempt.sessionRef, "attempt.sessionRef"),
    }
  }
  return { instanceId: text(data.instanceId, "instanceId"), tasks, attempts }
}

export class TandemClient {
  readonly #url: URL
  readonly #token: string
  readonly #fetch: typeof fetch

  constructor(url: string, token: string, request: typeof fetch = fetch) {
    this.#url = new URL(url)
    if (
      !["http:", "https:"].includes(this.#url.protocol) ||
      this.#url.username ||
      this.#url.password ||
      this.#url.search ||
      this.#url.hash ||
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
      if (!operationId || operationId.length > 200 || /[\r\n]/u.test(operationId)) {
        throw new Error("Mutations require a stable operationId (at most 200 characters)")
      }
      headers["X-Operation-ID"] = operationId
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
