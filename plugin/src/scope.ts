import { type Snapshot, type Task, type TandemClient } from "./client.ts"

export async function scopedTask(
  client: TandemClient,
  state: Snapshot,
  taskId: string,
  signal: AbortSignal,
  owner?: Task,
): Promise<Task> {
  const target = await client.task(state, taskId, signal)
  if (owner && target.projectId !== owner.projectId) throw new Error("Task is outside the owned project")
  if (owner && owner.role !== "coordinator" && target.id !== owner.id) {
    throw new Error("Ordinary workers may only steer or answer forms for their current task")
  }
  return target
}
