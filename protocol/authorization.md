# Authorization

Authentication ([auth](./auth.md)) establishes *who* a client is. Authorization
decides *what* that client may do, and is enforced by the node on every request.
A valid token is necessary but not sufficient: the owner's live consent decides
access, and it can be withdrawn at any moment.

## Grant model

A **grant** records that the owner has allowed a client some access. It is the
combination of:

- **client** — the registered client the grant applies to;
- **operation** — `read` or `write`;
- **type set** — the OLF types covered (a wildcard `*` covers all);
- **data window** — an `occurred_at` range the read may touch (an open upper
  bound means "and all future records"); see [read window](#read-window);
- **lifetime** — an optional expiry (none means "until revoked");
- **state** — active or revoked.

The **data window** and the **lifetime** are distinct concepts: the window
bounds *which records* are visible; the lifetime bounds *how long* the grant
lasts.

## Scope vocabulary

Requested and granted scopes use `lifelog:<read|write>:<type>`, with `*` as a
type wildcard (e.g. `lifelog:read:*`). Scope is the coarse front gate; the grant
ledger's window × lifetime × state is the authoritative decision.

## Obtaining a grant (consent)

1. **Request** — the client declares the scope it wants when it starts the
   [authorization code flow](./auth.md#authorization-code).
2. **Consent** — the resource owner reviews the request and narrows it (by type
   and operation, and MAY bound the [read data window](#read-window)). How the
   owner is presented this decision is implementation-defined.
3. **Issue** — the node records a grant and issues a token.

The granted access is the **intersection** of two narrowings: the
[capability URL](./capability-urls.md) bound and the owner's consent. A client
that requests `read:*` may be cut down to `read:meal` by either the URL or the
owner.

## Per-request enforcement

On every request the node:

1. reads the client identity from the presented token;
2. checks the request's (operation, type) against the **capability bound** —
   outside it is denied (`403` on REST; a tool error on MCP);
3. consults the **grant ledger** for `(client, operation, type)` — no active
   grant is denied;
4. for an allowed read, applies the grant's [read window](#read-window).

Enforcement happens **inside the node that holds the data**. The node does not
proxy requests to a separate consent service, and it does not relay data through
a third party.

## Read window

The data window constrains **reads only** — a write grant is never bounded by it
(a window limits what data is disclosed, not what may be added). An allowed read
is constrained to the grant's `occurred_at` window, intersected with any range the
client requested:

- **List** returns only records whose `occurred_at` falls within the
  intersection, and **never clips silently**: the response reports the requested
  vs effective range and a `clipped` flag, so a caller can tell "0 results
  because nothing happened" from "0 results because they're outside my window"
  (see the [response envelope](./rest.md#response-envelope)).
- **Get** distinguishes a non-existent id (`404 not_found`) from a record that
  **exists but is outside the window** (`403 out_of_read_scope`, carrying the
  granted window and the record's `occurred_at`). This **discloses the existence**
  of unreadable records. For a single-owner personal node that is the intended
  trade: knowing "there is older data you cannot currently see" is more useful to
  the owner's own tools than hiding it, and the consent window — not obscurity —
  is the access control. (A multi-tenant deployment with a different threat model
  MAY instead collapse both to `404`.)

Because reads disclose that out-of-window data exists (via `clipped` and the
`out_of_read_scope` error), a windowed grant bounds **what a client can read**,
not whether it can tell that more exists. Writes are never windowed, so a client
can create a record outside its own read window; the write response warns when
that happens (`written_outside_read_window`) so "saved but unreadable" is caught
at write time rather than discovered later as a confusing empty read.

Window bounds are compared as **true instants** (offset-independent), exactly like
`occurred_at`. How the owner expresses a bound is implementation-defined; if an
owner-supplied window is malformed, the node MUST fail closed (deny rather than
grant a wider window than intended). When several grants authorize the same read,
their windows are **intersected** (most restrictive wins), so the decision never
fails open on grant overlap.

## Immediate revocation

Revoking a grant takes effect immediately: the next request from that client for
the affected access is denied. There is no revocation list to propagate and no
waiting for a token to expire — a read issued after revocation MUST be denied.
