#!/usr/bin/env bash
# The cell check invokes this while Headscale is running in the drill namespace.
set -euo pipefail
umask 077

runtime="$1"
database_url="$2"
api_url=http://127.0.0.1:8080
cell_url=https://127.0.0.1:7443
server_pid=""
agent_pid=""
cleanup() {
  if [[ -n "${agent_pid}" ]]; then kill "${agent_pid}" 2>/dev/null || true; wait "${agent_pid}" 2>/dev/null || true; fi
  if [[ -n "${server_pid}" ]]; then kill "${server_pid}" 2>/dev/null || true; wait "${server_pid}" 2>/dev/null || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

db() { env -i PATH="${PATH}" psql -XqAt -v ON_ERROR_STOP=1 --dbname="${database_url}" "$@"; }
probe() { curl -q --silent --show-error --noproxy '*' --max-time 2 "$@"; }

if [[ "$(db -c 'SHOW data_directory')" != "$3" ]]; then
  echo 'The control-plane check must use the PostgreSQL copy created by this drill.' >&2
  exit 1
fi
enforce_count="$(db -c "SELECT count(*) FROM cells WHERE mode = 'enforce'")"
if (( enforce_count > 1 )); then
  echo 'The drill restores one Headscale cell. Recovery requires all enforce-mode cells to be present.' >&2
  exit 1
fi
cell_name="$(db -c "SELECT name FROM cells WHERE mode = 'enforce'")"
sweep_cutoff="$(db -c 'SELECT clock_timestamp()')"
db -v cutoff="${sweep_cutoff}" >"${runtime}/overdue.ids" <<'SQL'
SELECT id FROM grants WHERE revoked_at IS NULL AND expiry_audited_at IS NULL
  AND expires_at <= :'cutoff'::timestamptz ORDER BY id;
SQL

# Use a non-superuser application role so the read-only grant probe exercises RLS.
app_role="hm_drill_${runtime##*.}"
app_role="${app_role,,}"
db -v app_role="${app_role}" <<'SQL'
CREATE ROLE :"app_role" NOLOGIN NOBYPASSRLS;
GRANT USAGE ON SCHEMA public TO :"app_role";
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO :"app_role";
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO :"app_role";
SQL

public_key="$(headmesh-server keygen -out "${runtime}/signing.key" 2>"${runtime}/keygen.log")"
env -i PATH="${PATH}" HEADMESH_DATABASE_URL="${database_url}&options=-c%20role%3D${app_role}" \
  HEADMESH_LISTEN=127.0.0.1:8080 HEADMESH_CELL_LISTEN=127.0.0.1:7443 HEADMESH_RECOVERY_TIMEOUT=15s \
  HEADMESH_PUBLIC_URL=https://restore.example.test HEADMESH_CELL_CA="${runtime}/ca.crt" \
  HEADMESH_TLS_CERT="${runtime}/server.crt" HEADMESH_TLS_KEY="${runtime}/server.key" \
  HEADMESH_OP_SIGNING_KEY="${runtime}/signing.key" \
  headmesh-server serve -recovery >"${runtime}/server.log" 2>&1 &
server_pid=$!
for _ in {1..20}; do
  if probe --fail "${api_url}/healthz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "${server_pid}" 2>/dev/null; then break; fi
  sleep 1
done
if ! probe --fail "${api_url}/healthz" >/dev/null; then
  echo 'The restored Headmesh API did not start.' >&2
  tail -n 12 "${runtime}/server.log" >&2
  exit 1
fi

if (( enforce_count )); then
  if [[ "$(probe --output /dev/null --write-out '%{http_code}' "${api_url}/readyz")" != 503 ]] ||
    [[ "$(probe --request POST --dump-header "${runtime}/gated.headers" --output /dev/null --write-out '%{http_code}' \
      "${api_url}/v1/grants")" != 503 ]] || ! grep -iq '^retry-after: [1-9][0-9]*' "${runtime}/gated.headers"; then
    echo 'The API recovery gate did not refuse writes while the cell was offline.' >&2
    exit 1
  fi

  headmesh-server cell-cert -dir "${runtime}" -cell "${cell_name}" -out "${runtime}" -days 1 \
    >"${runtime}/cell-cert.log" 2>&1
  jq -n --arg run "${runtime}" --arg cell "${cell_name}" --arg url "${cell_url}" --arg key "${public_key}" '{
    cell_name: $cell, mode: "enforce", state_dir: ($run + "/agent"),
    headscale_socket: ($run + "/headscale.sock"), headscale_http: "https://127.0.0.1:8443", server_url: $url,
    client_cert: ($run + "/" + $cell + ".crt"), client_key: ($run + "/" + $cell + ".key"),
    server_ca: ($run + "/ca.crt"), server_keys: [$key]
  }' >"${runtime}/agent.json"
  env -i PATH="${PATH}" SSL_CERT_FILE="${runtime}/ca.crt" headmesh-cellagent run -config "${runtime}/agent.json" \
    >"${runtime}/agent.log" 2>&1 &
  agent_pid=$!
