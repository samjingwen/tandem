import {
  object,
  operationID,
  TandemClient,
  text,
  type Evidence,
  type Snapshot,
  type SubmitResult,
  type Task,
} from "./client.ts"
import {
  authority,
  confirmPermissionReply,
  decisionRequest,
  type Context,
  type NativeAccess,
} from "./authority.ts"
import { scopedTask } from "./scope.ts"
import { isDeepStrictEqual } from "node:util"

interface ToolSpec {
  name: string
  description: string
  input: {
    type: "object"
    properties: Record<string, unknown>
    required: string[]
    additionalProperties: false
  }
  options: { namespace: string; codemode: true }
  execute(input: unknown, context: Context): Promise<{ content: string }>
}

const string = { type: "string", minLength: 1 }
const operationId = {
  ...string,
  maxLength: 128,
  pattern: "^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$",
  description: "Stable ID; reuse unchanged after an uncertain response",
}

function schema(properties: Record<string, unknown>, required: string[] = []) {
  return { type: "object" as const, properties, required, additionalProperties: false as const }
}

function output(value: unknown) {
  return { content: JSON.stringify(value) }
}

function generation(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 1)
    throw new Error(
      "Result requires an explicit integer generation >= 1 from the instructions being reported",
    )
  return value
}

function evidence(value: unknown): Evidence[] {
  if (!Array.isArray(value)) throw new Error("Result evidence must be an array")
  return value.map((item: unknown) => {
    const row = object(item)
    if (typeof row.passed !== "boolean") throw new Error("Evidence requires a boolean passed value")
    return {
      name: text(row.name, "evidence.name"),
      reference: text(row.reference, "evidence.reference"),
      passed: row.passed,
    }
  })
}

async function dependencies(
  client: TandemClient,
  value: unknown,
  state: Snapshot,
  projectId: string,
  signal: AbortSignal,
): Promise<string[] | undefined> {
  if (value === undefined) return undefined
  if (!Array.isArray(value) || value.some((id: unknown) => typeof id !== "string"))
    throw new Error("Dependencies must belong to the proposed task's project")
  for (const id of value as string[]) {
    if ((await client.task(state, id, signal)).projectId !== projectId)
      throw new Error("Dependencies must belong to the proposed task's project")
  }
  return value as string[]
}

async function proposalParent(
  client: TandemClient,
  input: Record<string, unknown>,
  state: Snapshot,
  projectId: string,
  signal: AbortSignal,
  owner?: Task,
) {
  if (owner && projectId !== owner.projectId) throw new Error("Worker proposals must use their own project")
  if (owner && input.parentId !== undefined && input.parentId !== owner.id)
    throw new Error("Worker proposals must use their current task as parent")
  const parentId = owner?.id ?? input.parentId
  if (
    parentId !== undefined &&
    (await client.task(state, text(parentId, "parentId"), signal)).projectId !== projectId
  )
    throw new Error("Parent must belong to the proposed task's project")
  return parentId
}

async function proposal(
  client: TandemClient,
  input: Record<string, unknown>,
  state: Snapshot,
  signal: AbortSignal,
  owner?: Task,
) {
  const projectId = text(input.projectId, "projectId")
  const parentId = await proposalParent(client, input, state, projectId, signal, owner)
  if (owner && owner.role !== "coordinator" && input.role !== undefined && input.role !== "worker")
    throw new Error("Ordinary workers cannot propose coordinator privileges")
  if (input.role !== undefined && input.role !== "worker" && input.role !== "coordinator")
    throw new Error("Task role must be worker or coordinator")
  const dependencyIDs = await dependencies(client, input.dependencies, state, projectId, signal)
  return {
    projectId,
    title: text(input.title, "title"),
    instructions: text(input.instructions, "instructions"),
    ...(parentId === undefined ? {} : { parentId }),
    ...(input.role === undefined ? {} : { role: input.role }),
    ...(dependencyIDs === undefined ? {} : { dependencies: dependencyIDs }),
  }
}

type Authority = Awaited<ReturnType<typeof authority>>

