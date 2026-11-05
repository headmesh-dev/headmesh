# MCP OAuth

Headmesh's authorization server connects a signed-in user to a sandbox through
an explicit resource grant and a consent page. GitHub and Hugging Face establish
identity; they do not grant machine access. Neither provider's token is accepted
as a sandbox credential or forwarded to a sandbox.

The server uses the plan's pinned `zitadel/oidc` v3.51.16 provider. It implements
authorization code with PKCE S256, RFC 8707 resource binding, RFC 9207 issuer
identification, public CIMD clients, and rate-limited public DCR for compatibility.
Discovery advertises only these implemented grants. Device flow and platform
client credentials remain separate future work.

## Enable a self-hosted installation

Apply migration 6. Configure two different private files containing independently
generated, base64-encoded 32-byte values. The OAuth signing seed must also differ
from the control plane's operation-signing key. Set:

```dotenv
HEADMESH_OAUTH_ENABLED=true
HEADMESH_OAUTH_SIGNING_KEY_FILE=/etc/headmesh/oauth/signing-key
HEADMESH_OAUTH_CRYPTO_KEY_FILE=/etc/headmesh/oauth/encryption-key
```

Files must be private regular files (0600 or stricter), readable by the backend.
Configure browser sign-in as described in [browser-sign-in.md](browser-sign-in.md).
Public metadata is served at `/.well-known/oauth-authorization-server` and
`/.well-known/openid-configuration` under `HEADMESH_PUBLIC_URL`.

An operator with `HEADMESH_DATABASE_OWNER_URL` can grant a linked identity access:

```sh
headmesh-server oauth-grant -provider github -subject PROVIDER_NUMERIC_ID \
  -resource https://YOUR_SANDBOX_HOST/mcp -name 'My sandbox' \
  -scopes sandbox:read,sandbox:exec,sandbox:write
```

The subject is the provider's immutable account ID, not an email. The command
requires an existing enabled account and never creates a sign-in identity.
Use the same identity/resource with `-revoke` to remove its access immediately.
No public sign-up endpoint grants access to another user's machine.

Configure the sandbox resource server:

```toml
[sandboxd.oauth]
enabled = true
issuer = "https://YOUR_API_HOST"
jwks_url = "https://YOUR_API_HOST/oauth/jwks"
resource = "https://YOUR_SANDBOX_HOST/mcp"
authorization_servers = ["https://YOUR_API_HOST"]
```

The public JWKS must share the issuer's HTTPS origin. Existing scoped `hms_`
credentials continue to work. OAuth grants support `sandbox:read`, `sandbox:exec`
and `sandbox:write`; they never confer `sandbox:admin`.

## Token lifecycle

Access JWTs use Ed25519, last 15 minutes, and are bound to exactly one sandbox
resource. The resource server verifies signature, algorithm, issuer, audience,
expiry, lifetime, scope and access-token type. ID tokens are rejected. It then
checks `/oauth/token-status` using that same bearer credential; this endpoint
only reports the status of the credential the caller already holds. Revocations,
disabled accounts, changed grants and reused refresh families therefore stop
existing access immediately. An unavailable issuer denies OAuth access.

Refresh credentials are stored by SHA-256 digest, rotate on every use, and belong
to a fixed 30-day family. Reusing an old credential revokes that family. Scopes
can shrink during refresh; they cannot expand. Requests must preserve the client
and exact resource binding. `/oauth/revoke` supports access and refresh tokens.
Previously active public signing keys remain published for the access lifetime
plus one minute when a new signing seed is deployed.

## Verification

```sh
go test -race -tags integration ./api/login ./api/oauth
cd sandbox && go test -race ./sandboxd/authn ./sandboxd/server
```

Integration tests use real PostgreSQL with a non-superuser, non-BYPASSRLS
application role. They exercise consent and refusal, PKCE, code races/replay,
resource binding, refresh rotation/reuse, resource-grant RLS and immediate
revocation. Resource-server tests reject forged signatures, ID tokens, foreign
audiences/issuers, unbounded lifetimes, scope escalation and issuer outages.
Production acceptance still requires a live authorization/refresh/MCP/revocation
rehearsal; unit tests and directory publication alone do not establish it.
