# Capability URLs

A path segment in the connection URL encodes the **upper bound** of the
(type, operation) pairs that connection may reach. The same grammar applies to
the MCP surface (`/mcp/{capability}`) and the REST surface (`/api/{capability}`).

There is no link ledger and no separate revocation of a capability URL — the URL
*is* the bound. Knowing a URL grants no access on its own; safety is enforced by
[OAuth](./auth.md) and the owner's [consent](./authorization.md). A capability is
a ceiling, not a grant: it can only restrict what a token and grant already
allow.

## Grammar

```
capability := perm ("," perm)*
perm       := type ":" ops
type       := an OLF type name (e.g. "meal", "x.com.acme.mood") or "*"
ops        := "r" | "w" | "rw" | "wr"
```

- `r` is read, `w` is write; `rw` (and `wr`) is both.
- `*` means "all types" for the given operation.

## Examples

| URL | Meaning |
|---|---|
| `/mcp/meal:w` | write `meal` only |
| `/mcp/meal:rw,sleep:r` | read+write `meal`, read `sleep` |
| `/mcp/*:r` | read all types |
| `/mcp/*:rw,meal:w` | read all types, plus write `meal` |
| `/api/meal:rw,sleep:r` | (REST) read+write `meal`, read `sleep` |
| `/api/*:rw` | (REST) read+write all types |
| `/mcp` | (MCP only) un-scoped: every type is offered, subject to consent |

A given capability string is identical across surfaces — only the `/mcp/` vs
`/api/` prefix differs. The MCP surface additionally offers an **un-scoped**
`/mcp` connection; the REST surface has **no un-scoped form** — to reach every
type over REST, use a `*:rw` (or `*:r`) capability.

## Effects

A capability URL governs all of the following:

- **Exposed operations.** On MCP, `tools/list` returns only the tools within the
  URL's bound. On REST, an (operation, type) outside the bound is rejected with
  `403`.
- **Consent options.** The consent step offers only the intersection of the
  client's requested scope and the URL's bound.
- **Token audience.** The connected URL is recorded as the token's `resource`;
  a token minted for a different URL is rejected (see
  [audience binding](./auth.md#audience-binding-rfc-8707)). The surface segment
  (`mcp` / `api`) is part of the audience.
- **Unknown type / malformed grammar.** The URL resolves to `404`.
