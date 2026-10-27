-- name: LockChainHead :one
SELECT head_hash, last_id FROM audit_chain_heads WHERE chain_key = $1 FOR UPDATE;

-- name: InitChainHead :exec
INSERT INTO audit_chain_heads (chain_key, head_hash, last_id) VALUES ($1, $2, 0) ON CONFLICT (chain_key) DO NOTHING;

-- name: AdvanceChainHead :exec
UPDATE audit_chain_heads SET head_hash = $2, last_id = $3 WHERE chain_key = $1;

-- name: InsertAuditEvent :one
INSERT INTO audit_events (org_id, occurred_at, actor_type, actor_id, action, target_type, target_id, request_id, ip, user_agent, data, prev_hash, hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING id;

-- name: ListAuditEvents :many
SELECT * FROM audit_events WHERE org_id = $1 AND id > $2 ORDER BY id LIMIT $3;

-- name: ListAuditEventsForChain :many
SELECT * FROM audit_events WHERE org_id IS NOT DISTINCT FROM $1 AND id > $2 ORDER BY id LIMIT $3;
