-- name: CreateService :one
INSERT INTO services (org_id, device_id, name, ports, description)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListServicesByOrg :many
SELECT * FROM services WHERE org_id = $1 ORDER BY name;

-- name: GetService :one
SELECT * FROM services WHERE id = $1;

-- name: DeleteService :exec
DELETE FROM services WHERE id = $1 AND org_id = $2;
