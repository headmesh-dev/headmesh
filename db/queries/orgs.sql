-- name: CreateOrg :one
INSERT INTO orgs (short_id, slug, name, cell_id, parent_org_id, is_platform, template, headscale_users)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetOrg :one
SELECT * FROM orgs WHERE id = $1;

-- name: GetOrgBySlug :one
SELECT * FROM orgs WHERE slug = $1;

-- name: ListOrgsInCell :many
SELECT * FROM orgs WHERE cell_id = $1 AND status <> 'deleted' ORDER BY short_id;

-- name: ListChildOrgs :many
SELECT * FROM orgs WHERE parent_org_id = $1 AND status <> 'deleted' ORDER BY short_id;

-- name: GetOrgByShortID :one
SELECT * FROM orgs WHERE short_id = $1;
