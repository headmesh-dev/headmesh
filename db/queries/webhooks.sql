-- name: CreateWebhookEndpoint :one
INSERT INTO webhook_endpoints (org_id, url, secret_ciphertext, events, description)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListWebhookEndpoints :many
SELECT * FROM webhook_endpoints WHERE org_id = $1 ORDER BY created_at;

-- name: GetWebhookEndpoint :one
SELECT * FROM webhook_endpoints WHERE id = $1;

-- name: DeleteWebhookEndpoint :exec
DELETE FROM webhook_endpoints WHERE id = $1 AND org_id = $2;

-- name: EnqueueWebhookDeliveries :exec
-- The outbox: one pending delivery per active endpoint of the event's org whose filter matches the action exactly,
-- by its first segment ("grant.*"), or with "*".
INSERT INTO webhook_deliveries (org_id, endpoint_id, audit_event_id, status, next_attempt_at)
SELECT e.org_id, e.id, sqlc.arg(audit_event_id)::bigint, 'pending', now()
FROM webhook_endpoints e
WHERE e.org_id = sqlc.arg(org_id) AND e.status = 'active'
  AND (sqlc.arg(action)::text = ANY (e.events) OR '*' = ANY (e.events)
       OR split_part(sqlc.arg(action)::text, '.', 1) || '.*' = ANY (e.events));

-- name: ClaimDueWebhookDeliveries :many
-- Leases due deliveries: pushing next_attempt_at forward hides them from other workers until the lease ends, so a
-- worker that dies mid-delivery only delays a retry.
UPDATE webhook_deliveries d
SET next_attempt_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::int), attempt = d.attempt + 1,
    last_attempt_at = now()
WHERE d.id IN (
    SELECT w.id FROM webhook_deliveries w
    WHERE w.status = 'pending' AND w.next_attempt_at <= now()
    ORDER BY w.next_attempt_at
    LIMIT sqlc.arg(max_deliveries)::int
    FOR UPDATE SKIP LOCKED
)
RETURNING d.*;

-- name: FinishWebhookDelivery :exec
UPDATE webhook_deliveries
SET status = sqlc.arg(status), response_code = sqlc.narg(response_code), error = sqlc.narg(error),
    next_attempt_at = sqlc.narg(next_attempt_at), delivered_at = sqlc.narg(delivered_at)
WHERE id = sqlc.arg(id);

-- name: MarkWebhookEndpointHealthy :exec
UPDATE webhook_endpoints SET failing_since = NULL WHERE id = $1 AND failing_since IS NOT NULL;

-- name: MarkWebhookEndpointFailing :one
-- Records the first failure and disables an endpoint that has failed for 24 hours. Returns the new status.
UPDATE webhook_endpoints
SET failing_since = coalesce(failing_since, now()),
    status = CASE WHEN failing_since < now() - interval '24 hours' THEN 'disabled' ELSE status END
WHERE id = $1
RETURNING status;

-- name: ListWebhookDeliveries :many
SELECT d.*, a.action FROM webhook_deliveries d JOIN audit_events a ON a.id = d.audit_event_id
WHERE d.endpoint_id = $1
ORDER BY d.created_at DESC
LIMIT $2;