async function coordinatorTask(
  client: TandemClient,
  view: Authority,
  taskId: string,
  signal: AbortSignal,
): Promise<Task> {
  if (!view.hub && view.owned?.task.role !== "coordinator")
    throw new Error(
      "Only managed coordinators or the explicitly opted-in hub can control tasks or answer decisions",
    )
  return scopedTask(client, view.state, taskId, signal, view.owned?.task)
}

async function childTask(
  client: TandemClient,
  state: Snapshot,
  taskId: string,
  parentId: string,
  signal: AbortSignal,
): Promise<boolean> {
  const visited = new Set<string>()
  let current: Task | undefined = await client.task(state, taskId, signal)
  while (current && !visited.has(current.id)) {
    visited.add(current.id)
    if (current.parentId === parentId) return true
    current =
      typeof current.parentId === "string" ? await client.task(state, current.parentId, signal) : undefined
  }
  return false
}

function controlInput(input: Record<string, unknown>) {
  if (
    !["pause", "resume", "cancel", "retry"].includes(text(input.action, "control action")) ||
    typeof input.cascade !== "boolean"
  )
    throw new Error("Control requires pause, resume, cancel, or retry and an explicit cascade boolean")
  return { action: input.action, cascade: input.cascade }
}

async function recheckPermissionAuthority(
  client: TandemClient,
  native: NativeAccess,
  context: Context,
  id: string,
  taskId: string,
  attemptId: string,
  details: unknown,
) {
  const latest = await authority(client, native, context)
  await coordinatorTask(client, latest, taskId, context.signal)
  const current = decisionRequest(id, await client.jsonRecord(latest.state, "decisions", id, context.signal))
  if (
    current.status !== "pending" ||
    current.kind !== "permission" ||
    current.taskId !== taskId ||
    current.attemptId !== attemptId ||
    !isDeepStrictEqual(current.details, details)
  )
    throw new Error("Decision changed while awaiting user confirmation")
}

