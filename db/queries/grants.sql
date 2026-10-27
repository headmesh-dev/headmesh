-- name: CreateGrant :one
-- Idempotent on (grantor, external_ref): a retried create returns no row, and the caller loads the existing one.
INSERT INTO grants (grantor_org_id, grantee_org_id, src, dst_device_id, ports, starts_at, expires_at, reason, external_ref, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (grantor_org_id, external_ref) WHERE external_ref IS NOT NULL DO NOTHING
RETURNING *;

-- name: GetGrant :one
SELECT * FROM grants WHERE id = $1;

-- name: GetGrantByExternalRef :one
SELECT * FROM grants WHERE grantor_org_id = $1 AND external_ref = $2;

-- name: RevokeGrant :one
UPDATE grants SET revoked_at = now(), revoked_by = $2
WHERE id = $1 AND revoked_at IS NULL
RETURNING *;

-- name: ExtendGrant :one
UPDATE grants SET expires_at = $2
WHERE id = $1 AND revoked_at IS NULL AND $2 > expires_at
RETURNING *;

-- name: ListGrantsInCell :many
-- Grants that are active now or start within the horizon; expired and revoked grants are excluded.
SELECT g.* FROM grants g JOIN orgs o ON o.id = g.grantor_org_id
WHERE o.cell_id = $1 AND g.revoked_at IS NULL AND g.expires_at > $2 AND g.starts_at <= $3
ORDER BY g.id;

-- name: ListOverdueGrants :many
-- Grants whose expiry passed while nothing processed them (for example during a control-plane outage).
SELECT * FROM grants
WHERE revoked_at IS NULL AND expires_at <= $1 AND expires_at > $2
ORDER BY expires_at;

-- name: ListGrantsForOrg :many
SELECT * FROM grants WHERE (grantor_org_id = $1 OR grantee_org_id = $1) AND revoked_at IS NULL AND expires_at > now()
ORDER BY expires_at;
