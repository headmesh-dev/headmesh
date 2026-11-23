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
if ! command -v age-keygen >/dev/null; then apt-get install -y --no-install-recommends age >/dev/null; fi

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

# Preserve a referenced provider credential together with the ordinary runtime files.
sed 's@https://test.headmesh.dev@https://headmesh.dev@g' /etc/headmesh/test/waitlist.env >"${tmp}/waitlist.env"
install -m 0640 -o root -g headmesh "${tmp}/waitlist.env" /etc/headmesh/prod/waitlist.env
install -d -m 0750 -o root -g headmesh /etc/headmesh/oauth
printf '%s\n' fixture-client-credential >/etc/headmesh/oauth/fixture-client-secret
printf '%s\n' 'HEADMESH_GITHUB_CLIENT_ID=fixture-runtime-client' \
  "HEADMESH_GITHUB_CLIENT_SECRET_FILE='/etc/headmesh/oauth/fixture-client-secret'" >>/etc/headmesh/prod/backend.env
age-keygen -o "${tmp}/owner.agekey" 2>/dev/null
age-keygen -o "${tmp}/runtime.agekey" 2>/dev/null
"${release_path}/headmesh-server" backup-runtime pack -recipient "$(age-keygen -y "${tmp}/owner.agekey")" \
  -recovery-recipient "$(age-keygen -y "${tmp}/runtime.agekey")" >"${tmp}/runtime.age"
tls_key_hash="$(sha256sum /etc/headmesh/tls/privkey.pem | cut -d ' ' -f 1)"
mv /etc/headmesh/prod "${tmp}/old-prod-config"
mv /etc/headmesh/tls "${tmp}/old-tls"
mv /etc/headmesh/oauth "${tmp}/old-oauth"
mv /etc/headmesh-cellagent "${tmp}/old-agent-config"
systemctl stop postgresql@18-main.service
cp -a /var/lib/postgresql/18/main "${stage}/postgres"
mv /var/lib/postgresql/18/main "${tmp}/old-postgres"
mv /etc/postgresql/18/main "${tmp}/old-postgres-config"
mv /var/lib/headscale "${tmp}/old-headscale"
mv /var/lib/headmesh/prod/state "${tmp}/old-deploy-state"
mv /var/lib/headmesh-cellagent "${tmp}/old-agent-state"
/usr/local/lib/headmesh/bootstrap.sh restore "${stage}"

install_runtime() { "${ctl}" install-recovery-runtime "${release}" "${tmp}/runtime.agekey" -bundle "${tmp}/runtime.age"; }
install -d -m 0750 -o root -g headmesh /etc/headmesh/prod
printf '%s\n' fixture-conflict >/etc/headmesh/prod/backend.env
if install_runtime >"${tmp}/runtime-conflict.log" 2>&1; then exit 1; fi
grep -q 'runtime destination differs' "${tmp}/runtime-conflict.log"
[[ "$(</etc/headmesh/prod/backend.env)" == fixture-conflict && ! -e "${app}" && ! -e /etc/headmesh/tls/privkey.pem ]]
rm /etc/headmesh/prod/backend.env
install_runtime
install_runtime
for name in backend.env db.env db-owner.env waitlist.env secrets.env backend/op-signing.key; do
  cmp "${tmp}/old-prod-config/${name}" "/etc/headmesh/prod/${name}"
done
[[ "$(sha256sum /etc/headmesh/tls/privkey.pem | cut -d ' ' -f 1)" == "${tls_key_hash}" ]]
cmp "${tmp}/old-agent-config/client.key" /etc/headmesh-cellagent/client.key
cmp "${tmp}/old-oauth/fixture-client-secret" /etc/headmesh/oauth/fixture-client-secret
[[ "$(stat -c '%a:%U:%G' /etc/headmesh/prod/db-owner.env)" == 600:root:root &&
  "$(stat -c '%a:%U:%G' /etc/headmesh/oauth/fixture-client-secret)" == 600:headmesh:headmesh &&
  "$(stat -c '%a:%U:%G' /etc/headmesh-cellagent/client.key)" == 640:root:headmesh-cellagent ]]
echo 'Installed the encrypted runtime on an empty standby, preserved a conflicting file, and verified retry and file permissions.'

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

