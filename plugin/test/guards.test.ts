import assert from "node:assert/strict"
import { mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { test } from "node:test"
import type { PermissionEvaluation } from "@opencode/plugin/promise/permission"
import { permissionGuard, toolGuard } from "../src/guards.ts"
import type { SessionLookup } from "../src/authority.ts"

async function fixture() {
  const root = await mkdtemp(path.join(await realpath(tmpdir()), "tandem-guard-test-"))
  const work = path.join(root, "workspace")
  const outside = path.join(root, "other-workspace")
  const plugin = path.join(work, "loaded-plugin")
  await Promise.all([mkdir(work), mkdir(outside)])
  await mkdir(plugin)
  await writeFile(path.join(outside, "private.txt"), "owned test fixture only", { mode: 0o600 })
  await symlink(outside, path.join(work, "outside-link"))
  const lookup: SessionLookup = (id) =>
    Promise.resolve({
      id,
      metadata:
        id === "ses_managed"
          ? {
              tandem: {
                instanceId: "instance",
                taskId: "task",
                attemptId: "attempt",
                directory: work,
                role: "worker",
              },
            }
          : {},
      location: { directory: work },
    })
  return { root, work, plugin, lookup, cleanup: () => rm(root, { recursive: true, force: true }) }
}

function evaluation(
  action: string,
  resources: string[],
  metadata?: Record<string, unknown>,
): PermissionEvaluation {
  return {
    sessionID: "ses_managed" as PermissionEvaluation["sessionID"],
    action,
    resources,
    effect: "allow",
    ...(metadata === undefined ? {} : { metadata }),
  }
}

await test("managed paths are canonicalized, including new files and search roots, without executing native tools", async () => {
  const f = await fixture()
  try {
    const guard = permissionGuard(f.lookup, undefined, f.plugin, f.work)
    for (const [action, resource] of [
      ["read", "README.md"],
      ["edit", "new/nested/file.go"],
    ]) {
      const event = evaluation(action ?? "", [resource ?? ""])
      await guard(event)
      assert.equal(event.effect, "allow")
    }
    for (const resource of [
      "../other-workspace/private.txt",
      "outside-link/private.txt",
      "outside-link/new.txt",
    ]) {
      const event = evaluation("read", [resource])
      await guard(event)
      assert.equal(event.effect, "deny")
    }
    for (const resource of [
      "opencode.json",
      "nested/.opencode/plugins/escape.js",
      ".git/config",
      "loaded-plugin/server.js",
    ]) {
      const event = evaluation("edit", [resource])
      await guard(event)
      assert.equal(event.effect, "deny")
    }
    const search = evaluation("grep", ["pattern"], { path: "../other-workspace" })
    await guard(search)
    assert.equal(search.effect, "deny")
    const insideSearch = evaluation("grep", ["ordinary pattern"], { path: "." })
    await guard(insideSearch)
    assert.equal(
      insideSearch.effect,
      "allow",
      "saved native content-search choices remain authoritative within the checked root",
    )
    const shell = evaluation("shell", ["git status"])
    await guard(shell)
    assert.equal(shell.effect, "allow", "the runtime's actual saved shell choice must remain authoritative")
    const gate = {
      ...evaluation("tandem.permission.reply", ["exact-answer"]),
      sessionID: "ses_unbound" as PermissionEvaluation["sessionID"],
    }
    await guard(gate)
    assert.equal(
      gate.effect,
      "ask",
      "the explicitly opted-in hub still needs confirmation before forwarding a permission",
    )
  } finally {
    await f.cleanup()
  }
})

await test("native delegation/admin/MCP tool names and outside mutation targets fail before execution", async () => {
  const f = await fixture()
  try {
    const guard = toolGuard(f.lookup, undefined, f.plugin)
    for (const tool of [
      "subagent",
      "opencode_worktree_create",
      "opencode_worktree_remove",
      "opencode_session_move",
      "mcp_secret_tool",
      "provider_manage",
    ]) {
      await assert.rejects(
        guard({ tool, sessionID: "ses_managed", input: { directory: f.work } }),
        /unmanaged delegation/u,
      )
    }
    for (const input of [
      { tool: "write", input: { path: "../other-workspace/file" } },
      { tool: "edit", input: { path: "opencode.json" } },
      { tool: "glob", input: { path: "outside-link", pattern: "*" } },
      {
        tool: "patch",
        input: {
          patchText:
            "*** Begin Patch\n*** Update File: outside-link/private.txt\n@@\n-secret\n+changed\n*** End Patch",
        },
      },
    ]) {
      await assert.rejects(guard({ ...input, sessionID: "ses_managed" }), /assigned workspace/u)
    }
    await guard({ tool: "write", sessionID: "ses_managed", input: { path: "new-file.go" } })
    await guard({ tool: "opencode_worktree_remove", sessionID: "ses_unbound", input: { directory: f.work } })
  } finally {
    await f.cleanup()
  }
})

await test("moving a managed native session cannot redefine its assigned workspace", async () => {
  const f = await fixture()
  try {
    const moved: SessionLookup = async (id) => {
      const session = await f.lookup(id)
      return {
        ...session,
        metadata: {
          tandem: {
            instanceId: "instance",
            taskId: "task",
            attemptId: "attempt",
            directory: f.work,
            role: "worker",
          },
        },
        location: { directory: path.join(f.root, "other-workspace") },
      }
    }
    await assert.rejects(
      toolGuard(moved)({ tool: "read", sessionID: "ses_managed", input: { path: "private.txt" } }),
    )
  } finally {
    await f.cleanup()
  }
})

await test("fresh approval requirements never relax an existing runtime deny", async () => {
  const f = await fixture()
  try {
    const guard = permissionGuard(f.lookup)
    for (const action of ["shell", "tandem.permission.reply"]) {
      const event = evaluation(action, ["explicitly-forbidden"])
      event.effect = "deny"
      await guard(event)
      assert.equal(event.effect, "deny", "an explicit deny must not become an ask")
    }
  } finally {
    await f.cleanup()
  }
})

await test("unbound inherited hook calls are denied outside the explicit hub", async () => {
  const f = await fixture()
  try {
    const outside: SessionLookup = async (id) => ({
      ...(await f.lookup(id)),
      metadata: {},
      location: { directory: path.join(f.root, "other-workspace") },
    })
    await assert.rejects(
      toolGuard(
        outside,
        undefined,
        undefined,
        f.work,
      )({ tool: "tandem_status", sessionID: "ses_unbound", input: {} }),
      /explicitly configured coordinator directory/u,
    )
    const gate = {
      ...evaluation("tandem.permission.reply", ["answer"]),
      sessionID: "ses_unbound" as PermissionEvaluation["sessionID"],
    }
    await permissionGuard(outside, undefined, undefined, f.work)(gate)
    assert.equal(gate.effect, "deny")
    await toolGuard(
      f.lookup,
      undefined,
      undefined,
      f.work,
    )({ tool: "tandem_status", sessionID: "ses_unbound", input: {} })
  } finally {
    await f.cleanup()
  }
})

await test("native guards reject malformed bindings and moved permission roots without a Tandem lookup", async () => {
  const f = await fixture()
  try {
    for (const change of [
      { directory: null },
      { role: null },
      { role: "admin" },
      { directory: path.join(f.root, "other-workspace") },
    ]) {
      const lookup: SessionLookup = async (id) => {
        const session = await f.lookup(id)
        return {
          ...session,
          metadata: {
            tandem: {
              instanceId: "instance",
              taskId: "task",
              attemptId: "attempt",
              directory: f.work,
              role: "worker",
              ...change,
            },
          },
        }
      }
      await assert.rejects(
        toolGuard(lookup)({ tool: "read", sessionID: "ses_managed", input: { path: "private.txt" } }),
        /Malformed or moved/u,
      )
      const event = evaluation("read", ["private.txt"])
      await permissionGuard(lookup)(event)
      assert.equal(event.effect, "deny")
    }
    await toolGuard(f.lookup)({ tool: "write", sessionID: "ses_managed", input: { path: "offline-work.go" } })
    const event = evaluation("edit", ["offline-work.go"])
    await permissionGuard(f.lookup)(event)
    assert.equal(event.effect, "allow")
    for (const tool of ["tandem_control", "tandem_decision"])
      await assert.rejects(
        toolGuard(f.lookup)({ tool, sessionID: "ses_managed", input: {} }),
        /Ordinary workers/u,
      )
  } finally {
    await f.cleanup()
  }
})
