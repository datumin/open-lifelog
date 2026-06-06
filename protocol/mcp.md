# MCP surface

The MCP surface exposes the node's core read and write operations as
[Model Context Protocol](https://modelcontextprotocol.io) tools over the
Streamable HTTP transport. It is the surface an LLM client uses; it reads and
writes the same records under the same authorization as the [REST](./rest.md)
surface.

## Endpoints

| Path | Scope |
|---|---|
| `POST /mcp` | Un-scoped: tools for every type are offered (subject to consent). |
| `POST /mcp/{capability}` | Only the (type, operation) pairs within the [capability](./capability-urls.md). |

Both require a valid [bearer token](./auth.md#presenting-a-token-bearer-validation),
and every tool call is subject to the
[three-stage narrowing](./README.md#the-three-stage-narrowing).

## Per-type tools

`tools/list` is generated from the node's type set and schema registry — types
are not hardcoded. For each type, up to five tools are offered, filtered to the
connection's capability bound:

| Tool | Kind | Required scope |
|---|---|---|
| `<type>_record` | write | `lifelog:write:<type>` |
| `<type>_update` | write | `lifelog:write:<type>` |
| `<type>_delete` | write | `lifelog:write:<type>` |
| `<type>_get` | read | `lifelog:read:<type>` |
| `<type>_list` | read | `lifelog:read:<type>` |

A new type (a standard addition or a reverse-DNS extension) yields its tools
automatically once its schema is present.

## Input schemas

- Each tool's `inputSchema` embeds the type's payload JSON Schema under the
  `payload` property. If the type schema has `$defs`, they are hoisted to the
  `inputSchema` root so `#/$defs/...` references resolve; `$schema` and `$id` are
  stripped.
- Write tools accept the same writable envelope fields as the REST
  [write body](./rest.md#write-body): `olf_version` (OPTIONAL, defaults to the
  node's current version), `occurred_at`, `tz` (OPTIONAL), `source`, `payload`.
  `id` and `recorded_at` are node-assigned and MUST NOT be supplied.
- `<type>_get` and `<type>_delete` take an `id`.
- `<type>_list` takes `occurred_from` and `occurred_to` — offset-bearing
  RFC 3339 instants bounding `occurred_at` inclusively; either MAY be omitted for
  an unbounded side. The returned range is intersected with the grant's
  [read window](./authorization.md#read-window).

## Results

A successful tool result carries the same [response envelope](./rest.md#response-envelope)
as REST — `{ data, meta }` — in **both** `structuredContent` and the text content,
so an LLM client sees the `meta` (a clipped list range, or a
`written_outside_read_window` warning), not just the data. `_list` returns the
record array under `data` with `ListMeta`; `_get`/`_record`/`_update` return the
single record under `data`.

## Annotations

Tools carry `ToolAnnotations` so a client can reason about their effects:

- read tools (`_get`, `_list`) are `ReadOnlyHint`;
- `_update` and `_delete` are `DestructiveHint` + `IdempotentHint`;
- `_record` is non-destructive.

## Errors

Validation failures, "not found", and authorization denials are returned as
**tool errors** (`CallToolResult.IsError = true`), not MCP protocol errors, so a
client can read the reason and self-correct. The error carries the same
[error envelope](./rest.md#errors) (`{ "error": { "code", "message", … } }`) in
`structuredContent`, with `message` mirrored in the text content. In particular
`<type>_get` distinguishes `not_found` (the id does not exist) from
`out_of_read_scope` (the record exists but is outside the read window — the latter
carries `granted_read_window` and `record_occurred_at`). Transport-level
authentication (an absent or invalid bearer) is still a `401` per
[auth](./auth.md).
