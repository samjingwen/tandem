import type { ToolContext } from "@opencode/plugin/promise/tool"
import type { OpenCodeClient } from "@opencode/client"
import { currentWorker, object, TandemClient, text } from "./client.ts"

type NativeSession = Pick<Awaited<ReturnType<OpenCodeClient["session"]["get"]>>, "id" | "metadata">
type Context = Pick<ToolContext, "sessionID" | "signal">

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
  maxLength: 200,
  description: "Stable ID; reuse unchanged after an uncertain response",
}

function schema(properties: Record<string, unknown>, required: string[] = []) {
  return { type: "object" as const, properties, required, additionalProperties: false as const }
}

function output(value: unknown) {
  return { content: JSON.stringify(value) }
}

async function worker(client: TandemClient, context: Context) {
  const state = await client.snapshot(context.signal)
  return { state, owned: currentWorker(state, context.sessionID) }
}

export function tools(
  client: TandemClient,
  nativeSession: (sessionID: string, signal: AbortSignal) => Promise<NativeSession>,
): ToolSpec[] {
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
        const { owned } = await worker(client, context)
        const projectId = text(input.projectId, "projectId")
        if (owned && projectId !== owned.task.projectId)
          throw new Error("Worker proposals must use their own project")
        if (owned && input.parentId !== undefined && input.parentId !== owned.task.id) {
          throw new Error("Worker proposals must use their current task as parent")
        }
        const parentId = owned?.task.id ?? input.parentId
        return output(
          await client.request(
            "/v1/tasks",
            context.signal,
            {
              projectId,
              title: text(input.title, "title"),
              instructions: text(input.instructions, "instructions"),
              ...(parentId === undefined ? {} : { parentId }),
              ...(input.role === undefined ? {} : { role: input.role }),
              ...(input.dependencies === undefined ? {} : { dependencies: input.dependencies }),
            },
            text(input.operationId, "operationId"),
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
        return output(await client.request("/v1/snapshot", context.signal))
      },
    },
    {
      name: "followup",
      description: "Send durable follow-up instructions through Tandem. No direct native-session dispatch.",
      input: schema({ operationId, taskId: string, text: string }, ["operationId", "taskId", "text"]),
      options,
      async execute(raw, context) {
        const input = object(raw)
        return output(
          await client.request(
            `/v1/tasks/${encodeURIComponent(text(input.taskId, "taskId"))}/followup`,
            context.signal,
            { text: text(input.text, "text") },
            text(input.operationId, "operationId"),
          ),
        )
      },
    },
    {
      name: "decision",
      description:
        "Reply to a pending Tandem decision. Permission answer uses decision, not reply; forms use answer field keys.",
      input: schema({ operationId, decisionId: string, answer: { type: "object" } }, [
        "operationId",
        "decisionId",
        "answer",
      ]),
      options,
      async execute(raw, context) {
        const input = object(raw)
        return output(
          await client.request(
            `/v1/decisions/${encodeURIComponent(text(input.decisionId, "decisionId"))}/reply`,
            context.signal,
            { answer: object(input.answer) },
            text(input.operationId, "operationId"),
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
        ["operationId", "summary", "evidence"],
      ),
      options,
      async execute(raw, context) {
        const input = object(raw)
        const { state, owned } = await worker(client, context)
        if (!owned) throw new Error("This native session is not a Tandem worker")
        const session = await nativeSession(context.sessionID, context.signal)
        const metadata = object(object(session.metadata).tandem)
        if (
          session.id !== context.sessionID ||
          metadata.instanceId !== state.instanceId ||
          metadata.taskId !== owned.task.id ||
          metadata.attemptId !== owned.attempt.id
        ) {
          throw new Error("Native session metadata does not match Tandem ownership")
        }
        return output(
          await client.request(
            `/v1/tasks/${encodeURIComponent(owned.task.id)}/results`,
            context.signal,
            {
              attemptId: owned.attempt.id,
              summary: text(input.summary, "summary"),
              evidence: input.evidence,
            },
            text(input.operationId, "operationId"),
          ),
        )
      },
    },
  ]
}
