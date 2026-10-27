-- name: CreateCell :one
INSERT INTO cells (name, server_url, base_domain, mode)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetCellByName :one
SELECT * FROM cells WHERE name = $1;

-- name: GetCell :one
SELECT * FROM cells WHERE id = $1;

-- name: TouchCell :exec
UPDATE cells SET last_seen_at = now(), headscale_version = $2 WHERE id = $1;
