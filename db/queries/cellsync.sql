-- name: NextPolicyRevision :one
SELECT (coalesce(max(revision), 0) + 1)::bigint AS next FROM policy_revisions WHERE cell_id = $1;

-- name: InsertPolicyRevision :exec
INSERT INTO policy_revisions (cell_id, revision, sha256, body, compiler_version, effective_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (cell_id, revision) DO NOTHING;

-- name: MarkPolicyRevision :exec
UPDATE policy_revisions SET status = $3, applied_at = $4, error = $5
WHERE cell_id = $1 AND revision = $2;

-- name: SyncDeviceFromNode :exec
-- Keeps device rows in step with what Headscale reports for nodes Headmesh already knows.
UPDATE devices
SET ipv4 = $3, ipv6 = $4, online = $5, last_seen_at = coalesce($6, last_seen_at), updated_at = now()
WHERE cell_id = $1 AND headscale_node_id = $2;