# Recreate the render directory and install a minimal site release for nginx.
rendered="${tmp}/nginx"
mkdir -p "${rendered}"/{snippets,sites,stream}
cp /etc/nginx/headmesh/cloudflare-realip.conf "${rendered}/"
cp /etc/nginx/conf.d/headmesh.conf "${rendered}/"
cp /etc/nginx/modules-enabled/99-headmesh-stream.conf "${rendered}/stream-include.conf"
cp /etc/nginx/snippets/headmesh-{tls,proxy}.conf "${rendered}/snippets/"
cp /etc/nginx/sites-available/headmesh-prod.conf "${rendered}/sites/"
cp /etc/nginx/headmesh/prod/stream.conf "${rendered}/stream/prod.conf"
site_root=/srv/headmesh/prod/marketing
site="${site_root}/releases/${release}"
standby_ipv4=192.0.2.77
standby_ipv6=2001:db8:77::1
ip -4 address add "${standby_ipv4}/32" dev lo
ip -6 address add "${standby_ipv6}/128" dev lo nodad
prepare_ingress() { "${ctl}" prepare-recovery-ingress "$1" "${release}" "${standby_ipv4}" "${standby_ipv6}"; }
install -d -m 0755 "${site_root}" "${site_root}/releases" "${site}" "${site}/site" "${site}/nginx"
echo '<title>Headmesh</title>' >"${site}/site/index.html"
echo '# Fixture security headers' >"${site}/nginx/security-headers.conf"
chmod 0644 "${site}/site/index.html" "${site}/nginx/security-headers.conf"
ln -s /etc/nginx/sites-available/default /etc/nginx/sites-enabled/default
before_upstream="$(sha256sum /etc/nginx/headmesh/prod/backend-upstream.conf)"
before_vhost="$(sha256sum /etc/nginx/sites-available/headmesh-prod.conf)"
ingress_closed() {
  [[ "$(systemctl show -p ActiveState --value nginx.service)" == inactive &&
    "$(readlink /etc/systemd/system/nginx.service)" == /dev/null &&
    -L /etc/nginx/sites-enabled/default && ! -L "${site_root}/current" && ! -e "${record}/ingress-ready" &&
    ! -e /etc/systemd/system/headscale.service.d/standby-relay.conf &&
    "$(ss -Hlun 'sport = :3478' | awk '{print $4}')" == 127.0.0.1:3478 &&
    "$(sha256sum /etc/nginx/headmesh/prod/backend-upstream.conf)" == "${before_upstream}" &&
    "$(sha256sum /etc/nginx/sites-available/headmesh-prod.conf)" == "${before_vhost}" ]]
}

if prepare_ingress "${rendered}" >"${tmp}/no-waitlist.log" 2>&1; then exit 1; fi
grep -q 'start the restored waitlist' "${tmp}/no-waitlist.log"
ingress_closed

# Recover the signup confirmed on the ordinary fixture earlier in the suite.
waitlist_target="/srv/headmesh/prod/waitlist/releases/${release}"
waitlist_snapshot="${tmp}/waitlist.db"
waitlist_db=/var/lib/headmesh/prod/waitlist/waitlist.db
install -d -m 0755 /srv/headmesh/prod/waitlist "$(dirname "${waitlist_target}")"
cp -a "$(readlink -f /srv/headmesh/slots/waitlist-test-blue)" "${waitlist_target}"
sqlite3 -readonly /var/lib/headmesh/test/waitlist/waitlist.db ".backup '${waitlist_snapshot}'"
waitlist_source_hash="$(sha256sum "${waitlist_snapshot}")"
echo 'invalid SQLite fixture' >"${tmp}/bad-waitlist.db"
if "${ctl}" start-recovery-waitlist "${release}" "${tmp}/bad-waitlist.db" >"${tmp}/bad-waitlist.log" 2>&1; then exit 1; fi
grep -q 'failed SQLite integrity or schema validation' "${tmp}/bad-waitlist.log"
[[ ! -e "${waitlist_db}" && ! -e "${record}/waitlist-ready" ]]