fi

for _ in {1..45}; do
  if probe --fail "${api_url}/readyz" >/dev/null 2>&1; then break; fi
  if ! kill -0 "${server_pid}" 2>/dev/null; then break; fi
  sleep 1
done
if ! probe --fail "${api_url}/readyz" >/dev/null; then
  echo 'The restored Headmesh API did not finish recovery.' >&2
  tail -n 12 "${runtime}/server.log" >&2
  if [[ -f "${runtime}/agent.log" ]]; then tail -n 12 "${runtime}/agent.log" >&2; fi
  exit 1
fi
if (( enforce_count )); then
  applied="$(db -v cell="${cell_name}" -v cutoff="${sweep_cutoff}" <<'SQL'
SELECT EXISTS (SELECT 1 FROM policy_revisions r JOIN cells c ON c.id = r.cell_id
  WHERE c.name = :'cell' AND r.status = 'applied' AND r.applied_at >= :'cutoff'::timestamptz);
SQL
)"
  if [[ "${applied}" != t ]]; then
    echo 'Headmesh readiness passed without a recorded cell policy acknowledgement.' >&2
    exit 1
  fi
fi
remaining="$(db -v cutoff="${sweep_cutoff}" <<'SQL'
SELECT count(*) FROM grants WHERE revoked_at IS NULL AND expiry_audited_at IS NULL
  AND expires_at <= :'cutoff'::timestamptz;
SQL
)"
if (( remaining )); then
  echo 'The recovery sweep left overdue grants without an expiry audit.' >&2
  exit 1
fi
db -v cutoff="${sweep_cutoff}" >"${runtime}/audited.ids" <<'SQL'
SELECT DISTINCT target_id FROM audit_events WHERE action = 'grant.expired_late' AND target_type = 'grant'
  AND occurred_at >= :'cutoff'::timestamptz AND (data->>'noticed_late_seconds')::bigint >= 0
ORDER BY target_id;
SQL
if [[ -n "$(comm -23 "${runtime}/overdue.ids" "${runtime}/audited.ids")" ]]; then
  echo 'The recovery sweep omitted one or more late-expiry events.' >&2
  exit 1
fi
HEADMESH_DATABASE_URL="${database_url}" headmesh-server backup-manifest audit

org="$(db <<'SQL'
SELECT id FROM orgs o WHERE status = 'active' ORDER BY EXISTS (
  SELECT 1 FROM grants g WHERE (g.grantor_org_id = o.id OR g.grantee_org_id = o.id)
    AND g.revoked_at IS NULL AND g.expires_at > now()
) DESC, id LIMIT 1;
SQL
)"
if [[ -z "${org}" ]]; then
  echo 'The restored database has no active organization to sample through the grant API.' >&2
  exit 1
fi
token="hmk_$(openssl rand -hex 32)"
printf 'Authorization: Bearer %s\n' "${token}" >"${runtime}/grant.headers"
digest="$(printf '%s' "${token}" | sha256sum | cut -d ' ' -f 1)"
db -v org="${org}" -v digest="${digest}" <<'SQL'
INSERT INTO api_keys (org_id, name, prefix, key_hash, scopes, expires_at)
VALUES (:'org'::uuid, 'restore drill', 'hmk_', decode(:'digest', 'hex'), ARRAY['grants:read'], now() + interval '5 min');
SQL
expected="$(db -v org="${org}" <<'SQL'
SELECT id FROM grants WHERE (grantor_org_id = :'org'::uuid OR grantee_org_id = :'org'::uuid)
  AND revoked_at IS NULL AND expires_at > now() ORDER BY id;
SQL
)"
result="$(probe --fail --header "@${runtime}/grant.headers" "${api_url}/v1/grants")"
actual="$(jq -er '.grants | map(.id) | sort | join("\n")' <<<"${result}")"
if [[ "${expected}" != "${actual}" ]]; then
  echo 'The restored grant API returned a different set of grants than the database.' >&2
  exit 1
fi
printf 'Headmesh recovery completed with %d enforce-mode cell acknowledgements; the grant API matched the restored data (%d grants).\n' \
  "${enforce_count}" "$(jq '.grants | length' <<<"${result}")"
