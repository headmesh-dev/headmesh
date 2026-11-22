\set ON_ERROR_STOP on

BEGIN;
CREATE ROLE headmesh_backup_manifest LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT CONNECT ON DATABASE :"DBNAME" TO headmesh_backup_manifest;
GRANT USAGE ON SCHEMA public TO headmesh_backup_manifest;
GRANT SELECT (id, action, hash) ON TABLE public.audit_events TO headmesh_backup_manifest;
GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system(), pg_catalog.pg_create_restore_point(text),
  pg_catalog.pg_switch_wal() TO headmesh_backup_manifest;
COMMIT;
