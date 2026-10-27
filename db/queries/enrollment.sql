-- name: CreateEnrollmentToken :one
INSERT INTO enrollment_tokens (org_id, token_hash, prefix, kind, roles, device_kind, max_uses, ephemeral, approval_required, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: ConsumeEnrollmentToken :one
-- Atomically spends one use of a live token. No row means invalid, revoked, expired or used up.
UPDATE enrollment_tokens SET uses = uses + 1
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now() AND uses < max_uses
RETURNING *;

-- name: RevokeEnrollmentToken :exec
UPDATE enrollment_tokens SET revoked_at = now() WHERE id = $1 AND org_id = $2 AND revoked_at IS NULL;

-- name: CreateEnrollment :one
INSERT INTO enrollments (org_id, token_id, cell_id, status, hostname, os, arch, client_ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: MarkEnrollmentMinted :one
UPDATE enrollments SET status = 'minted', preauth_key_id = $2 WHERE id = $1 AND status IN ('pending_approval', 'minted') RETURNING *;

-- name: CompleteEnrollment :one
UPDATE enrollments SET status = 'completed', headscale_node_id = $2, device_id = $3, completed_at = now()
WHERE id = $1 AND status = 'minted'
RETURNING *;
