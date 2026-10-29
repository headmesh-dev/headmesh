-- name: CreateRule :one
INSERT INTO rules (org_id, src, dst, ports, starts_at, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListRulesInCell :many
SELECT r.* FROM rules r JOIN orgs o ON o.id = r.org_id WHERE o.cell_id = $1 ORDER BY r.id;

-- name: ListRulesByOrg :many
SELECT * FROM rules WHERE org_id = $1 ORDER BY created_at;

-- name: DeleteRule :exec
DELETE FROM rules WHERE id = $1 AND org_id = $2;

-- name: CreatePlatformRule :one
INSERT INTO platform_rules (org_id, src, dst, ports) VALUES ($1, $2, $3, $4) RETURNING *;

-- name: ListPlatformRulesInCell :many
SELECT pr.* FROM platform_rules pr JOIN orgs o ON o.id = pr.org_id WHERE o.cell_id = $1 ORDER BY pr.id;

-- name: GetRule :one
SELECT * FROM rules WHERE id = $1;

-- name: UpdateRule :one
-- Optimistic concurrency: the update only applies to the version the caller read.
UPDATE rules SET src = $3, dst = $4, ports = $5, starts_at = $6, expires_at = $7, version = version + 1, updated_at = now()
WHERE id = $1 AND org_id = $2 AND version = $8
RETURNING *;
