# On-disk layout

OLF records are portable plain JSON. This document defines how a collection of
records is laid out as files so that the whole log is (a) carryable as a single
tree (`tar` it and move it), and (b) directly queryable by tools that read
JSON/JSONL from object storage or a local filesystem.

```
lifelog/
  meal/2026/05/2026-05-28.jsonl
  sleep/2026/05/2026-05-28.jsonl
  steps/2026/05/2026-05-28.jsonl
  weight/2026/05/2026-05-28.jsonl
  x.com.acme.mood/2026/05/2026-05-28.jsonl
  _manifest.json
```

## Partitioning

The partition key is **`type` + the local calendar date of `occurred_at`**:

```
<type>/<YYYY>/<MM>/<YYYY-MM-DD>.jsonl
```

- `<type>` is the full type identifier used verbatim as a single path segment.
  Extension types keep their dots (`x.com.acme.mood/…`); dots are not path
  separators.
- The date components come **directly from the date portion of `occurred_at`**.
  Because `occurred_at` is offset-bearing, its date portion already is the local
  wall-clock date — no time-zone math is needed to place a record.
- Granularity is **daily**: one file per `(type, local date)`.

### Interval and overnight events

Interval events (`steps`, `sleep`) are filed by the date of `occurred_at`, which
is the **start** of the interval. A sleep session that begins at
`2026-05-28T23:00:00+09:00` is therefore stored under `sleep/2026/05/2026-05-28`,
even though one might think of it as "the night of the 29th". This placement is
deterministic and requires no interpretation. Any "assign this sleep to the wake
day" logic is a **query-layer** concern and MUST NOT change where the record is
stored.

## File format

- Each partition file is **JSONL** (newline-delimited JSON): one complete OLF
  record (envelope + payload) per line.
- JSONL is append-friendly and is read directly by columnar query engines via
  globbing, so a log can be queried in place without a separate database.
- One-file-per-record layouts are **not** used: they explode file counts and are
  poor for both append and query.

## Mutation and uniqueness

- The canonical invariant: **a given `id` appears at most once** across the
  entire log.
- **Append** a new record by adding a line to the appropriate day file.
- **Edit or delete** a record by **rewriting that day file** (records are
  predominantly append-only; edits and deletes are rare). Rewriting keeps the
  file free of duplicates and tombstones, which keeps the canonical tree clean
  and human-inspectable.
- OLF 1.0 does **not** define append-only / last-write-wins / tombstone
  semantics for multi-writer or offline-sync scenarios. If a future deployment
  needs them, they are layered on by the runtime, not by this format.

## `_manifest.json`

OPTIONAL and **fully regenerable** from the record tree. It is informational —
a convenience index, never a source of truth. A reader MUST function correctly
when it is absent or stale.

Minimal shape:

```jsonc
{
  "format_version": "1.0",                  // OLF format version of the tree
  "generated_at": "2026-05-29T07:20:00+09:00",
  "types": {
    "sleep":  { "schema_versions": ["1.0"], "record_count": 412 },
    "weight": { "schema_versions": ["1.0"], "record_count": 365 }
  }
}
```

`record_count` and any further fields are OPTIONAL. Richer indexing structures
(precomputed aggregates, column stores, compaction artifacts) are explicitly a
**runtime** concern and are out of scope for this format specification.

## Access-control note

Because each `type` is the top-level directory, the `type` prefix doubles as a
natural access-control boundary: a store can grant read or write on a per-type
prefix (e.g. allow reading `sleep/**` but not `meal/**`) at the storage-ACL or
signed-URL level. OLF only fixes the layout; it does not mandate any particular
authorization mechanism.
