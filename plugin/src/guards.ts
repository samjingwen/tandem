import { realpath } from "node:fs/promises"
import path from "node:path"
import type { PermissionEvaluation } from "@opencode/plugin/promise/permission"
import { Error as ToolError } from "@opencode/plugin/promise/tool"
import {
  assignment,
  isHub,
  permissionReplyAction,
  type Assignment,
  type NativeSession,
  type SessionLookup,
} from "./authority.ts"
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
  "tandem_control",
])

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
  const [base, target] = await Promise.all([realpath(root), canonical(candidate)])
  if (base !== root) throw new Error("Assigned directory is no longer canonical")
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
export function permissionGuard(
  lookup: SessionLookup,
  tokenFile?: string,
  protectedRoot?: string,
  coordinatorDirectory?: string,
) {
  return async (event: PermissionEvaluation) => {
    if (event.effect === "deny") return
    try {
      const session = await lookup(event.sessionID)
      const binding = assignment(session, event.sessionID)
      if (event.action === permissionReplyAction) {
        event.effect = confirmationEffect(binding, session, coordinatorDirectory)
        event.message = "Confirm forwarding this exact Tandem permission answer."
        return
      }
      if (!binding) return
      if (event.action === "edit" && binding.role === "coordinator") {
        event.effect = "deny"
        return
      }
      if (event.action === "read" || event.action === "edit") {
        for (const resource of event.resources)
          await workspacePath(
            binding.directory,
            resource,
            tokenFile,
            false,
            event.action === "edit",
            protectedRoot,
          )
      } else if (event.action === "grep" || event.action === "glob") {
        const directory = event.metadata?.path ?? "."
        await workspacePath(binding.directory, text(directory, "search path"), tokenFile, true)
      }
    } catch {
      event.effect = "deny"
      event.message = "Tandem confines native file access to the assigned workspace and excludes credentials."
    }
  }
}

function confirmationEffect(
  binding: Assignment | undefined,
  session: NativeSession,
  coordinatorDirectory?: string,
): "ask" | "deny" {
  return (binding ? binding.role === "coordinator" : isHub(session, coordinatorDirectory)) ? "ask" : "deny"
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
export function toolGuard(
  lookup: SessionLookup,
  tokenFile?: string,
  protectedRoot?: string,
  coordinatorDirectory?: string,
) {
  return async (event: BeforeTool) => {
    const session = await lookup(event.sessionID)
    let binding
    try {
      binding = assignment(session, event.sessionID)
    } catch {
      throw new ToolError({ message: "Malformed or moved managed session assignment" })
    }
    if (!binding) {
      if (event.tool.startsWith("tandem_") && !isHub(session, coordinatorDirectory))
        throw new ToolError({
          message: "Unbound Tandem tools require the explicitly configured coordinator directory",
        })
      return
    }
    if (!allowedTools.has(event.tool))
      throw new ToolError({
        message:
          "Managed Tandem sessions cannot invoke unmanaged delegation, MCP, or runtime/provider/worktree administration",
      })
    if (binding.role === "worker" && ["tandem_control", "tandem_decision"].includes(event.tool))
      throw new ToolError({ message: "Ordinary workers cannot control tasks or answer decisions" })
    if (binding.role === "coordinator" && ["edit", "write", "patch"].includes(event.tool))
      throw new ToolError({ message: "Managed coordinators cannot edit project files" })
    try {
      for (const target of targets(event.tool, object(event.input))) {
        await workspacePath(
          binding.directory,
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
