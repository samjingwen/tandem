import { createHash } from "node:crypto"

const fixtureCursor = (data: Record<string, string>) =>
  Buffer.from(JSON.stringify(data)).toString("base64url")

function fixtureContent(
  value: Record<string, unknown> | undefined,
  query: Record<string, string>,
  revision: Record<string, unknown>,
  collection: string,
) {
  if (!value) return Response.json({ error: { code: "not_found" } }, { status: 404 })
  const data = Buffer.from(JSON.stringify(value))
  const offset = Number(query.offset ?? 0)
  let end = Math.min(data.length, offset + 32768)
  while (end < data.length && (Number(data[end]) & 0xc0) === 0x80) end--
  return Response.json({
    ...revision,
    collection,
    key: query.key,
    offset,
    totalBytes: data.length,
    text: data.subarray(offset, end).toString(),
    sha256: createHash("sha256").update(data).digest("hex"),
    ...(end < data.length ? { nextCursor: fixtureCursor({ ...query, offset: String(end) }) } : {}),
  })
}

function fixtureRows(state: Record<string, unknown>, collection: string, query: Record<string, string>) {
  const values = (state[collection] ?? {}) as Record<string, Record<string, unknown>>
  const taskProject = (row: Record<string, unknown>) => {
    if (collection === "projects") return row.id
    if (collection === "tasks") return row.projectId
    return (state.tasks as Record<string, Record<string, unknown>>)[String(row.taskId)]?.projectId
  }
  return Object.entries(values).filter(
    ([key, row]) =>
      (!query.key || query.key === key) &&
      (!query.projectId || query.projectId === taskProject(row)) &&
      (!query.taskId || query.taskId === row.taskId || (collection === "tasks" && query.taskId === row.id)) &&
      (!query.sessionRef || query.sessionRef === row.sessionRef),
  )
}

// Executable protocol fixture. It projects fixture data and follows explicit
// page/content requests; it never reads implementation source or credentials.
export function readFixture(input: unknown, url: URL): Response {
  if (!url.pathname.startsWith("/v1/reads/")) return Response.json({ ok: true })
  const state = input as Record<string, unknown>
  if (Array.isArray(state.tasks)) return Response.json({ invalid: true })
  const revision = { instanceId: state.instanceId, revision: state.revision ?? 0 }
  if (url.pathname === "/v1/reads/snapshot")
    return Response.json({
      ...revision,
      maxPageBytes: 262144,
      contentBytes: 32768,
      maxRecords: 64,
      collections: {},
    })
  let query = Object.fromEntries(url.searchParams)
  if (query.cursor)
    query = {
      ...(JSON.parse(Buffer.from(query.cursor, "base64url").toString()) as Record<string, string>),
      ...query,
    }
  delete query.cursor
  const collection = query.collection ?? "tasks"
  const rows = fixtureRows(state, collection, query)
  if (url.pathname === "/v1/reads/content") return fixtureContent(rows[0]?.[1], query, revision, collection)
  const start = Number(query.offset ?? 0)
  const limit = Number(query.limit ?? 32)
  const records = rows.slice(start, start + limit).map(([key, row]) => {
    const summary = Object.fromEntries(
      Object.entries(row).filter(
        ([key]) => !["instructions", "details", "reply", "text", "evidence", "summary"].includes(key),
      ),
    )
    const data = Buffer.from(JSON.stringify(row))
    return {
      key,
      summary: {
        ...summary,
        ...(row.workspace ? { workspacePath: (row.workspace as { path: string }).path } : {}),
      },
      content: {
        cursor: fixtureCursor({ ...query, collection, key, offset: "0" }),
        bytes: data.length,
        sha256: createHash("sha256").update(data).digest("hex"),
      },
    }
  })
  return Response.json({
    ...revision,
    collection,
    records,
    ...(start + limit < rows.length
      ? { nextCursor: fixtureCursor({ ...query, offset: String(start + limit) }) }
      : {}),
  })
}
