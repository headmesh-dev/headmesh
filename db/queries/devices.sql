-- name: CreateDevice :one
INSERT INTO devices (org_id, cell_id, short_id, name, given_name, kind, roles, headscale_user, enrolled_via, external_ref)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetDevice :one
SELECT * FROM devices WHERE id = $1;

-- name: GetDeviceByNode :one
SELECT * FROM devices WHERE cell_id = $1 AND headscale_node_id = $2;

-- name: ListDevicesByOrg :many
SELECT * FROM devices WHERE org_id = $1 AND state <> 'removed' ORDER BY name;

-- name: ListDevicesInCell :many
SELECT * FROM devices WHERE cell_id = $1 AND state <> 'removed' ORDER BY short_id;

-- name: AttachDeviceNode :one
UPDATE devices
SET headscale_node_id = $2, ipv4 = $3, ipv6 = $4, online = true, last_seen_at = now(), updated_at = now()
WHERE id = $1
RETURNING *;

-- name: SetDeviceState :one
UPDATE devices SET state = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: GetDeviceByShortID :one
SELECT * FROM devices WHERE short_id = $1;

-- name: RenameDeviceGivenName :exec
UPDATE devices SET given_name = $2, updated_at = now() WHERE id = $1;
