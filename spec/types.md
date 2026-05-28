# v1 types

This document defines the payload of each v1 type. The envelope fields
(including `occurred_at`, `recorded_at`, `tz`, `source`, `id`) are common to all
types and are specified in [Envelope](./envelope.md); they are not repeated in
the payload.

All payloads are **closed objects** (`additionalProperties: false`). To carry
data beyond what a standard type defines, use an
[extension type](./type-system.md#extension-types) rather than adding fields.

## Schema constraints vs. semantic invariants

Each type lists two kinds of rules:

- **Schema constraints** — enforced by the type's JSON Schema: presence
  (`required`), JSON type, string `format`, numeric range, enum membership, and
  presence/cardinality combinations (`anyOf` of `required`, `minItems`).
- **Semantic invariants** — cross-field or ordering rules a producer **MUST**
  uphold but that JSON Schema **cannot express** (notably: comparing two
  date-time values for ordering — JSON Schema has no relational comparison
  between fields). A reader **MUST NOT** assume these hold merely because a
  record passed schema validation; see
  [Type system § Validating a record](./type-system.md#validating-a-record).

## Units

OLF v1 uses **bare numbers with fixed, documented units** (the unit is implied
by the field name and fixed by this spec — there are no `{value, unit}` pairs):

| Quantity | Unit | Notes |
|---|---|---|
| body mass | kilograms | field suffix `_kg` |
| body fat | percent (0–100) | field suffix `_percent` |
| food mass | grams | field suffix `_g` |
| energy | kilocalories (kcal) | field suffix `_kcal` |
| step count | integer steps | unitless count |
| timestamps | ISO 8601 offset-bearing | as in the envelope |

A producer that has data in other units (e.g. pounds) MUST convert to the OLF
unit before writing. See [Open mHealth interop](./interop-open-mhealth.md) for
conversion guidance.

---

## `meal`

Something eaten, with optional itemization and macronutrient totals.
`occurred_at` is the time the meal was eaten.

**Schema constraints:**

| Field | JSON type | Req. | Constraints |
|---|---|---|---|
| `input_method` | string | OPTIONAL | one of `photo`, `text`, `manual` |
| `source_ref` | string | OPTIONAL | reference to the originating artifact (e.g. a photo id/URI) |
| `raw_input` | string | OPTIONAL | the user's original input, verbatim |
| `note` | string | OPTIONAL | free text |
| `total_kcal` | number | OPTIONAL | `>= 0` |
| `protein_g` | number | OPTIONAL | `>= 0` |
| `fat_g` | number | OPTIONAL | `>= 0` |
| `carbs_g` | number | OPTIONAL | `>= 0` |
| `items` | array of MealItem | OPTIONAL | see below |

**MealItem:**

| Field | JSON type | Req. | Constraints |
|---|---|---|---|
| `name` | string | MUST | non-empty (`minLength: 1`) |
| `grams` | number | OPTIONAL | `>= 0` |
| `kcal` | number | OPTIONAL | `>= 0` |
| `protein_g` | number | OPTIONAL | `>= 0` |
| `fat_g` | number | OPTIONAL | `>= 0` |
| `carbs_g` | number | OPTIONAL | `>= 0` |

Non-emptiness (schema-enforced via `anyOf`): at least one of `raw_input`, a
non-empty `items`, or any nutrition figure (`total_kcal`, `protein_g`, `fat_g`,
`carbs_g`) MUST be present. (A bare `source_ref`/`input_method` with no
substantive content is not a valid meal record.)

(No cross-field semantic invariants for producers; see the totals/items
interpretation note below.)

```json
{
  "input_method": "photo",
  "source_ref": "photo://abc123",
  "raw_input": "ramen and gyoza",
  "total_kcal": 850,
  "protein_g": 35,
  "fat_g": 28,
  "carbs_g": 95,
  "items": [
    { "name": "ramen", "grams": 500, "kcal": 600, "protein_g": 25, "fat_g": 18, "carbs_g": 70 },
    { "name": "gyoza", "grams": 150, "kcal": 250, "protein_g": 10, "fat_g": 10, "carbs_g": 25 }
  ]
}
```

Notes:

- `input_method` is named to avoid colliding with the envelope's `source` (which
  identifies the *client*, not how the meal was captured).
- Item order is significant and conveys position; items carry **no** `id` or
  `position` field (the array index is the position, and an item has no identity
  outside its meal).
- When both top-level totals and `items` are present, the **top-level totals are
  authoritative** for aggregation and `items` is the breakdown; OLF does **not**
  require (or verify) that item nutrients sum to the totals — rounding and
  independent estimates are expected. This is a reader-interpretation rule, not a
  producer invariant.

---

## `sleep`

One sleep **session**, optionally broken into stages. `occurred_at` is the start
of the session; the payload carries the session end and an optional stage
breakdown. A session is a single record (stages are embedded, not separate
records).

**Schema constraints:**

| Field | JSON type | Req. | Constraints |
|---|---|---|---|
| `ended_at` | string | MUST | `format: date-time`, offset-bearing |
| `stages` | array of SleepStage | OPTIONAL | — |

**SleepStage:**

| Field | JSON type | Req. | Constraints |
|---|---|---|---|
| `stage` | string | MUST | one of `in_bed`, `core`, `deep`, `rem`, `awake`, `asleep`, `unknown` |
| `started_at` | string | MUST | `format: date-time`, offset-bearing |
| `ended_at` | string | MUST | `format: date-time`, offset-bearing |

**Semantic invariants:**

- `ended_at` MUST be `>= occurred_at` (the session end is at or after its start).
- For each stage, `ended_at` MUST be `>= started_at`.
- Stages MUST be ordered by `started_at` (chronological).
- Each stage SHOULD fall within the session interval
  `[occurred_at, ended_at]`.
- **Stages MAY overlap and need not cover the whole session.** Real devices
  report an enclosing `in_bed` interval alongside finer `core`/`deep`/`rem`
  stages, and may leave gaps. Consequently a consumer **MUST NOT** assume stages
  partition the session, and **MUST** account for possible overlap when
  aggregating per-stage durations (e.g. do not naively sum stage durations).

```jsonc
// envelope.occurred_at = 2026-05-28T23:00:00+09:00 (session start)
{
  "ended_at": "2026-05-29T07:00:00+09:00",
  "stages": [
    { "stage": "in_bed", "started_at": "2026-05-28T23:00:00+09:00", "ended_at": "2026-05-29T07:00:00+09:00" },
    { "stage": "core",   "started_at": "2026-05-28T23:10:00+09:00", "ended_at": "2026-05-29T01:00:00+09:00" },
    { "stage": "deep",   "started_at": "2026-05-29T01:00:00+09:00", "ended_at": "2026-05-29T02:30:00+09:00" },
    { "stage": "rem",    "started_at": "2026-05-29T02:30:00+09:00", "ended_at": "2026-05-29T03:30:00+09:00" }
  ]
}
```

Stage start/end times cannot be derived from the envelope, so they are explicit
on each stage. Total sleep duration is derived (from the session interval and/or
the stages, accounting for overlap) and is not stored. Sources that report only
session bounds omit `stages`.

---

## `steps`

A step count over a time interval. `occurred_at` is the **start** of the
interval; the payload carries only the end.

**Schema constraints:**

| Field | JSON type | Req. | Constraints |
|---|---|---|---|
| `count` | integer | MUST | `>= 0` |
| `ended_at` | string | MUST | `format: date-time`, offset-bearing |

**Semantic invariants:**

- `ended_at` MUST be `>= occurred_at` (the interval end is at or after its
  start).

```jsonc
// envelope.occurred_at = 2026-05-28T08:00:00+09:00 (interval start)
{
  "count": 1234,
  "ended_at": "2026-05-28T09:00:00+09:00"
}
```

Multiple intervals in a day are multiple `steps` records. The interval duration
is derived from `occurred_at`..`ended_at` and is not stored.

---

## `weight`

A body-weight (and optional body-composition) measurement at a single instant.
`occurred_at` is the time of measurement.

**Schema constraints:**

| Field | JSON type | Req. | Constraints |
|---|---|---|---|
| `weight_kg` | number | MUST | `> 0` |
| `body_fat_percent` | number | OPTIONAL | `0` … `100` |
| `note` | string | OPTIONAL | free text |

(No cross-field semantic invariants.)

```json
{
  "weight_kg": 70.5,
  "body_fat_percent": 18.2,
  "note": "after waking"
}
```

BMI is **derived** (from weight and a height that lives outside OLF) and is not
stored. Additional body-composition fields (e.g. muscle mass) may be added in a
future minor version.
