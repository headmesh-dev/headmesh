# Browser sign-in

The API serves `/oauth/login`, `/oauth/account`, `/oauth/logout` and provider
callbacks. GitHub and Hugging Face establish a Headmesh identity. Signing in
does not create an organization membership or grant access to a sandbox.

Register apps with your API's public HTTPS origin:

| Provider | Registration | Callback path |
| --- | --- | --- |
| GitHub | https://github.com/settings/applications/new | `/oauth/callback/github` |
| Hugging Face | https://huggingface.co/settings/applications/new | `/oauth/callback/huggingface` |

GitHub uses `read:user` and `user:email`. Hugging Face uses `openid`, `profile`
and `email`; no repository, inference or billing permissions are requested.
Both flows use PKCE S256. Hugging Face ID tokens are checked against discovered
signing keys, issuer, client audience and a per-login nonce; userinfo subjects
must match. Either provider must return a verified email.

Enable GitHub with `HEADMESH_GITHUB_CLIENT_ID` and
`HEADMESH_GITHUB_CLIENT_SECRET_FILE`. Enable Hugging Face with
`HEADMESH_HF_LOGIN_ENABLED=true`, `HEADMESH_HF_CLIENT_ID` and, for a
confidential app, `HEADMESH_HF_CLIENT_SECRET_FILE`.
Secret files must be private regular files (0600) readable by the backend user.
Never commit secrets or expose them in frontend variables.

Hugging Face also supports public CIMD registration: leave its client ID and
secret file empty, and the API uses `/.well-known/oauth-cimd` with PKCE.
Keep the registered ID when using an existing app. Production and test need
separate registered callbacks and credentials.

Migration 5 adds one-use browser-bound login flows, identities keyed by
immutable provider subjects, and hashed browser sessions. Sessions expire
after 12 hours, rotate on sign-in/linking, and are revoked at logout.
Matching emails never automatically merge accounts. Sign in with the original
provider, then explicitly link the other provider from the account page.

Browser login establishes identity. The separately configured
[MCP authorization and resource servers](mcp-oauth.md) issue and validate sandbox
access tokens after an explicit grant and consent. Both components must be
deployed and verified together before relying on OAuth for MCP access.

## MCP Registry

`sandbox/server.json` describes the sandbox for the official MCP Registry.
Users supply their own sandbox hostname and scoped client token. The listing
contains no token and grants no access to a shared machine. Validate through
the official registry's `POST /v0.1/validate` endpoint; publish with namespace
authentication through `mcp-publisher`. A registry entry and acceptance into a
curated directory are separate milestones.
