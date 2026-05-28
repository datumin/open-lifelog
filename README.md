# open-lifelog

An open, portable format for personal lifelog data.

**open-lifelog** defines **OLF (open-lifelog-format)** — a language-neutral way to
represent everyday personal data (meals, sleep, steps, weight, and more) as typed,
versioned JSON records that you own and can carry between tools.

> This repository is the **format specification** only. The runtime
> (MCP server, query API, storage) lives in a separate repository.

## Design at a glance

Every OLF record shares a common **envelope** and carries a type-specific **payload**:

```jsonc
{
  "id": "uuid",
  "type": "sleep",                              // reverse-DNS for extensions: "x.acme.mood"
  "olf_version": "1.0",                         // semver of this type's schema
  "occurred_at": "2026-05-28T23:00:00+09:00",   // when it happened
  "recorded_at": "2026-05-29T07:10:00+09:00",   // when it was recorded
  "source": "my-client",                        // who recorded it
  "payload": { /* type-specific, validated by JSON Schema */ }
}
```

- **Typed & versioned**: each type is a semver'd JSON Schema in [`schemas/`](./schemas).
- **Open to extension**: third parties add types under a reverse-DNS namespace
  (`x.<reverse-dns>.<name>`) — no central registry needed, no collisions.
- **Portable**: records are plain JSON, organized by `type` and date, so the whole
  log can be carried (`tar`) and read by any tool that understands OLF.

## Planned v1 types

`meal` · `sleep` · `steps` · `weight`

Where applicable, payload shapes will align with existing health-data standards
(Open mHealth / IEEE 1752.1) to maximize interoperability.

## Status

Early design. The format specification and v1 schemas are in progress.

- [`spec/`](./spec) — format specification (envelope, versioning, namespacing rules)
- [`schemas/`](./schemas) — per-type JSON Schemas

## License

[Apache License 2.0](./LICENSE)
