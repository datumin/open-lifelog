# Contributing to open-lifelog

Thanks for your interest in OLF (open-lifelog-format). This repository is the
**format specification** — the prose spec in [`spec/`](./spec), the JSON Schemas
in [`schemas/`](./schemas), and a language-neutral conformance suite. There is no
runtime here.

## Ground rules

- **The prose spec is normative.** `spec/*.md` is the source of truth; the JSON
  Schemas implement it and the conformance fixtures verify them.
- **Type bindings aren't committed.** `mise run gen` generates pydantic models
  into `.gen/` (gitignored) on demand. CI doesn't check committed bindings — it
  runs the codegen as a schema-quality gate (every schema must codegen cleanly
  and round-trip real data), so there's nothing to hand-edit or keep in sync.
- **Keep it lifelog-pure.** OLF models timestamped personal observations only.
  Application-specific concepts (user profiles, goals, derived/computed values)
  are out of scope.

## Development setup

Tooling is managed by [mise](https://mise.jdx.dev/) (Python via `uv`):

```bash
mise install        # install toolchains
mise run install    # uv sync
mise run test       # conformance + schema tests (incl. the codegen gate)
mise run lint       # ruff
mise run typecheck  # ty
mise run gen        # generate pydantic bindings into .gen/ (optional, local use)
```

## Proposing a change

Which change you make determines the version bump (see
[`spec/type-system.md`](./spec/type-system.md) for the full policy):

- **Backward-compatible (minor):** add an OPTIONAL field, add an enum value, or
  relax a constraint. Edit `schemas/<type>/<major>.json` in place, add
  conformance fixtures (valid **and** invalid), and add a CHANGELOG entry.
- **Breaking (major):** removing/renaming a field, tightening a type, or changing
  units/semantics. Create a new file `schemas/<type>/<N+1>.json`; leave the prior
  major frozen.
- **A brand-new standard type:** standard (dot-free) names are reserved — open an
  issue to discuss before adding one.
- **An experimental or org-specific type:** you do **not** need approval. Define
  it under a reverse-DNS namespace, e.g. `x.com.example.mood`, per
  [`spec/type-system.md`](./spec/type-system.md#namespacing).

## Pull request checklist

- [ ] `mise run test` passes (valid fixtures accepted, invalid rejected; every
      schema codegens cleanly and round-trips).
- [ ] `mise run lint` and `mise run typecheck` pass.
- [ ] Prose spec in `spec/` updated to match any schema change.
- [ ] `CHANGELOG.md` updated.

## License

By contributing, you agree that your contributions are licensed under the
[Apache License 2.0](./LICENSE).
