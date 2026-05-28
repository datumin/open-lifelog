# OLF Specification

This directory holds the **open-lifelog-format (OLF)** specification.

> Status: in progress. The sections below are the planned outline; content is
> authored in the SP0 work track.

## Outline

1. **Envelope** — the common fields every OLF record carries
   (`id`, `type`, `olf_version`, `occurred_at`, `recorded_at`, `source`, `payload`)
   and their exact semantics (id generation, timezone handling).
2. **Type system & JSON Schema registry** — how standard types are defined as
   semver'd JSON Schemas in [`../schemas`](../schemas), and how records are validated.
3. **Versioning** — compatibility policy (minor = backward-compatible additions,
   major = breaking), and how readers handle multiple coexisting versions.
4. **Extension namespacing** — reverse-DNS (`x.<reverse-dns>.<name>`) for
   third-party types; rules that keep the core namespace clean.
5. **On-disk layout** — how records are partitioned (by `type` and `occurred_at`
   date) for portability.

## v1 types

`meal` · `sleep` · `steps` · `weight`
