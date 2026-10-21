import { realpath } from "node:fs/promises"
import path from "node:path"
import type { PermissionEvaluation } from "@opencode/plugin/promise/permission"
import { Error as ToolError } from "@opencode/plugin/promise/tool"
import { claim, permissionReplyAction, type NativeSession, type SessionLookup } from "./authority.ts"
import { object, text } from "./client.ts"

const allowedTools = new Set([
  "execute",
  "read",
  "edit",
  "write",
  "patch",
  "glob",
  "grep",
  "shell",
  "question",
  "tandem_create",
  "tandem_status",
  "tandem_followup",
  "tandem_decision",
  "tandem_result",
])

function managed(session: NativeSession): boolean {
  const owner = claim(session)
  if (!owner) return false
  text(owner.instanceId, "native instanceId")
  text(owner.taskId, "native taskId")
  text(owner.attemptId, "native attemptId")
  return true
}

function within(root: string, file: string): boolean {
  const relative = path.relative(root, file)
  return (
    relative === "" ||
    (relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
  )
}

async function canonical(file: string): Promise<string> {
  try {
    return await realpath(file)
  } catch (error) {
    const code = (error as NodeJS.ErrnoException).code
    if ((code !== "ENOENT" && code !== "ENOTDIR") || path.dirname(file) === file)
      throw new Error("Cannot verify workspace path", { cause: error })
    return path.join(await canonical(path.dirname(file)), path.basename(file))
  }
}

function controlMutation(candidate: string, target: string, protectedRoot?: string) {
  const parts = `${candidate}${path.sep}${target}`.toLowerCase().split(path.sep)
  const names = [path.basename(candidate).toLowerCase(), path.basename(target).toLowerCase()]
  return (
    parts.includes(".opencode") ||
    parts.includes(".git") ||
    names.some((name) => name === "opencode.json" || name === "opencode.jsonc") ||
    (protectedRoot !== undefined && within(path.resolve(protectedRoot), target))
  )
}

async function workspacePath(
  root: string,
  input: string,
  tokenFile?: string,
  search = false,
  mutation = false,
  protectedRoot?: string,
) {
  const normalized = input.replaceAll("\\", "/")
  if (normalized.startsWith("~") || normalized.includes("\0") || normalized.length > 4096)
    throw new Error("Unsupported workspace path")
  const candidate = path.resolve(root, normalized)
  if (!within(root, candidate)) throw new Error("Path is outside the assigned workspace")
  const [base, target] = await Promise.all([canonical(root), canonical(candidate)])
  if (!within(base, target)) throw new Error("Symlink target is outside the assigned workspace")
  if (mutation && controlMutation(candidate, target, protectedRoot))
    throw new Error("Runtime configuration and loaded plugins are outside model mutation authority")
  if (tokenFile && (target === tokenFile || (search && within(target, tokenFile))))
    throw new Error("Credential files are outside the model's read authority")
  privateEnvironment(target)
}

function privateEnvironment(target: string) {
  const name = path.basename(target).toLowerCase()
  if ((name === ".env" || name.startsWith(".env.")) && name !== ".env.example")
    throw new Error("Private environment files are outside the model's authority")
}

// Pinned Core read/edit resources are relative paths. Grep/glob resources are
// patterns, so their search root must instead be checked from native metadata.
export function permissionGuard(lookup: SessionLookup, tokenFile?: string, protectedRoot?: string) {
  return async (event: PermissionEvaluation) => {
    if (event.action === permissionReplyAction) {
      event.effect = "ask"
      event.message = "Confirm forwarding this exact Tandem permission answer."
      return
    }
    const session = await lookup(event.sessionID)
    if (!managed(session)) return
    if (event.action === "shell") {
      event.effect = "ask"
      return
    }
    try {
      if (event.action === "read" || event.action === "edit") {
        for (const resource of event.resources)
          await workspacePath(
            session.location.directory,
            resource,
            tokenFile,
            false,
            event.action === "edit",
            protectedRoot,
          )
      } else if (event.action === "grep" || event.action === "glob") {
        const directory = event.metadata?.path ?? "."
        await workspacePath(session.location.directory, text(directory, "search path"), tokenFile, true)
        if (event.action === "grep") event.effect = "ask"
      }
    } catch {
      event.effect = "deny"
      event.message = "Tandem confines native file access to the assigned workspace and excludes credentials."
    }
  }
}

interface BeforeTool {
  tool: string
  sessionID: string
  input: unknown
}

function targets(tool: string, input: Record<string, unknown>): string[] {
  if (["read", "edit", "write"].includes(tool)) return [text(input.path, "path")]
  if (tool === "shell") return [text(input.workdir ?? ".", "workdir")]
  if (tool === "grep" || tool === "glob") return [text(input.path ?? ".", "search path")]
  if (tool !== "patch") return []
  const paths = text(input.patchText, "patchText")
    .split(/\r?\n/u)
    .flatMap((line) => {
      const match = /^\*\*\* (?:Add File|Delete File|Update File|Move to): (.+)$/u.exec(line.trim())
      return match ? [text(match[1], "patch path").trim()] : []
    })
  if (paths.length === 0) throw new Error("No verifiable patch targets")
  return paths
}

// A tool can advertise permission:"edit" instead of its own name. In 2.1.0
// worktree_remove does exactly this, so session wildcard rules alone cannot
// exclude every administrative/delegation entrypoint from an editing worker.
export function toolGuard(lookup: SessionLookup, tokenFile?: string, protectedRoot?: string) {
  return async (event: BeforeTool) => {
    const session = await lookup(event.sessionID)
    if (!managed(session)) return
    if (!allowedTools.has(event.tool))
      throw new ToolError({
        message:
          "Managed Tandem sessions cannot invoke unmanaged delegation, MCP, or runtime/provider/worktree administration",
      })
    try {
      for (const target of targets(event.tool, object(event.input))) {
        await workspacePath(
          session.location.directory,
          target,
          tokenFile,
          event.tool === "grep" || event.tool === "glob",
          ["edit", "write", "patch"].includes(event.tool),
          protectedRoot,
        )
      }
    } catch {
      throw new ToolError({
        message: "Tandem tool targets must remain inside the assigned workspace and exclude credentials",
      })
    }
  }
}
