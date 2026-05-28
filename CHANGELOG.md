# Changelog

All notable changes to OLF (the format specification and its JSON Schemas) are
documented here. The format is versioned per type with semver-style
`major.minor`; see [`spec/type-system.md`](./spec/type-system.md). This file
follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [1.0] - 2026-05-29 (draft)

Initial draft of OLF (open-lifelog-format).

### Added
- **Envelope** specification: `id`, `type`, `olf_version`, `occurred_at`,
  `recorded_at`, `tz`, `source`, `payload`.
- **Type system**: JSON Schema (draft 2020-12) registry at
  `schemas/<type>/<major>.json`, semver-style compatibility policy,
  multi-version read rules, and reverse-DNS extension namespacing
  (`x.<reverse-dns>.<name>`).
- **On-disk layout**: daily JSONL partitioned by the local date of `occurred_at`.
- **v1 payload types**: `meal`, `sleep`, `steps`, `weight`.
- **Conformance suite**: language-neutral accept/reject fixtures with a pytest
  runner, plus a two-step record validator.
- **Type bindings**: generated Python and TypeScript types.
- Non-normative [Open mHealth / IEEE 1752.1 interop mapping](./spec/interop-open-mhealth.md).
