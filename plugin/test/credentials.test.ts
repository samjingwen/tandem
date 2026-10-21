import assert from "node:assert/strict"
import { chmod, link, mkdir, mkdtemp, realpath, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { test } from "node:test"
import { loadTokenFile, settings } from "../src/credentials.ts"

async function fixture() {
  const root = await mkdtemp(path.join(await realpath(tmpdir()), "tandem-token-test-"))
  const file = path.join(root, "own-fixture-token")
  await writeFile(file, "test-fixture-token\n", { mode: 0o600 })
  return { root, file, cleanup: () => rm(root, { recursive: true, force: true }) }
}

await test("explicit owner-only token file and URL options work without shared service environment", async () => {
  const f = await fixture()
  try {
    const work = path.join(f.root, "work")
    await mkdir(work, { mode: 0o700 })
    assert.equal(await loadTokenFile(f.file), "test-fixture-token")
    const configured = await settings(
      { url: "http://127.0.0.1:7777", tokenFile: f.file },
      { TANDEM_URL: "http://localhost:9999", TANDEM_TOKEN: "env-fallback" },
      work,
    )
    assert.equal(configured.url, "http://127.0.0.1:7777")
    assert.equal(configured.token, "test-fixture-token")
    assert.deepEqual(
      await settings({}, { TANDEM_URL: "http://localhost:7777", TANDEM_TOKEN: "explicit-env" }),
      { url: "http://localhost:7777", token: "explicit-env", tokenFile: undefined },
    )
    await assert.rejects(
      settings(
        { url: "http://localhost", tokenFile: f.file },
        { TANDEM_URL: undefined, TANDEM_TOKEN: undefined },
        f.root,
      ),
      /outside/u,
    )
  } finally {
    await f.cleanup()
  }
})

await test("token loader rejects symlinks, parent aliases, hardlinks, public modes, directories, and invalid text", async () => {
  const f = await fixture()
  try {
    await chmod(f.file, 0o644)
    await assert.rejects(loadTokenFile(f.file), /owner-only/u)
    await chmod(f.file, 0o600)
    const alias = path.join(f.root, "alias")
    await symlink(f.file, alias)
    await assert.rejects(loadTokenFile(alias), /non-symlink/u)
    const directory = path.join(f.root, "directory")
    await mkdir(directory, { mode: 0o700 })
    const nested = path.join(directory, "token")
    await writeFile(nested, "fixture", { mode: 0o600 })
    const parentAlias = path.join(f.root, "parent-alias")
    await symlink(directory, parentAlias)
    await assert.rejects(loadTokenFile(path.join(parentAlias, "token")), /non-symlink/u)
    await assert.rejects(loadTokenFile(directory), /regular/u)
    await link(f.file, path.join(f.root, "hardlink"))
    await assert.rejects(loadTokenFile(f.file), /regular/u)
    for (const content of ["", "two tokens", "line\nbreak", "x".repeat(16_385)]) {
      await writeFile(nested, content, { mode: 0o600 })
      await assert.rejects(loadTokenFile(nested), /secure Tandem tokenFile/u)
    }
    await assert.rejects(loadTokenFile("relative/token"), /absolute/u)
    await assert.rejects(
      settings(
        { url: "http://localhost", tokenFile: alias },
        { TANDEM_URL: undefined, TANDEM_TOKEN: "must-not-fallback" },
      ),
      /non-symlink/u,
    )
  } finally {
    await f.cleanup()
  }
})

await test("owner identity mismatch is rejected using only an owned disposable fixture", async (t) => {
  if (typeof process.getuid !== "function") {
    t.skip("POSIX owner checks require getuid")
    return
  }
  const f = await fixture()
  const uid = process.getuid()
  try {
    const posix = process as NodeJS.Process & { getuid(): number }
    t.mock.method(posix, "getuid", () => uid + 1)
    await assert.rejects(loadTokenFile(f.file), /owner-only/u)
  } finally {
    t.mock.restoreAll()
    await f.cleanup()
  }
})
