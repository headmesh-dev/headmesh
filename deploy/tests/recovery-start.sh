#!/usr/bin/env bash
# Called inside the disposable deploy e2e container after its ordinary deploys.
set -euo pipefail
umask 077
[[ "$(systemd-detect-virt --container)" == docker ]]
ctl=/usr/local/lib/headmesh/deployctl.sh
app=/etc/headmesh-cellagent/cellagent.json
record=/var/lib/headmesh/recovery
tmp="$(mktemp -d)"
stage="${tmp}/stage"
release_path="$(readlink -f /srv/headmesh/slots/backend-prod-green)"
release="$(basename "${release_path}")"
sql() { runuser -u postgres -- psql -XAq -v ON_ERROR_STOP=1 -d headmesh_prod "$@"; }
probe() { curl -qsS --noproxy '*' --max-time 2 -o /dev/null -w '%{http_code}' "$@"; }

systemctl stop headmesh-cellagent.service headmesh-cellcert@prod.timer headmesh-backend@prod-green.service \
  headmesh-backend@test-blue.service headmesh-waitlist@test-blue.service certbot-renew.timer nginx.service headscale.service
systemctl disable --quiet headmesh-cellagent.service headmesh-backend@prod-green.service headmesh-backend@test-blue.service
systemctl mask --quiet nginx.service headscale.service

# Record one node and a grant that expired during the interruption.
sqlite3 -batch -bail /var/lib/headscale/db.sqlite <<'SQL'
INSERT INTO nodes (machine_key, node_key, disco_key, hostname, given_name, ipv4, ipv6,
    endpoints, tags, approved_routes, user_id, created_at, updated_at)
SELECT 'mkey:' || lower(hex(randomblob(32))), 'nodekey:' || lower(hex(randomblob(32))),
    'discokey:' || lower(hex(randomblob(32))), 'standby-fixture', 'standby-fixture',
    '100.64.0.44', 'fd7a:115c:a1e0::44', '[]', '[]', '[]', id, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
FROM users WHERE name = 'e2e-upgrade';
INSERT INTO policies (data, created_at, updated_at)
SELECT '{"grants":[]}', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP WHERE NOT EXISTS (SELECT 1 FROM policies);
SQL
sql <<'SQL'
INSERT INTO orgs (short_id, slug, name, cell_id)
SELECT 'standbyabc', 'standby-consumer', 'Standby consumer', id FROM cells WHERE name = 'hs1';
WITH target AS (
    INSERT INTO devices (org_id, cell_id, short_id, name, given_name, kind)
    SELECT id, cell_id, 'standbydev', 'target', 'target', 'server' FROM orgs WHERE slug = 'headmesh-ops'
    RETURNING id, org_id
)
INSERT INTO grants (grantor_org_id, grantee_org_id, src, dst_device_id, ports, starts_at, expires_at, external_ref)
SELECT target.org_id, consumer.id, '{"kind":"all"}', target.id, ARRAY['tcp:22'], now() - interval '2 hours',
       now() - interval '1 hour', 'standby-overdue'
FROM target CROSS JOIN orgs consumer WHERE consumer.slug = 'standby-consumer';
SQL
[[ "$(sql -t -c "SELECT count(*) FROM grants WHERE external_ref = 'standby-overdue'")" == 1 ]]

# Reuse the fixture's original credentials and publishable state on an empty
# standby layout. The separate object-storage drill exercises actual WAL replay.
chmod 0711 "${tmp}"
install -d -o postgres -g postgres -m 0700 "${stage}" "${stage}/keys" "${stage}/tablespaces"
sqlite3 /var/lib/headscale/db.sqlite ".backup '${stage}/headscale.sqlite'"
chown postgres:postgres "${stage}/headscale.sqlite"
install -o postgres -g postgres -m 0600 /etc/headscale/config.yaml "${stage}/keys/config.yaml"
install -o postgres -g postgres -m 0600 /var/lib/headscale/{noise_private.key,derp_server_private.key} "${stage}/keys/"
runuser -u postgres -- env HEADMESH_DATABASE_URL='host=/var/run/postgresql dbname=headmesh_prod user=postgres' \
  "${release_path}/headmesh-server" backup-manifest capture -sqlite "${stage}/headscale.sqlite" \
    -noise-key "${stage}/keys/noise_private.key" >"${stage}/manifest.json"
