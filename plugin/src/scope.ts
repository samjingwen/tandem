import { object, type Snapshot, type Task } from "./client.ts"

export function records(value: unknown): Record<string, unknown> {
  return value === undefined ? {} : object(value)
}

export function projectSnapshot(state: Snapshot, projectId: string): Record<string, unknown> {
  const tasks = Object.fromEntries(
    Object.entries(state.tasks).filter(([, task]) => task.projectId === projectId),
  )
  const taskIDs = new Set(Object.keys(tasks))
  const select = (value: unknown, keep: (key: string, row: Record<string, unknown>) => boolean) =>
    Object.fromEntries(Object.entries(records(value)).filter(([key, row]) => keep(key, object(row))))
  const inProject = (_key: string, row: Record<string, unknown>) =>
    typeof row.taskId === "string" && taskIDs.has(row.taskId)
  // Unknown top-level extensions are not passed through to managed sessions.
  return {
    instanceId: state.instanceId,
    revision: state.raw.revision,
    projects: select(state.raw.projects, (key, row) => key === projectId && row.id === projectId),
    tasks,
    attempts: select(state.attempts, inProject),
    commands: select(state.raw.commands, inProject),
    decisions: select(state.raw.decisions, inProject),
    results: select(state.raw.results, inProject),
  }
}

export function scopedTask(state: Snapshot, taskId: string, owner?: Task): Task {
  const target = state.tasks[taskId]
  if (!target) throw new Error("Task is not available")
  if (owner && target.projectId !== owner.projectId) throw new Error("Task is outside the owned project")
  if (owner && owner.role !== "coordinator" && target.id !== owner.id) {
    throw new Error("Ordinary workers may only steer or answer forms for their current task")
  }
  return target
}
