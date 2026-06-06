# OLF node — implementation specification

This is the specification for **this node**: the self-hosted, single-owner Go
binary that serves one person's OLF lifelog. It is **not** the canonical
contract — those live one level up and are language- and implementation-neutral:

- **Format** — what a record is: [`../../spec/`](../../spec) (envelope, type
  system, on-disk layout, v1 types).
- **Protocol** — how records move over the wire: [`../../protocol/`](../../protocol)
  (auth, capability URLs, the REST and MCP surfaces, authorization/PEP, the
  `{data,meta}` response envelope and coded errors).

This document covers only what those deliberately leave **implementation-defined**
for a node: storage, the write/read core, owner authentication, the node-local
parts of the read data window, the owner UI, deployment, and the conformance
checklist. Where a behavior is part of the wire contract, this doc defers to
`../../protocol/` rather than restating it.

> The authoritative Japanese version of this document is `spec.ja.md` (kept
> local). This English copy tracks it; on any divergence the canonical
> `../../spec` and `../../protocol` win.

## 1. Scope

A **node** is one running instance, owned by a single person (the **owner**) who
self-hosts it. Clients (third-party apps, the owner's own LLM client, ingestion
adapters) read and write through it. Multi-tenant / federated hosting is a
different implementation and out of scope here; it shares only the format and
protocol contracts.

Design principles:

- **Storage and control are separate.** The lifelog (canonical JSONL) is the
  owner's data; the control plane (auth + grant ledger) is metadata. Both are
  the owner's, kept in distinct files.
- **Promise only what can be enforced.** The owner chooses what to share, can
  revoke future access instantly, and accepts that copies already handed out
  cannot be recalled.
- **Light, language-neutral, portable.** The lifelog is plain JSONL; `tar` moves
  the whole node.

## 2. Architecture

```
clients ─▶ HTTP/MCP surfaces ─▶ core (read / write) ─▶ storage
            (thin translation)    (single write path)   (JSONL + meta.db)
                    │
                    └─ OAuth AS + consent + PEP (authorization)
```