chown postgres:postgres "${stage}/manifest.json"
systemctl stop postgresql@18-main.service
cp -a /var/lib/postgresql/18/main "${stage}/postgres"
mv /var/lib/postgresql/18/main "${tmp}/old-postgres"
mv /etc/postgresql/18/main "${tmp}/old-postgres-config"
mv /var/lib/headscale "${tmp}/old-headscale"
mv /var/lib/headmesh/prod/state "${tmp}/old-deploy-state"
mv /var/lib/headmesh-cellagent "${tmp}/old-agent-state"
/usr/local/lib/headmesh/bootstrap.sh restore "${stage}"

# A missing agent key keeps readiness and writes closed and rolls back the
# candidate services. Retry with the real fixture credentials.
cp -a "${app}" "${tmp}/agent.json"
jq '.client_key = "/run/recovery-fixture-missing.key"' "${tmp}/agent.json" >"${app}"
HM_HEALTH_TIMEOUT=12 "${ctl}" start-recovery "${release}" >"${tmp}/blocked.log" 2>&1 &
candidate=$!
pending=0
for _ in {1..100}; do
  if [[ "$(probe http://127.0.0.1:8101/readyz 2>/dev/null || true)" == 503 ]]; then
    pending=1
    break
  fi
  if ! kill -0 "${candidate}" 2>/dev/null; then break; fi
  sleep 0.1
done
if (( pending )); then
  [[ "$(probe -X POST http://127.0.0.1:8101/v1/grants)" == 503 ]]
  if systemctl is-active --quiet nginx.service; then exit 1; fi
fi
if wait "${candidate}" || (( ! pending )); then
  cat "${tmp}/blocked.log" >&2
  exit 1
fi
grep -q 'did not finish the recovery policy checks' "${tmp}/blocked.log"
[[ ! -e "${record}/backend-ready" && "$(</etc/postgresql/18/main/start.conf)" == disabled ]]
for unit in postgresql@18-main headscale headmesh-cellagent headmesh-backend@prod-blue nginx; do
  [[ "$(systemctl show -p ActiveState --value "${unit}.service")" == inactive ]]
done
echo 'Standby withheld readiness and writes and stopped the candidate after the missing cell acknowledgement.'

cp -a "${tmp}/agent.json" "${app}"
cutoff="$(date -u '+%Y-%m-%dT%H:%M:%S.%NZ')"
"${ctl}" start-recovery "${release}"
[[ "$(<"${record}/backend-ready")" == "${release}" && "$(probe http://127.0.0.1:8101/readyz)" == 204 ]]
[[ "$(readlink /etc/systemd/system/nginx.service)" == /dev/null ]]
[[ -z "$(ss -Hltn 'sport = :443')" ]]
[[ "$(ss -Hlun 'sport = :3478' | awk '{print $4}')" == 127.0.0.1:3478 ]]
if ss -Hltn | awk '{print $4}' | grep -E ':(8080|8101|8201|9090|50443)$' | grep -qv '^127\.0\.0\.1:'; then
  echo 'A recovery listener is reachable beyond loopback.' >&2
  exit 1
fi
cmp "${tmp}/agent.json" "${app}"
jq -e '.server_url == "https://127.0.0.1:8201"' /etc/headmesh-cellagent/cellagent-recovery.json >/dev/null
[[ "$(sql -t -c "SELECT count(*) FROM grants g JOIN audit_events e ON e.target_id = g.id::text
   WHERE g.external_ref = 'standby-overdue' AND e.action = 'grant.expired_late'")" == 1 ]]
[[ "$(sql -t -v cutoff="${cutoff}" <<'SQL'
SELECT count(*) > 0 FROM policy_revisions WHERE status = 'applied' AND applied_at >= :'cutoff'::timestamptz;
SQL
)" == t ]]
echo 'Standby kept nginx offline and became ready after a fresh cell acknowledgement and one late-expiry audit.'
