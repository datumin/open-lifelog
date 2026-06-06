# REST surface

The REST surface is a thin HTTP/JSON translation over the node's core read and
write operations. It is always [capability-scoped](./capability-urls.md): every
path is of the form `/api/{capability}/…`. There is **no un-scoped `/api`
endpoint** — use a `*:rw` (or `*:r`) capability to reach every type.

Every request is subject to the [three-stage narrowing](./README.md#the-three-stage-narrowing)
and MUST carry a valid [bearer token](./auth.md#presenting-a-token-bearer-validation).

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/{capability}/query/{type}` | List `{type}` records. Optional `?from=&to=`. |
| `GET` | `/api/{capability}/query/{type}/{id}` | Get one record by id. |
| `POST` | `/api/{capability}/records/{type}` | Create a record. |
| `PUT` | `/api/{capability}/records/{type}/{id}` | Replace an existing record. |
| `DELETE` | `/api/{capability}/records/{type}/{id}` | Delete a record (`204` on success). |

## Response envelope

Every successful read and write returns the same shape: a `data` field and a
`meta` field. The envelope exists to make implicit behavior explicit — a client
can tell "no results because nothing happened" apart from "no results because
they're outside my read window", and can learn at write time that a record landed
somewhere it will never be able to read back.

```jsonc
// list — data is the (possibly empty) array of records
{
  "data": [ /* records */ ],
  "meta": {
    "requested_range": { "from": "2026-06-01T00:00:00Z", "to": null },
    "effective_range": { "from": "2026-06-06T00:00:00Z", "to": null },
    "clipped": true,                                  // the read window narrowed the request
    "warnings": [ { "code": "range_clipped_by_scope", "message": "…" } ]
  }
}
// get success / create / update — data is the single record
{ "data": { /* record */ }, "meta": { "warnings": [ /* e.g. written_outside_read_window */ ] } }
```

- **List** never silently clips: `meta.clipped` is `true` when the grant's read
  window narrowed the requested range, and `requested_range`/`effective_range`
  show exactly what was asked for versus served. `clipped: true` with empty
  `data` means "records exist outside your window", not "nothing happened".
- **Create/Update** attach a `written_outside_read_window` warning when the new
  record's `occurred_at` is outside the caller's **read** window — i.e. it was
  saved but the caller cannot read it back (see
  [the write/read asymmetry](./authorization.md#read-window)).

### List range

`from` and `to` are **offset-bearing RFC 3339** instants and bound `occurred_at`
**inclusively**. Either MAY be omitted for an unbounded side. The range is
evaluated by true instant (offset-independent); see the format spec's
[occurred_at semantics](../spec/envelope.md). The effective range is the
intersection of the requested range and the grant's
[read window](./authorization.md#read-window); `meta.clipped` reports whether that
intersection removed anything.

## Write body

`POST` and `PUT` take a JSON object with the writable envelope fields and the
type payload:

```jsonc
{
  "olf_version": "1.0",                       // OPTIONAL; defaults to the node's current version
  "occurred_at": "2026-05-28T07:05:00+09:00", // REQUIRED; offset-bearing RFC 3339
  "tz": "Asia/Tokyo",                         // OPTIONAL; IANA zone
  "source": "my-app",                         // client identifier slug
  "payload": { /* type-specific */ }          // validated against the type schema
}
```

- `id` and `recorded_at` are **node-assigned**; a client MUST NOT send them. The
  node mints `id` and sets `recorded_at`, then returns the complete record under
  `data` (with `meta` per the [envelope](#response-envelope)).
- The payload is validated against the type's JSON Schema for `olf_version`'s
  major, plus the type's semantic invariants. A failure is a `400`.

## Errors

Every error response uses `Content-Type: application/json` and the envelope:

```jsonc
{ "error": { "code": "<stable machine code>", "message": "<human reason>" } }
```

The `code` is stable and meant for branching; `message` is for humans. This holds
for all error statuses, including `401` (which additionally carries the
`WWW-Authenticate` header). The out-of-read-scope `GET` additionally carries the
granted window and the record's `occurred_at`:

```json
{
  "error": {
    "code": "out_of_read_scope",
    "message": "this record exists but is outside your granted read window",
    "granted_read_window": { "from": "2026-06-06T00:00:00Z", "to": null },
    "record_occurred_at": "2026-06-05T12:00:00+09:00"
  }
}
```

## Status codes

| Status | `code` | When |
|---|---|---|
| `200 OK` | — | successful `GET` / `PUT` |
| `201 Created` | — | successful `POST` |
| `204 No Content` | — | successful `DELETE` |
| `400 Bad Request` | `bad_request` | validation failure, malformed body, or invalid range |
| `401 Unauthorized` | `unauthenticated` | missing / invalid / expired / wrong-audience token |
| `403 Forbidden` | `forbidden` | (operation, type) outside the capability bound, or no active grant |
| `403 Forbidden` | `out_of_read_scope` | `GET …/{id}` on a record that **exists** but is outside the read window |
| `404 Not Found` | `not_found` | the id does not exist, unknown type, or unknown/malformed capability |

Note the deliberate split on `GET …/{id}`: a non-existent id is `404 not_found`,
while a record that exists but is outside the read window is `403
out_of_read_scope`. This **discloses the existence** of unreadable records — a
deliberate choice for a single-owner personal node (see
[Read window](./authorization.md#read-window)).
