import assert from "node:assert/strict"
import { spawnSync } from "node:child_process"
import { mkdtempSync, rmSync, writeFileSync } from "node:fs"
import path from "node:path"
import { test } from "node:test"

await test("real formatter, linter, and compiler reject representative violations", () => {
  const root = path.resolve(import.meta.dirname, "..")
  const directory = mkdtempSync(path.join(import.meta.dirname, "quality-fixture-"))
  const file = path.join(directory, "fixture.ts")
  const run = (tool: string, args: string[]) =>
    spawnSync(process.execPath, [path.join(root, "node_modules", tool), ...args], {
      cwd: root,
      encoding: "utf8",
      timeout: 30_000,
    })
  const lint = () => run("eslint/bin/eslint.js", [file])
  const format = () => run("prettier/bin/prettier.cjs", ["--check", file])
  const compile = () => run("typescript/bin/tsc", ["--noEmit", "--strict", "--skipLibCheck", file])
  try {
    writeFileSync(file, 'export const label = "ok"\n')
    assert.equal(format().status, 0)
    const lintPass = lint()
    assert.equal(lintPass.status, 0, lintPass.stdout + lintPass.stderr)
    assert.equal(compile().status, 0)

    writeFileSync(file, 'export const label="bad";\n')
    assert.equal(format().status, 1, "Prettier must reject nonconforming layout")
    writeFileSync(file, "export const label: string = 42\n")
    const compilerFailure = compile()
    assert.notEqual(compilerFailure.status, 0)
    assert.match(compilerFailure.stdout, /not assignable/u)
    writeFileSync(file, 'export const label = "ok"\nPromise.resolve("unhandled")\n')
    const lintFailure = lint()
    assert.equal(lintFailure.status, 1)
    assert.match(lintFailure.stdout, /no-floating-promises/u)
  } finally {
    rmSync(directory, { recursive: true })
  }
})
