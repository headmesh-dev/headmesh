-- name: CreateAPIKey :one
INSERT INTO api_keys (org_id, name, prefix, key_hash, scopes, created_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetAPIKeyByHash :one
-- Runs in the system scope: the key itself is the credential that establishes the tenant scope.
SELECT * FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now());

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokeAPIKey :exec
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL;

-- name: ListAPIKeys :many
SELECT * FROM api_keys WHERE org_id = $1 ORDER BY created_at DESC;
