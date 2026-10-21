import { createHash } from "node:crypto"
import type { Plugin } from "@opencode/plugin"
import type { OpenCodeClient } from "@opencode/client"
import type { ToolContext } from "@opencode/plugin/promise/tool"
import { currentWorker, object, TandemClient, text } from "./client.ts"

export const permissionReplyAction = "tandem.permission.reply"
export type NativeSession = Pick<
  Awaited<ReturnType<OpenCodeClient["session"]["get"]>>,
  "id" | "metadata" | "location"
>
export type SessionLookup = (sessionID: string, signal?: AbortSignal) => Promise<NativeSession>
export type Context = Pick<ToolContext, "sessionID" | "signal" | "agent" | "messageID" | "id">
export type AssertPermission = Plugin.Context["permission"]["assert"]
export interface NativeAccess {
  session: SessionLookup
  assert: AssertPermission
}

export function claim(session: NativeSession): Record<string, unknown> | undefined {
  const metadata = session.metadata === undefined ? {} : object(session.metadata)
  return metadata.tandem === undefined ? undefined : object(metadata.tandem)
}

export async function authority(client: TandemClient, native: NativeAccess, context: Context) {
  const state = await client.snapshot(context.signal)
  const session = await native.session(context.sessionID, context.signal)
  if (session.id !== context.sessionID) throw new Error("Native session identity mismatch")
  const owned = currentWorker(state, context.sessionID)
  const metadata = claim(session)
  if (!owned && metadata) throw new Error("Managed native session is not a current Tandem attempt")
  if (
    owned &&
    (!metadata ||
      metadata.instanceId !== state.instanceId ||
      metadata.taskId !== owned.task.id ||
      metadata.attemptId !== owned.attempt.id)
  ) {
    throw new Error("Native session metadata does not match Tandem ownership")
  }
  return { state, owned }
}

export interface DecisionRequest {
  id: string
  taskId: string
  attemptId: string
  details: unknown
}

// Native user rejection is intentionally not caught or converted to tool output.
// The plugin's evaluate hook forces a fresh ask, even after a saved approval.
export async function confirmPermissionReply(
  assertPermission: AssertPermission,
  context: Context,
  instanceId: string,
  decision: DecisionRequest,
  answer: Record<string, unknown>,
  operationId: string,
) {
  const fingerprint = createHash("sha256").update(JSON.stringify(answer)).digest("hex")
  await assertPermission({
    sessionID: context.sessionID,
    agent: context.agent,
    source: { type: "tool", messageID: context.messageID, id: context.id },
    action: permissionReplyAction,
    resources: [`${instanceId}/${decision.id}/${operationId}/${fingerprint}`],
    save: [],
    metadata: {
      tandem: {
        instanceId,
        decisionId: decision.id,
        taskId: decision.taskId,
        attemptId: decision.attemptId,
        operationId,
      },
      request: decision.details,
      answer,
    },
  })
  context.signal.throwIfAborted()
}

export function decisionRequest(
  id: string,
  value: unknown,
): DecisionRequest & { kind: string; status: string } {
  const row = object(value)
  if (row.id !== id) throw new Error("Decision identity mismatch")
  return {
    id,
    taskId: text(row.taskId, "decision.taskId"),
    attemptId: text(row.attemptId, "decision.attemptId"),
    kind: text(row.kind, "decision.kind"),
    status: text(row.status, "decision.status"),
    details: row.details ?? {},
  }
}