export function tools(client: TandemClient, native: NativeAccess): ToolSpec[] {
  const options = { namespace: "tandem", codemode: true as const }
  return [
    {
      name: "create",
      description:
        "Propose a persistent task. Worker proposals become children in their own project, not native subagents.",
      input: schema(
        {
          operationId,
          projectId: string,
          parentId: string,
          title: string,
          instructions: string,
          role: string,
          dependencies: { type: "array", items: string },
        },
        ["operationId", "projectId", "title", "instructions"],
      ),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const { state, owned } = await authority(client, native, context)
        return output(
          await client.request(
            "/v1/tasks",
            context.signal,
            await proposal(client, input, state, context.signal, owned?.task),
            operationID(input.operationId),
          ),
        )
      },
    },
    {
      name: "status",
      description:
        "Read one bounded revision-pinned record or content page. Receipts require an exact retained operation ID as key. Follow nextCursor explicitly; omitted record fields remain available through content.cursor. Idle native sessions are not completion evidence.",
      input: schema({
        collection: {
          type: "string",
          enum: ["projects", "tasks", "attempts", "commands", "decisions", "results", "receipts"],
        },
        cursor: string,
        contentCursor: string,
        key: string,
        taskId: string,
      }),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const { state, owned } = await authority(client, native, context)
        if (input.contentCursor !== undefined)
          return output(
            await client.content(
              state,
              text(input.contentCursor, "contentCursor"),
              context.signal,
              owned?.task.projectId,
            ),
          )
        const collection = input.collection === undefined ? "tasks" : text(input.collection, "collection")
        if (
          !["projects", "tasks", "attempts", "commands", "decisions", "results", "receipts"].includes(
            collection,
          )
        )
          throw new Error("Unsupported collection")
        const filters: Record<string, string> = owned ? { projectId: owned.task.projectId } : {}
        for (const key of ["cursor", "key", "taskId"])
          if (input[key] !== undefined) filters[key] = text(input[key], key)
        return output(await client.page(state, collection, context.signal, filters))
      },
    },
    {
      name: "followup",
      description: "Send durable follow-up instructions through Tandem. No direct native-session dispatch.",
      input: schema({ operationId, taskId: string, text: string }, ["operationId", "taskId", "text"]),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const { state, owned } = await authority(client, native, context)
        const target = await scopedTask(
          client,
          state,
          text(input.taskId, "taskId"),
          context.signal,
          owned?.task,
        )
        return output(
          await client.request(
            `/v1/tasks/${encodeURIComponent(target.id)}/followup`,
            context.signal,
            { text: text(input.text, "text") },
            operationID(input.operationId),
          ),
        )
      },
    },
    {
      name: "decision",
      description:
        "Coordinators may answer child forms, never their own user questions. The explicitly opted-in hub can route decisions. Forwarding any permission answer requires fresh native user confirmation. Ordinary workers cannot answer decisions.",
      input: schema({ operationId, decisionId: string, answer: { type: "object" } }, [
        "operationId",
        "decisionId",
        "answer",
      ]),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const view = await authority(client, native, context)
        const { state, owned } = view
        const id = text(input.decisionId, "decisionId")
        const decision = decisionRequest(id, await client.jsonRecord(state, "decisions", id, context.signal))
        if (decision.status !== "pending") throw new Error("Decision is not pending")
        await coordinatorTask(client, view, decision.taskId, context.signal)
        const answer = structuredClone(object(input.answer))
        const operation = operationID(input.operationId)
        if (decision.kind === "permission") {
          await confirmPermissionReply(native.assert, context, state.instanceId, decision, answer, operation)
          await recheckPermissionAuthority(
            client,
            native,
            context,
            id,
            decision.taskId,
            decision.attemptId,
            decision.details,
          )
        } else if (decision.kind !== "form") {
          throw new Error("Unsupported decision kind")
        } else if (
          owned &&
          (decision.taskId === owned.task.id ||
            !(await childTask(client, state, decision.taskId, owned.task.id, context.signal)))
        ) {
          throw new Error("Managed coordinators may answer only child forms, never their own user questions")
        }
        return output(
          await client.request(
            `/v1/decisions/${encodeURIComponent(id)}/reply`,
            context.signal,
            { answer },
            operation,
          ),
        )
      },
    },
    {
      name: "result",
      description:
        "Submit structured evidence only for this native session's current Tandem attempt. Does not accept, merge, or clean up.",
      input: schema(
        {
          operationId,
          generation: {
            type: "integer",
            minimum: 1,
            description:
              "Exact instruction generation whose evidence is being reported; never substitute a later snapshot generation",
          },
          summary: string,
          evidence: {
            type: "array",
            items: schema({ name: string, reference: string, passed: { type: "boolean" } }, [
              "name",
              "reference",
              "passed",
            ]),
          },
        },
        ["operationId", "generation", "summary", "evidence"],
      ),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const reportedGeneration = generation(input.generation)
        const { owned } = await authority(client, native, context)
        if (!owned) throw new Error("This native session is not a Tandem worker")
        const result: SubmitResult = {
          attemptId: owned.attempt.id,
          generation: reportedGeneration,
          summary: text(input.summary, "summary"),
          evidence: evidence(input.evidence),
        }
        return output(
          await client.request(
            `/v1/tasks/${encodeURIComponent(owned.task.id)}/results`,
            context.signal,
            result,
            operationID(input.operationId),
          ),
        )
      },
    },
    {
      name: "control",
      description:
        "Pause, resume, cancel, or retry a persistent task. Only managed coordinators in their own project or the explicitly opted-in hub may control tasks; cascade must be explicit.",
      input: schema(
        {
          operationId,
          taskId: string,
          action: { type: "string", enum: ["pause", "resume", "cancel", "retry"] },
          cascade: { type: "boolean" },
        },
        ["operationId", "taskId", "action", "cascade"],
      ),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const body = controlInput(input)
        const view = await authority(client, native, context)
        const target = await coordinatorTask(client, view, text(input.taskId, "taskId"), context.signal)
        return output(
          await client.request(
            `/v1/tasks/${encodeURIComponent(target.id)}/control`,
            context.signal,
            body,
            operationID(input.operationId),
          ),
        )
      },
    },
  ]
}
