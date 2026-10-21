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
import { projectSnapshot, records, scopedTask } from "./scope.ts"
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

function dependencies(value: unknown, state: Snapshot, projectId: string): string[] | undefined {
  if (value === undefined) return undefined
  if (
    !Array.isArray(value) ||
    value.some((id: unknown) => typeof id !== "string" || state.tasks[id]?.projectId !== projectId)
  )
    throw new Error("Dependencies must belong to the proposed task's project")
  return value as string[]
}

function proposalParent(input: Record<string, unknown>, state: Snapshot, projectId: string, owner?: Task) {
  if (owner && projectId !== owner.projectId) throw new Error("Worker proposals must use their own project")
  if (owner && input.parentId !== undefined && input.parentId !== owner.id)
    throw new Error("Worker proposals must use their current task as parent")
  const parentId = owner?.id ?? input.parentId
  if (parentId !== undefined && state.tasks[text(parentId, "parentId")]?.projectId !== projectId)
    throw new Error("Parent must belong to the proposed task's project")
  return parentId
}

function proposal(input: Record<string, unknown>, state: Snapshot, owner?: Task) {
  const projectId = text(input.projectId, "projectId")
  const parentId = proposalParent(input, state, projectId, owner)
  if (owner && owner.role !== "coordinator" && input.role !== undefined && input.role !== "worker")
    throw new Error("Ordinary workers cannot propose coordinator privileges")
  if (input.role !== undefined && input.role !== "worker" && input.role !== "coordinator")
    throw new Error("Task role must be worker or coordinator")
  const dependencyIDs = dependencies(input.dependencies, state, projectId)
  return {
    projectId,
    title: text(input.title, "title"),
    instructions: text(input.instructions, "instructions"),
    ...(parentId === undefined ? {} : { parentId }),
    ...(input.role === undefined ? {} : { role: input.role }),
    ...(dependencyIDs === undefined ? {} : { dependencies: dependencyIDs }),
  }
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
  if (!latest.owned || latest.owned.task.role !== "coordinator")
    throw new Error("Coordinator authority changed while awaiting user confirmation")
  scopedTask(latest.state, taskId, latest.owned.task)
  const current = decisionRequest(id, records(latest.state.raw.decisions)[id])
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
            proposal(input, state, owned?.task),
            operationID(input.operationId),
          ),
        )
      },
    },
    {
      name: "status",
      description: "Read Tandem's durable snapshot. Idle native sessions are not completion evidence.",
      input: schema({}),
      options,
      async execute(_input, context) {
        const { state, owned } = await authority(client, native, context)
        return output(owned ? projectSnapshot(state, owned.task.projectId) : state.raw)
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
        const target = scopedTask(state, text(input.taskId, "taskId"), owned?.task)
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
        "Reply within the owned project. Ordinary workers can answer only their own planning forms, never permissions. Coordinators must obtain fresh native user confirmation before forwarding any permission answer.",
      input: schema({ operationId, decisionId: string, answer: { type: "object" } }, [
        "operationId",
        "decisionId",
        "answer",
      ]),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const { state, owned } = await authority(client, native, context)
        if (!owned) throw new Error("Decision replies require a current project-bound Tandem session")
        const id = text(input.decisionId, "decisionId")
        const decision = decisionRequest(id, records(state.raw.decisions)[id])
        if (decision.status !== "pending") throw new Error("Decision is not pending")
        scopedTask(state, decision.taskId, owned.task)
        const answer = structuredClone(object(input.answer))
        const operation = operationID(input.operationId)
        if (decision.kind === "permission") {
          if (owned.task.role !== "coordinator")
            throw new Error("Ordinary workers cannot answer permission requests")
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
  ]
}
