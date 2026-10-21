import { Plugin } from "@opencode/plugin"
import { TandemClient } from "./client.ts"
import { tools } from "./tools.ts"

export default Plugin.define({
  id: "tandem",
  async setup(ctx) {
    if (ctx.app.version !== "2.1.0") throw new Error("Tandem plugin requires OpenCode 2.1.0")
    const url = process.env.TANDEM_URL
    const token = process.env.TANDEM_TOKEN
    if (!url || !token) throw new Error("Explicit TANDEM_URL and TANDEM_TOKEN are required")
    const client = new TandemClient(url, token)
    await ctx.shell.hook("create.before", (event) => {
      // Empty overrides also prevent a process spawner from inheriting the secret.
      event.env.TANDEM_TOKEN = ""
    })
    await ctx.tool.transform((editor) => {
      editor.namespace({
        name: "tandem",
        description: "Persistent coordination, evidence, and human decisions",
      })
      for (const tool of tools(client, (sessionID, signal) => ctx.session.get({ sessionID }, { signal })))
        editor.add(tool)
    })
  },
})