sed -i '/^WAITLIST_SECRET=/d' /etc/headmesh/prod/waitlist.env
if HM_HEALTH_TIMEOUT=4 "${ctl}" start-recovery-waitlist "${release}" "${waitlist_snapshot}" >"${tmp}/bad-env.log" 2>&1; then exit 1; fi
grep -q 'restored waitlist did not become ready' "${tmp}/bad-env.log"
[[ ! -e "${waitlist_db}" && ! -e "${record}/waitlist-ready" ]]
[[ "$(systemctl show -p ActiveState --value headmesh-waitlist@prod-blue.service)" == inactive ]]
install -m 0640 -o root -g headmesh "${tmp}/waitlist.env" /etc/headmesh/prod/waitlist.env
"${ctl}" start-recovery-waitlist "${release}" "${waitlist_snapshot}"
[[ "$(sha256sum "${waitlist_snapshot}")" == "${waitlist_source_hash}" ]]
[[ "$(probe http://127.0.0.1:8301/readyz)" == 204 && "$(<"${record}/waitlist-ready")" == "${release}" ]]
[[ "$(ss -Hltn 'sport = :8301' | awk '{print $4}')" == 127.0.0.1:8301 ]]
[[ "$(sqlite3 -readonly "${waitlist_db}" "SELECT count(*) FROM waitlist WHERE email = 'e2e@example.com' AND confirmed_at IS NOT NULL;")" == 1 ]]
if "${ctl}" start-recovery-waitlist "${release}" "${waitlist_snapshot}" >"${tmp}/repeat-waitlist.log" 2>&1; then exit 1; fi
grep -q 'waitlist is already recorded' "${tmp}/repeat-waitlist.log"

if "${ctl}" prepare-recovery-ingress "${rendered}" "${release}" 192.0.2.177 "${standby_ipv6}" >"${tmp}/wrong-address.log" 2>&1; then
  exit 1
fi
grep -q 'IPv4 must be assigned to this server' "${tmp}/wrong-address.log"
ingress_closed
if prepare_ingress "${rendered}" >"${tmp}/untrusted.log" 2>&1; then exit 1; fi
grep -q 'certificate is untrusted or invalid' "${tmp}/untrusted.log"
ingress_closed
install -m 0644 /etc/headmesh/tls/fullchain.pem /usr/local/share/ca-certificates/recovery-fixture.crt
update-ca-certificates >/dev/null

# A syntactically valid misroute fails after nginx starts. The previous config
# and mask must be restored, and the staged backend must remain available.
cp -a "${rendered}" "${tmp}/misrouted"
sed -i 's#proxy_pass http://headmesh_prod_backend;#return 418;#' "${tmp}/misrouted/sites/headmesh-prod.conf"
if prepare_ingress "${tmp}/misrouted" >"${tmp}/misrouted.log" 2>&1; then exit 1; fi
if ! grep -q 'API is unready through nginx' "${tmp}/misrouted.log"; then
  cat "${tmp}/misrouted.log" >&2
  exit 1
fi
ingress_closed
[[ "$(probe http://127.0.0.1:8101/readyz)" == 204 ]]

prepare_ingress "${rendered}"
systemctl is-active --quiet nginx.service
[[ "$(systemctl show -p UnitFileState --value nginx.service)" == disabled ]]
[[ "$(cat "${record}/ingress-ready")" == "$(printf '%s\n%s' "${release}" "${release}")" ]]
[[ ! -e /etc/nginx/sites-enabled/default && ! -e /var/lib/headmesh/prod/state/backend.active ]]
for address in 127.0.0.1 '[::1]'; do
  [[ "$(probe --resolve "api.headmesh.dev:443:${address}" https://api.headmesh.dev/readyz)" == 204 ]]
  [[ "$(probe --resolve "hs1.headmesh.dev:443:${address}" https://hs1.headmesh.dev/health)" == 200 ]]
  [[ "$(probe --resolve "hs1.headmesh.dev:443:${address}" https://hs1.headmesh.dev/derp/probe)" == 200 ]]
  [[ "$(probe --resolve "headmesh.dev:443:${address}" https://headmesh.dev/)" == 200 ]]
  [[ "$(probe --resolve "headmesh.dev:443:${address}" https://headmesh.dev/api/waitlist/readyz)" == 204 ]]
  [[ "$(probe --resolve "headmesh.dev:443:${address}" https://headmesh.dev/api/waitlist)" == 405 ]]