- The **core** owns the only write path: validate → mint id/recorded_at →
  append. No surface duplicates this (see [protocol README — three-stage
  narrowing](../../protocol/README.md#the-three-stage-narrowing)).
- The **surfaces** ([REST](../../protocol/rest.md), [MCP](../../protocol/mcp.md))
  are thin protocol translation over the same core under the same authorization.
- Each layer is a replaceable boundary (storage, query, metadata DB).

## 3. Storage

Two stores, never mixed:

- **Lifelog (canonical).** Append-only JSONL, partitioned by type and the local
  calendar date of `occurred_at`:
  `<data>/<type>/<YYYY>/<MM>/<YYYY-MM-DD>.jsonl` (see
  [format on-disk layout](../../spec/on-disk.md)). This JSONL is the single
  source of truth; indexes/caches are derived and regenerable. Invariant: a given
  `id` appears at most once across the whole log.
- **Metadata.** A single SQLite file `<data>/meta.db` (WAL, `busy_timeout`,
  `foreign_keys=ON`), holding OAuth clients, tokens (hashes only), the grant
  ledger, and owner sessions/secret (hashes only). It **never** contains lifelog
  records. Portable column types only (`id` as TEXT, arrays as JSON TEXT, times
  as RFC 3339 TEXT). Schema migrations tracked via `PRAGMA user_version`.

## 4. Write core

`record` / `update` / `delete`, all through the core:

1. **Validate** every write before persisting (atomic — a failure changes
   nothing): envelope schema, then the payload against the type's schema for its
   major version, then the semantic invariants schemas cannot express (e.g.
   `ended_at >= occurred_at`, sleep-stage ordering). See
   [format type system](../../spec/type-system.md).
2. **Node-owned fields.** The node mints `id` (UUIDv7) and sets `recorded_at`;
   clients never supply them. `occurred_at` comes from the client.
3. **Persist.** `record` appends a JSON line (fsync). `update`/`delete` rewrite
   the affected day file atomically (temp + fsync + rename), keeping the
   "id at most once, no tombstones" invariant. An `update`/`delete` of a missing
   id is reported not-found and changes nothing.

## 5. Read core

- **List** by type over an `occurred_at` range; **Get** one record by id.
- `occurred_at` is compared as a **true instant** (offset/zone-independent);
  inputs are strict offset-bearing RFC 3339 (naïve values rejected); precision
  differences must not shift boundaries.
- **Read-after-write** is strongly consistent (local FS synchronous writes).
- Responses use the [`{data,meta}` envelope](../../protocol/rest.md#response-envelope):
  list reports the requested vs effective range and whether the read window
  clipped it; reads never silently clip.

## 6. Surfaces, auth, authorization

These follow the protocol spec verbatim:

- [REST](../../protocol/rest.md) and [MCP](../../protocol/mcp.md) surfaces.
- [OAuth 2.1 authorization server](../../protocol/auth.md): DCR, PKCE S256,
  `resource`/audience binding, opaque server-stored tokens.
- [Capability URLs](../../protocol/capability-urls.md) bound (type, op) per
  connection.
- [Authorization / PEP](../../protocol/authorization.md): the grant ledger
  (client × op × type set × data window × lifetime × state), per-request
  enforcement, read-window auto-injection, immediate revocation.

### 6.1 Read data window — node specifics

The protocol leaves *how the owner expresses* a window implementation-defined.
This node:

- Lets the owner set an optional read window (lower/upper `occurred_at` bound) at
  **calendar-day granularity in the node's timezone** — `serve --tz <IANA>`,
  defaulting to the host's local time. `from` = local midnight; `to` = the
  inclusive end of the local day. Bounds are stored as true instants. (Not UTC —
  UTC-day rounding would strand the owner's local morning.)
- Applies the window to the **read** grant only (writes are never windowed).
- **Fails closed** on a malformed window (deny / 400), before any ledger change.
- **Preserves** the window across re-grants (consent re-issue, dashboard edit,
  owner-token re-mint) by pre-filling from the current read grant.

## 7. Owner authentication

The protocol marks owner authentication implementation-defined; this node uses a
**generated secret**:

- On first run the node generates a 32-byte URL-safe secret, prints it **once**,
  and stores only its sha256 hash (no recovery; rotate with `olf secret rotate`).
- Owner UI (`/authorize` consent, `/grants`, `/links`) requires a session from
  `POST /login` with that secret: cookie `olf_owner_session`
  (`HttpOnly`, `SameSite=Lax`, ~12h), stored as a hash.
- All owner state-changing POSTs carry a per-session **CSRF token**
  (constant-time compared); missing/mismatch → 403.
- Same-machine CLI is implicitly trusted (gated by data-dir access).
- Default bind is localhost; remote exposure is explicit (`--base-url` + TLS /
  tunnel). The owner-auth abstraction (`oauth.OwnerAuth`) can be upgraded to
  passkeys later without touching the contract.

## 8. Owner UI

Browser pages (all require owner login except `/login`):

| Path | Purpose |
|---|---|
| `GET /` | home; links to owner tools and endpoints |
| `GET/POST /login`, `POST /logout` | owner secret login / logout |
| `GET /grants` | connected apps (name, client_id, active grant count) |
| `GET/POST /grants/client?client_id=…` | per-app access: type×op checkboxes plus the read data window (prefilled, editable) |
| `GET /links` | capability URL builder |

## 9. Deployment

- **Single static binary**, zero external services: lifelog on local FS,
  metadata in embedded SQLite (pure-Go, `CGO_ENABLED=0`). Schemas and the IANA
  tz database are embedded (no external files needed for validation or `--tz`).
- First run bootstraps the data dir, generates the owner secret, creates
  `meta.db`, and binds localhost.
- CLI: `serve [--addr] [--data] [--base-url] [--tz]`,
  `token …` (owner token for CLI/cron), `secret rotate`.

## 10. Conformance checklist

A conforming node MUST satisfy, with tests:

1. `occurred_at` instant comparison: identical result regardless of offset text
   or precision.
2. Schema + semantic-invariant validation on every write.
3. Scope/grant enforcement: out-of-scope type/op/window/expiry/revocation are
   always denied (incl. read-after-revoke, end to end).
4. Token validation: expiry, **audience (`resource`) exact match** (surface +
   capability), PKCE S256 required, authorization code one-time, `redirect_uri`
   exact match.
5. Capability URL bound: `tools/list` / REST routes / consent / token audience
   all confined to the URL's (type, op).
6. Read-after-write strong consistency.
7. Read data window: no silent clipping (clipped reported), write/read-asymmetry
   warning, `404 not_found` vs `403 out_of_read_scope`, node-timezone day
   boundaries.
8. At-rest: owner secret, sessions, OAuth codes and tokens are stored as sha256
   hashes only.
