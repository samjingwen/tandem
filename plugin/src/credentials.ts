import { constants, type Stats } from "node:fs"
import { lstat, open, realpath } from "node:fs/promises"
import path from "node:path"
import { object, text } from "./client.ts"

const limit = 16_384

function validateFile(info: Stats) {
  if (
    !info.isFile() ||
    typeof process.getuid !== "function" ||
    info.uid !== process.getuid() ||
    (info.mode & 0o077) !== 0 ||
    (info.mode & 0o400) === 0 ||
    info.nlink !== 1 ||
    info.size < 1 ||
    info.size > limit
  ) {
    throw new Error("Tandem tokenFile must be an owner-only readable regular single-link file")
  }
}

export async function loadTokenFile(file: string): Promise<string> {
  if (!path.isAbsolute(file) || file.includes("\0"))
    throw new Error("Tandem tokenFile requires an explicit absolute path")
  try {
    if ((await realpath(file)) !== path.resolve(file)) throw new Error("Noncanonical token path")
    const before = await lstat(file)
    validateFile(before)
    const handle = await open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
    try {
      const opened = await handle.stat()
      validateFile(opened)
      if (before.dev !== opened.dev || before.ino !== opened.ino) throw new Error("Token file changed")
      const buffer = Buffer.alloc(limit + 1)
      let offset = 0
      while (offset < buffer.length) {
        const { bytesRead } = await handle.read(buffer, offset, buffer.length - offset, null)
        if (bytesRead === 0) break
        offset += bytesRead
      }
      if (offset > limit) throw new Error("Token file exceeds size limit")
      validateFile(await handle.stat())
      const token = buffer.subarray(0, offset).toString("utf8").trim()
      if (!/^[\x21-\x7e]+$/u.test(token)) throw new Error("Invalid token text")
      return token
    } finally {
      await handle.close()
    }
  } catch {
    // Do not expose a credential path, OS error payload, or file contents.
    throw new Error("Cannot load secure Tandem tokenFile: require an owner-only regular non-symlink file")
  }
}

export async function settings(
  options: unknown,
  env: Pick<NodeJS.ProcessEnv, "TANDEM_URL" | "TANDEM_TOKEN">,
  directory?: string,
) {
  const input = object(options)
  if (Object.keys(input).some((key) => key !== "url" && key !== "tokenFile"))
    throw new Error("Unsupported Tandem plugin option")
  const url = input.url === undefined ? env.TANDEM_URL : text(input.url, "Tandem url option")
  const tokenFile =
    input.tokenFile === undefined ? undefined : text(input.tokenFile, "Tandem tokenFile option")
  if (tokenFile !== undefined && directory !== undefined) await credentialPlacement(directory, tokenFile)
  const token = tokenFile === undefined ? env.TANDEM_TOKEN : await loadTokenFile(tokenFile)
  if (!url || !token)
    throw new Error("Provide Tandem url/tokenFile options or explicit TANDEM_URL/TANDEM_TOKEN")
  return { url, token, tokenFile: tokenFile === undefined ? undefined : path.resolve(tokenFile) }
}

async function credentialPlacement(directory: string, file: string) {
  if (!path.isAbsolute(file)) throw new Error("Tandem tokenFile requires an explicit absolute path")
  const [root, target] = await Promise.all([realpath(directory), realpath(file)]).catch(() => {
    throw new Error("Tandem tokenFile and location must exist and remain outside the same workspace")
  })
  const relative = path.relative(root, target)
  if (
    relative === "" ||
    (relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative))
  )
    throw new Error("Tandem tokenFile must remain outside the plugin location and managed workspace")
}