done
derp_log="$(journalctl -u headscale.service -n 80 --no-pager -o cat)"
grep -F "IPv4:${standby_ipv4} IPv6:${standby_ipv6} STUNPort:3478" <<<"${derp_log}" >/dev/null
echo 'Standby rejected untrusted TLS, rolled back a misroute, and served verified HTTPS on IPv4 and IPv6 after retry.'
echo 'Standby preserved a confirmed waitlist signup and served the recovered waitlist behind nginx.'

state=/var/lib/headmesh/prod/state
mkdir "${state}"
echo green >"${state}/backend.active"
if "${ctl}" finalize-recovery >"${tmp}/existing-state.log" 2>&1; then exit 1; fi
grep -q 'production deploy state already exists' "${tmp}/existing-state.log"
[[ "$(<"${state}/backend.active")" == green ]]
rm "${state}/backend.active"
rmdir "${state}"

stream=/etc/nginx/headmesh/prod/cell-upstream.conf
direct=/etc/systemd/system/headmesh-cellagent.service.d/recovery.conf
cp -a "${stream}" "${tmp}/cell-upstream.conf"
cp -a "${direct}" "${tmp}/direct.conf"

# If a check fails after a proxied Hello, rollback must confirm a newer direct Hello.
waitlist_upstream=/etc/nginx/headmesh/prod/waitlist-upstream.conf
cp -a "${waitlist_upstream}" "${tmp}/waitlist-upstream.conf"
sed -i 's/127.0.0.1:8301/127.0.0.1:1/' "${waitlist_upstream}"
systemctl reload nginx.service
hello_count() { sql -t -c "SELECT count(*) FROM audit_events WHERE action = 'cell.connected' AND actor_id = 'hs1'"; }
before="$(hello_count)"
if HM_HEALTH_TIMEOUT=8 "${ctl}" finalize-recovery >"${tmp}/bad-waitlist.log" 2>&1; then exit 1; fi
grep -q 'standby waitlist is unready through nginx (HTTP 502)' "${tmp}/bad-waitlist.log"
if grep -q 'check the restored direct recovery agent connection' "${tmp}/bad-waitlist.log"; then exit 1; fi
[[ "$(hello_count)" -ge $((before + 2)) && ! -e "${state}" ]]
cp -a "${tmp}/waitlist-upstream.conf" "${waitlist_upstream}"
systemctl reload nginx.service

sed -i 's/127.0.0.1:8201/127.0.0.1:1/' "${stream}"
systemctl reload nginx.service
if HM_HEALTH_TIMEOUT=6 "${ctl}" finalize-recovery >"${tmp}/bad-proxy.log" 2>&1; then exit 1; fi
if ! grep -q 'did not connect through the nginx mTLS proxy' "${tmp}/bad-proxy.log"; then
  cat "${tmp}/bad-proxy.log" >&2
  exit 1
fi
[[ ! -e "${state}" && "$(</etc/postgresql/18/main/start.conf)" == manual ]]
[[ "$(systemctl show -p UnitFileState --value nginx.service)" == disabled ]]
cmp "${tmp}/direct.conf" "${direct}"
systemctl is-active --quiet headmesh-cellagent.service
grep -q 'cellagent-recovery.json' < <(systemctl show -p ExecStart headmesh-cellagent.service)
[[ "$(probe http://127.0.0.1:8101/readyz)" == 204 ]]

cp -a "${tmp}/cell-upstream.conf" "${stream}"
systemctl reload nginx.service
"${ctl}" finalize-recovery
"${ctl}" finalize-recovery
[[ "$(<"${state}/backend.active")" == blue && "$(<"${state}/waitlist.active")" == blue &&
  "$(<"${state}/marketing.active")" == "${release}" && ! -e "${state}/backend.previous" && ! -e "${direct}" ]]
[[ "$(<"${state}/backend-blue.schema")" == "$(sql -t -c 'SELECT max(version_id) FROM goose_db_version WHERE is_applied')" ]]
jq -e '.server_url == "https://127.0.0.1:7443"' "${app}" >/dev/null
diff <(jq -S 'del(.server_url)' "${tmp}/agent.json") <(jq -S 'del(.server_url)' "${app}")
echo 'Standby refused existing deploy state, recovered from a broken nginx mTLS route, and adopted Blue.'
