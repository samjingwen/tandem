import { Plugin } from "@opencode/plugin"
import { TandemClient } from "./client.ts"
import { tools } from "./tools.ts"
import { settings } from "./credentials.ts"
import { permissionGuard, toolGuard } from "./guards.ts"
import type { SessionLookup } from "./authority.ts"
import { fileURLToPath } from "node:url"
import { realpath } from "node:fs/promises"

export default Plugin.define({
  id: "tandem",
  async setup(ctx) {
    if (ctx.app.version !== "2.1.0") throw new Error("Tandem plugin requires OpenCode 2.1.0")
    const { url, token, tokenFile } = await settings(
      ctx.options,
      {
        TANDEM_URL: process.env.TANDEM_URL,
        TANDEM_TOKEN: process.env.TANDEM_TOKEN,
      },
      ctx.location.directory,
    )
    const client = new TandemClient(url, token)
    const lookup: SessionLookup = (sessionID, signal) =>
      ctx.session.get({ sessionID }, signal === undefined ? undefined : { signal })
    const protectedRoot = await realpath(fileURLToPath(new URL("../", import.meta.url)))
    await ctx.permission.hook("evaluate", permissionGuard(lookup, tokenFile, protectedRoot))
    await ctx.tool.hook("execute.before", toolGuard(lookup, tokenFile, protectedRoot))
    await ctx.shell.hook("create.before", (event) => {
      // Empty overrides also prevent a process spawner from inheriting the secret.
      event.env.TANDEM_TOKEN = ""
    })
    await ctx.tool.transform((editor) => {
      editor.namespace({
        name: "tandem",
        description: "Persistent coordination, evidence, and human decisions",
      })
      for (const tool of tools(client, { session: lookup, assert: ctx.permission.assert })) editor.add(tool)
    })
  },
})
