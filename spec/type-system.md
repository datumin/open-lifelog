# Type system

OLF defines a record's shape with **JSON Schema**. Each `type` has a payload
schema; the envelope has its own schema. Together they validate a full record.

- Schema dialect: **JSON Schema draft 2020-12**.
- Schemas live in [`../schemas`](../schemas) and are published as part of this
  repository.

## Registry layout

```
schemas/
  envelope/1.json          # the common envelope schema
  meal/1.json
  sleep/1.json
  steps/1.json
  weight/1.json
  x.com.acme.mood/1.json   # third-party extension example
```

- One file **per type, per major version**: `schemas/<type>/<major>.json`.
- The file for major `N` always reflects the **latest minor** of major `N`.
- Each schema sets `$id` to a stable URI that includes the current
  `major.minor`, e.g.
  `https://open-lifelog.org/schemas/sleep/1.3`. The minor in `$id` is the source
  of truth for "what minor this file currently is". A human-readable changelog
  accompanies each schema (in the file via `$comment`, and in the repository
  CHANGELOG).

## Validating a record

To validate a record:

1. Validate the whole record against the envelope schema
   `schemas/envelope/1.json` (the envelope schema for OLF 1.x; see
   [Envelope evolution](#envelope-evolution)). This checks `id`, `type`,
   `olf_version`, timestamps, `source`, and that `payload` is an object.
2. Parse `type` and `olf_version`. Let `M` be the major component of
   `olf_version`.
3. Validate `payload` against `schemas/<type>/<M>.json`.

A producer MUST perform both steps at write time. A reader MUST be able to
perform both steps for the types it understands.

Schema validation is **necessary but not sufficient**. Several types carry
**semantic invariants** — cross-field or ordering rules (e.g. `ended_at >=
occurred_at`, sleep stages in chronological order) that JSON Schema cannot
express, because vanilla JSON Schema has no relational comparison between two
fields. A producer MUST enforce these in addition to schema validation, and a
reader MUST NOT assume they hold merely because a record validated. The
per-type invariants are listed in [v1 types](./types.md).

## Versioning

`olf_version` is `major.minor`. Changes to a payload schema are classified as:

| Change | Level | Examples |
|---|---|---|
| Backward-compatible (additive) | **minor** | add an OPTIONAL field; add an enum value; widen/relax a constraint |
| Breaking | **major** | remove or rename a field; make an OPTIONAL field REQUIRED; narrow a type or range; change units or semantics; remove an enum value |

Editorial changes (description text, examples, `$comment`) do **not** change the
version.

**Prefer minor and new types over a major bump.** A breaking **major** change is
a near-last-resort. A mature reader ecosystem on major `N` cannot read major
`N+1` records, and there is no flag-day migration — so a major bump risks
stranding data behind the very apps that made the type useful. Evolve a type by
**additive minors** (optional fields, widened constraints, new enum values)
wherever possible, and when a shape genuinely cannot fit a type, introduce a
**new extension or standard type** rather than breaking the existing one. When a
major bump is truly unavoidable, both majors **coexist** in the registry and
producers/consumers migrate independently (see [Multi-version coexistence](#multi-version-coexistence-read-rule)).

### Multi-version coexistence (read rule)

- Records are **immutable** and keep the `olf_version` they were written with.
- A reader **interprets a record using the latest published schema of the
  record's major version** it can obtain — i.e. `schemas/<type>/<M>.json`.
  Because every minor within a major is backward-compatible by construction, the
  latest `M.x` schema accepts all `M.y` (`y <= x`) records. A reader holding an
  older minor schema still **MUST NOT** reject a record merely for carrying
  fields that schema does not define (see [Closed for writers, lenient for
  readers](#additionalproperties)) — it simply cannot interpret those newer
  fields until it updates.
- Consequently a reader only needs **one schema per major** it supports, not one
  per minor. The precise minor in `olf_version` remains useful for
  **feature detection** (e.g. "this record may use a field introduced in 1.3").
- The format layer performs **no automatic migration** between versions.
  Up-converting old records, if ever desired, is the responsibility of runtime
  or external tooling, not of OLF.
- Multiple majors coexist in the registry (`1.json`, `2.json`, …). When a major
  is superseded, its file is **frozen** (never edited again).

### `additionalProperties`

Payload schemas set **`additionalProperties: false`**. This is safe given the
latest-per-major read rule (a reader always holds a schema that knows every
field of the relevant major), and it catches typos early.

The deliberate consequence: you **cannot** sprinkle ad-hoc fields into a
standard payload. To carry extra data, define an **extension type** (below)
rather than polluting a standard type's payload.

**Closed for writers, lenient for readers.** `additionalProperties: false` is a
**write-time** contract: it governs what a *producer* may write (enforced by
whoever validates the write — e.g. the runtime node, which holds current
schemas). It is **not** a license for *readers* to reject. A reader **MUST NOT**
reject a record solely because its payload carries fields its schema does not
define, as long as the record's major matches one the reader supports. This
makes within-major forward compatibility automatic and symmetric with the
envelope: an OPTIONAL field added in `M.1` does not break a reader that only
knows `M.0`. To *use* a field introduced in a later minor, a reader needs that
minor's schema (and SHOULD keep its schemas current); to merely *not break* on
it, leniency suffices. The **envelope makes the same trade** for the same reason
(see [Envelope evolution](#envelope-evolution)).

### Envelope evolution

The envelope is versioned by the format's own major version (OLF 1.x →
`schemas/envelope/1.json`). Within a major version, envelope changes are
**additive only** (new OPTIONAL fields). Readers MUST tolerate unknown envelope
fields so that an additive envelope change does not break older readers.
(`schemas/envelope/1.json` therefore does **not** set
`additionalProperties: false` at the envelope root.)

OLF 1.x records carry no explicit envelope-version field; the envelope major is
implicitly 1. A future breaking envelope change (OLF 2.0) would introduce an
explicit discriminator before freezing `envelope/1.json`.

## Namespacing

OLF uses a single flat `type` namespace split into two reserved regions.

### Standard types

- Match `^[a-z][a-z0-9_]*$` — a short, lowercase, dot-free name.
- Reserved exclusively by this specification. v1: `meal`, `sleep`, `steps`,
  `weight`.
- A producer MUST NOT invent new dot-free names; those are reserved for future
  standardization.

### Extension types

- Anyone may define a type **without a central registry** by using a reverse-DNS
  namespace they control:

  ```
  x.<reverse-dns-domain>.<name>
  ```

  - The literal prefix `x.` marks the name as an extension. It is **reserved**:
    standard types never begin with `x.`, so standard and extension names can
    never collide.
  - `<reverse-dns-domain>` is the author's domain in reverse order
    (`acme.com` → `com.acme`).
  - `<name>` is the extension's own type name.
  - Example: the owner of `acme.com` defines a "mood" type as
    **`x.com.acme.mood`**.
- Each segment matches `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`; segments are joined by
  `.`. Equivalently, the full `type` string matches
  `^x(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?){2,}$` (the `x` prefix followed by at least
  two segments: one or more domain segments and a name).
- Extension schemas live at `schemas/<full-type>/<major>.json`, e.g.
  `schemas/x.com.acme.mood/1.json`. The dots are part of the type identifier and
  are **not** turned into nested directories — the full type string is a single
  directory name. (This applies on disk too; see
  [On-disk layout](./on-disk.md).)

## Reference validation library

The **JSON Schemas** themselves are the normative, shipped artifact. Any consumer
can validate with an off-the-shelf JSON Schema validator in any language.

**Type bindings are not shipped as a maintained artifact.** Because generation is
mechanical (schema → types), consumers generate bindings for their own language on
demand and keep the schemas as the single source of truth — there is no
second source of truth to hand-maintain. This repository exercises that codegen
for Python (pydantic) as a schema-quality gate, but does not commit or publish the
output.

OLF does **not** ship hand-written, separately-maintained validator or binding
packages in v1. Validation is "load schema + run a standard validator"; type
bindings are a generate-on-demand convenience. The exact generators, package
names, and publication targets are an implementation concern (see the SP0
implementation plan).
