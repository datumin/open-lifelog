# open-lifelog

An open, portable format for personal lifelog data.

**open-lifelog** defines **OLF (open-lifelog-format)** — a language-neutral way to
represent everyday personal data (meals, sleep, steps, weight, and more) as typed,
versioned JSON records that you own and can carry between tools.

> This repository holds two normative specs: the **format** ([`spec/`](./spec),
> what a record is) and the **protocol** ([`protocol/`](./protocol), how records
> move over the network — REST/MCP, OAuth, capability scoping, consent). A
> concrete runtime's storage and owner-authentication internals are
> implementation details and live elsewhere.

## Design at a glance

Every OLF record shares a common **envelope** and carries a type-specific **payload**:

```jsonc
{
  "id": "uuid",
  "type": "sleep",                              // reverse-DNS for extensions: "x.com.acme.mood"
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

## Install the node (`olf`)

`olf` is the reference OLF node (MCP / REST / OAuth / owner UI), shipped as a
single static binary.

```sh
curl -sSf https://raw.githubusercontent.com/datumin/open-lifelog/main/install.sh | sh
```

Or download a build for your platform from the
[Releases](https://github.com/datumin/open-lifelog/releases) page, or with Go:

```sh
go install open-lifelog.org/node/cmd/olf@latest
```

### Verify the download

Each release ships `checksums.txt` plus a cosign keyless signature
(`checksums.txt.sig` / `checksums.txt.pem`):

```sh
cosign verify-blob \
  --certificate checksums.txt.pem \
  --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/datumin/open-lifelog' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

The `olf` binary is versioned independently of the OLF format (semver,
starting at `v0.1.0`).

## Status

Early but functional. The format specification, the v1 JSON Schemas, and a
conformance test suite are in place.

- [`spec/`](./spec) — format specification (normative)
- [`protocol/`](./protocol) — protocol specification: REST/MCP surfaces, OAuth,
  capability scoping, consent (normative)
- [`schemas/`](./schemas) — per-type JSON Schemas
- [`conformance/`](./conformance) — language-neutral accept/reject fixtures

Typed bindings aren't shipped as a committed artifact — generate pydantic models
on demand from the schemas with `mise run gen` (output goes to `.gen/`, which is
gitignored). CI exercises this codegen as a schema-quality gate.

## License

[Apache License 2.0](./LICENSE)
