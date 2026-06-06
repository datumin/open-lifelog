# Authentication

A node is an OAuth 2.1 protected resource with a built-in authorization server.
Every request to a [REST](./rest.md) or [MCP](./mcp.md) surface MUST carry a
valid bearer token. This document defines how a client obtains and presents one.

## Authorization server

The node implements OAuth 2.1 (authorization code grant with PKCE) and the
endpoints the MCP authorization profile requires.

| Path | Purpose |
|---|---|
| `GET /.well-known/oauth-authorization-server` | Authorization-server metadata ([RFC 8414](https://www.rfc-editor.org/rfc/rfc8414)). `GET /.well-known/openid-configuration` is an alias with identical content. |
| `GET /.well-known/oauth-protected-resource[/mcp[/{capability}]]` | Protected-resource metadata ([RFC 9728](https://www.rfc-editor.org/rfc/rfc9728)) for the MCP surface. The path variants accommodate differing client discovery styles. |
| `GET /.well-known/oauth-protected-resource/api/{capability}` | Protected-resource metadata for the REST surface. REST is always capability-scoped, so only the per-capability form exists. |
| `POST /register` | Dynamic Client Registration ([RFC 7591](https://www.rfc-editor.org/rfc/rfc7591)). |
| `GET /authorize` | Authorization endpoint. Requires the resource owner to be signed in; an unauthenticated owner is redirected to sign in first. |
| `POST /authorize` | Receives the owner's consent decision. |
| `POST /token` | `authorization_code` and `refresh_token` grants. |

How the resource owner signs in to approve a request is implementation-defined
and not observable by the client.

## Client registration

- Clients register dynamically via `POST /register`.
- The node issues **public clients** only (`token_endpoint_auth_method: none`);
  there is no client secret. Clients MUST use PKCE to protect the code exchange.

## Authorization code

- `response_type=code` only.
- **PKCE with `S256` is REQUIRED.** A missing `code_verifier`/`code_challenge`
  or `code_challenge_method=plain` MUST be rejected.
- `redirect_uri` MUST match a registered URI **exactly**. A mismatch MUST NOT
  redirect to the supplied URI; the node responds with `400` directly.
- An authorization code is **single-use** (no replay) and **short-lived**
  (on the order of one minute).

## Tokens

- Access tokens and refresh tokens are opaque, high-entropy strings. A client
  MUST treat them as opaque and MUST NOT parse them.
- A refresh grant issues a new access token.
- Token lifetime and rotation are implementation details; a client MUST handle
  expiry by refreshing or re-authorizing.

## Audience binding (RFC 8707)

Tokens are bound to the exact URL they are for, so a token minted for one
connection cannot be replayed against another.

- A client declares the target URL with the `resource` parameter at
  authorization time (`/authorize`). The node binds that `resource` to the
  authorization code and carries it through token exchange and refresh, so the
  issued token is pinned to it ([RFC 8707](https://www.rfc-editor.org/rfc/rfc8707)).
- A resource server (`/mcp`, `/mcp/{capability}`, `/api/{capability}`) MUST
  verify that the presented token's stored `resource` **exactly matches the URL
  being connected to**.
- The bound `resource` includes **both the surface segment (`mcp` / `api`) and
  the capability**. Therefore:
  - a token for capability *A* is rejected on capability *B*, and
  - a token for one surface (e.g. `/api/meal:rw`) is rejected on the
    corresponding other surface (`/mcp/meal:rw`).

## Presenting a token (bearer validation)

- A client MUST send `Authorization: Bearer <token>` on every request.
- On a validation failure — missing, malformed, expired, or audience mismatch —
  the node responds `401` with a
  `WWW-Authenticate: Bearer resource_metadata="<metadata-url>"` header. The
  client discovers the authorization server from that metadata URL.
- On the REST surface the `401` body is the JSON [error envelope](./rest.md#errors);
  on the MCP surface authentication failures are transport-level `401`s (a
  client that is already connected sees authorization failures as
  [tool errors](./mcp.md#errors) instead).
