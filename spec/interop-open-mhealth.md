# Open mHealth interop (non-normative)

This appendix is **non-normative**. It documents how OLF's vital types relate to
[Open mHealth](https://www.openmhealth.org/documentation/#/schema-docs/schema-library)
and the schemas adopted into **IEEE 1752.1** (Standard for Mobile Health Data),
so that OLF data is *convertible* to and from those formats. OLF deliberately
does **not** adopt those schemas verbatim; see "Why not drop-in" below.

## Structural correspondence

Open mHealth models each data point as a **header** plus a **body**:

- **header** — metadata: an id, a creation timestamp, a `schema_id`
  (namespace + name + version), and acquisition provenance.
- **body** — the measurement itself, shaped by a schema-specific structure built
  from reusable blocks: `unit_value` (`{value, unit}`), `time_frame`
  (a `date_time` point or a `time_interval`), `duration_unit_value`, etc.

This is nearly the same split as OLF:

| Open mHealth | OLF |
|---|---|
| header | **envelope** |
| header `id` | `id` |
| header creation date-time | `recorded_at` |
| header `schema_id` (namespace + name + version) | `type` + `olf_version` |
| header acquisition source | `source` |
| body | **payload** |
| body `effective_time_frame` | `occurred_at` (+ payload `ended_at` for intervals) |

The key consequence: **OLF's envelope already carries the time frame**
(`occurred_at`/`ended_at`), so an OLF payload does not repeat it. A direct
embedding of an Open mHealth body would duplicate (and risk contradicting) the
envelope's timestamps.

## Per-type mapping

### `weight` ↔ Open mHealth `body-weight`

| OLF | Open mHealth |
|---|---|
| `payload.weight_kg` (number, kg) | `body_weight` = `{ value, unit }` with `unit: "kg"` |
| `payload.body_fat_percent` | (separate `body-fat-percentage` schema) |
| `occurred_at` | `effective_time_frame.date_time` |

### `steps` ↔ Open mHealth `step-count`

| OLF | Open mHealth |
|---|---|
| `payload.count` (integer) | `step_count` = `{ value, unit: "steps" }` |
| `occurred_at` … `payload.ended_at` | `effective_time_frame.time_interval` (`start_date_time` / `end_date_time`) |

### `sleep` ↔ Open mHealth / IEEE 1752.1 sleep schemas

| OLF | Open mHealth / 1752.1 |
|---|---|
| `occurred_at` … `payload.ended_at` (gross session interval) | `effective_time_frame.time_interval` |
| `payload.stages[]` (`stage`, `started_at`, `ended_at`) | per-stage durations / sleep-stage segments |

OLF stage names (`in_bed`, `core`, `deep`, `rem`, `awake`, `asleep`, `unknown`)
map to the corresponding sleep-stage categories; `core` corresponds to light
sleep.

Note on durations: the gross OLF session interval `occurred_at`..`ended_at`
corresponds to **time in bed / sleep period time**, *not* to Open mHealth's
`total_sleep_time` (TST). TST excludes intra-session wakefulness, so it must be
computed as the summed duration of the *asleep* stages (`core`/`deep`/`rem`,
excluding `awake` and the enclosing `in_bed`), accounting for overlap — it cannot
be read off the gross interval.

## Unit representation

The systematic difference is units. OLF uses **bare numbers with units fixed by
field name** (`weight_kg`, `*_g`, `*_kcal`); Open mHealth uses **`unit_value`
pairs** (`{ "value": 70.5, "unit": "kg" }`).

Converting OLF → Open mHealth: wrap each value in `{ value, unit }` with the unit
this spec fixes for that field. Converting Open mHealth → OLF: read the value,
convert to the OLF unit if necessary (e.g. lb → kg), and drop the unit wrapper.

## Why not drop-in

OLF favors its own clean shapes over verbatim Open mHealth bodies because:

1. **No time duplication** — the envelope owns the time frame; embedding Open
   mHealth's `effective_time_frame` would duplicate `occurred_at`.
2. **Producer ergonomics** — OLF's primary producers are programs (including LLM
   agents); bare `{"weight_kg": 70.5}` is far less error-prone to emit than
   nested `unit_value` objects.
3. **Smaller surface** — adopting Open mHealth verbatim pulls in its tree of
   shared sub-schemas (`time_frame`, `unit_value`, `duration_unit_value`, …),
   which is more than these four types need.

Interoperability is therefore delivered by **this mapping plus a converter**
(future tooling), not by making OLF a strict superset of Open mHealth.
