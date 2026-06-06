# OLF Specification

**open-lifelog-format (OLF)** — an open, portable, language-neutral format for
personal lifelog data: typed, versioned JSON records that you own and can carry
between tools.

- **Version**: 1.0 (draft)
- **Status**: Draft. The prose specification in this directory is the normative
  source of truth. The JSON Schema files in [`../schemas`](../schemas) implement
  it and the [`../conformance`](../conformance) fixtures verify them. Type
  bindings are generated mechanically from the schemas on demand, not shipped.
- **License**: Apache-2.0 (see [`../LICENSE`](../LICENSE)).

This directory is the **format specification**: what a record is. The companion
[protocol specification](../protocol/README.md) defines how records move over the
network (authentication, capability scoping, consent, and the REST/MCP
surfaces). A concrete runtime's storage and owner-authentication internals are
implementation details, out of scope for both.

## Conformance language

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**,
**SHOULD**, **SHOULD NOT**, **RECOMMENDED**, **MAY**, and **OPTIONAL** in this
specification are to be interpreted as described in
[RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) and
[RFC 8174](https://www.rfc-editor.org/rfc/rfc8174).

## The data model at a glance

Every OLF record is a single JSON object with a common **envelope** and a
type-specific **payload**:

```jsonc
{
  "id":          "0192f3a0-7b1e-7c3d-8e4f-0a1b2c3d4e5f", // UUID (v7 recommended)
  "type":        "sleep",                                 // standard short name, or x.<reverse-dns>.<name>
  "olf_version": "1.0",                                   // semver of this type's payload schema
  "occurred_at": "2026-05-28T23:00:00+09:00",             // when it happened (offset-bearing)
  "recorded_at": "2026-05-29T07:10:00+09:00",             // when it was recorded
  "tz":          "Asia/Tokyo",                            // IANA zone (optional, SHOULD be present)
  "source":      "mobile-app",                            // client identifier slug
  "payload":     { /* type-specific, validated by JSON Schema */ }
}
```

## Specification index

1. [Envelope](./envelope.md) — the common fields every record carries and their
   exact semantics.
2. [Type system](./type-system.md) — JSON Schema registry layout, semver
   compatibility policy, multi-version read rules, extension namespacing, and
   the reference validation library.
3. [On-disk layout](./on-disk.md) — how records are partitioned into files for
   portability and direct query.
4. [v1 types](./types.md) — normative payload definitions for `meal`, `sleep`,
   `steps`, and `weight`.
5. [Open mHealth interop](./interop-open-mhealth.md) — non-normative mapping
   between OLF vital types and Open mHealth / IEEE 1752.1.

## v1 type set

`meal` · `sleep` · `steps` · `weight`

These are **observation/event** types: each record describes something that
happened at a time (`occurred_at`). Current-state configuration (user profile,
goals) and derived values are **out of scope** for OLF; they belong to a
consuming application, not to the lifelog.

## Versioning at a glance

- Each type's payload schema is versioned **semver-style** (`major.minor`, no
  patch).
- A record carries its `olf_version`; records are **immutable** and keep the
  version they were written with.
- Backward-compatible changes bump **minor**; breaking changes bump **major**.
- A reader validates a record against the **latest schema of the record's
  major version**. See [Type system](./type-system.md) for the full policy.

## Conformance targets

- A **producer** MUST emit records whose envelope conforms to
  [Envelope](./envelope.md), whose payload validates against the selected type
  schema, and which satisfy that type's
  [semantic invariants](./types.md#schema-constraints-vs-semantic-invariants)
  (schema validation is necessary but not sufficient).
- A **reader** MUST select the schema per the rules in
  [Type system](./type-system.md) and MUST ignore record kinds (`type`s) it does
  not understand rather than rejecting the whole log.
- A **store** that organizes records on disk MUST follow
  [On-disk layout](./on-disk.md).
