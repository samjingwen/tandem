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
          ? { tandem: { instanceId: "instance", taskId: "task", attemptId: "attempt" } }
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
    const guard = permissionGuard(f.lookup, undefined, f.plugin)
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
      "ask",
      "content search must not silently bypass per-file read permissions",
    )
    const shell = evaluation("shell", ["git status"])
    await guard(shell)
    assert.equal(shell.effect, "ask", "saved shell allow must not bypass fresh approval")
    const gate = {
      ...evaluation("tandem.permission.reply", ["exact-answer"]),
      sessionID: "ses_unbound" as PermissionEvaluation["sessionID"],
    }
    await guard(gate)
    assert.equal(
      gate.effect,
      "ask",
      "even unbound sessions must not auto-approve Tandem permission forwarding",
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
