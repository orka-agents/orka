# ADR 0055: Serve OAuth protected-resource metadata for the MCP server

Date: 2026-11-26

## Status

Accepted. Partly supersedes [0043](0043-control-plane-mcp-server.md) for MCP OAuth
discovery. Adds opt-in OAuth 2.0 protected-resource metadata (RFC 9728) and bearer
challenges for `POST /mcp` when `--mcp-resource-url` is configured alongside
`--mcp-server-enabled` and OIDC.

## Context

MCP clients that implement MCP authorization look for OAuth 2.0 protected-resource
metadata (RFC 9728) when a request is refused. Without it, such a client cannot find
the identity provider, and people paste a bearer token that never refreshes.

## Decision

- With `--mcp-server-enabled`, OIDC configured, and `--mcp-resource-url`, the API
  serves `GET /.well-known/oauth-protected-resource/mcp` without authentication. It
  names the resource URL, `--oidc-issuer` as the only authorization server, the scopes
  clients should request, and header bearer tokens. It holds nothing secret.
- An authentication refusal of `POST /mcp` carries `WWW-Authenticate: Bearer
  resource_metadata="<metadata URL>", scope="<scopes>"`. Other routes and other
  refusals (`403`, `503`) are unchanged.
- `--oidc-scopes` sets the scopes, defaulting in the metadata to `openid profile email
  offline_access`. When set, `GET /api/v1/auth/config` advertises it, and
  `orka auth login` requests it for the advertised issuer.
- Nothing about token validation changes. Orka is not an authorization server: it
  registers no clients, issues and exchanges no tokens, and checks a token from an MCP
  client exactly as any OIDC token, including the exact `--oidc-audience`. A provider
  that sets `aud` to the client ID therefore needs MCP clients to use the
  installation's own public client, whose ID is the audience; providers without
  dynamic client registration need that client ID configured in the client.
- The resource URL must use `https` (loopback `http` is accepted only as a nonstandard
  local-development mode; RFC 9728 requires `https`), a non-empty hostname with no
  explicit default port (`:443` / `:80`) and a valid port if specified, and the path
  `/mcp`. RFC 9728 derives the metadata location from the resource path, so a path prefix
  would need the ingress to route a second well-known path; that is left out until
  someone needs it. Startup fails if the URL is invalid, or set without the endpoint or
  OIDC.

## Consequences

- MCP clients with OAuth support can discover the installation's OIDC issuer and
  refresh tokens automatically without changing Orka's REST/MCP token validation model.
- Installations without `--mcp-resource-url` keep the existing ADR 0043 behavior
  (no metadata route and no `WWW-Authenticate` challenge).
- Path-prefixed MCP ingress routes are not supported until a dedicated ingress/routing
  design is needed.
