# OLF Protocol Specification

**open-lifelog protocol** — the network contract a client uses to read and write
[OLF records](../spec/README.md) over the wire: how to authenticate, how access
is scoped and consented, and the exact shape of the request/response surfaces.

- **Version**: 1.0 (draft)
- **Status**: Draft. The prose in this directory is the normative source of
  truth. It is **implementation-independent**: any node that serves OLF records
  over the network conforms to this contract, regardless of how it stores data
  or authenticates its owner.
- **License**: Apache-2.0 (see [`../LICENSE`](../LICENSE)).

This is a **companion** to the [format specification](../spec/README.md): the
format spec defines what a record *is*; this protocol spec defines how records
*move* between a client and a node. The two are versioned independently.

## Conformance language

The key words **MUST**, **MUST NOT**, **REQUIRED**, **SHALL**, **SHALL NOT**,
**SHOULD**, **SHOULD NOT**, **RECOMMENDED**, **MAY**, and **OPTIONAL** are to be
interpreted as described in
[RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) and
[RFC 8174](https://www.rfc-editor.org/rfc/rfc8174).

## Roles

- **Node** — the network endpoint that holds a single owner's records and serves
  this protocol.
- **Resource owner** — the person whose records the node holds and who approves
  client access.
- **Client** — any application (an LLM client, a third-party app, an ingestion
  adapter) that reads or writes through the node.

How the resource owner authenticates to the node, and how the node stores
records, are **implementation-defined** and out of scope here. This document
fixes only what a client observes on the wire.

## Surfaces

A node exposes the same core operations over two interchangeable surfaces. Both
are OAuth-protected and capability-scoped; neither carries business logic the
other lacks.

| Surface | Transport | Audience |
|---|---|---|
| [REST](./rest.md) | HTTP request/response (JSON) | apps, dashboards, ingestion adapters |
| [MCP](./mcp.md) | Model Context Protocol over Streamable HTTP | LLM clients |

Both surfaces read and write the **same records** under the **same
authorization**. A client chooses the surface that fits it; the data and the
access decision are identical.

## The three-stage narrowing

Every request is narrowed by three independent gates, in order. A request must
pass all three; each can only *restrict*, never *widen*.

1. **Capability bound** — the capability encoded in the connection URL caps what
   that connection can ever reach (type × operation). See
   [Capability URLs](./capability-urls.md).
2. **Consent (grant)** — the owner's live grant ledger decides what the
   authenticated client may do right now. Revocation is immediate. See
   [Authorization](./authorization.md).
3. **Read window** — an allowed read is further constrained to the grant's
   `occurred_at` window. See [Authorization](./authorization.md#read-window).

## Specification index

1. [Authentication](./auth.md) — OAuth 2.1 authorization server, dynamic client
   registration, PKCE, token audience binding, and metadata discovery.
2. [Capability URLs](./capability-urls.md) — the grammar that scopes a
   connection to a set of (type, operation) pairs, and its effects.
3. [REST surface](./rest.md) — endpoints, request/response bodies, the error
   envelope, and status codes.
4. [MCP surface](./mcp.md) — per-type tools, naming, annotations, embedded
   payload schemas, and tool-level error reporting.
5. [Authorization](./authorization.md) — the grant model, per-request
   enforcement, read-window injection, and immediate revocation.

## Records on the wire

A record is the JSON object defined by the [envelope](../spec/envelope.md) and
its type payload. On **write**, the node owns `id` and `recorded_at` — a client
MUST NOT supply them; the node assigns them and returns the complete record. On
**read**, the node returns complete records (envelope + payload) verbatim.

A client SHOULD read leniently: per the format spec's
[reader rule](../spec/type-system.md#additionalproperties), a reader MUST NOT
reject a record solely because its payload carries fields the reader's schema
does not define, as long as the record's major version is one the reader
supports.

## Version negotiation (reserved)

A future revision will define how a client and node negotiate which OLF payload
**major** versions are exchanged — e.g. per-type advertisement of supported
majors and a read-time version filter — so that a client can avoid records of a
major it cannot interpret. This is **reserved** and not specified in 1.0; the
reader-leniency rule above is the only version-handling requirement for now.
