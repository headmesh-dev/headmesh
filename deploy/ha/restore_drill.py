#!/usr/bin/env python3
"""Run the Headmesh restore drill on a disposable Hetzner VM."""

import argparse
import configparser
import datetime as dt
import ipaddress
import json
import netrc
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


ROOT = Path(__file__).resolve().parents[2]
API_URL = "https://api.hetzner.cloud/v1"
RUN_LABEL = "headmesh.dev/restore-run"
DRILL_LABELS = {"drill": "true", "ephemeral": "true"}
SELECTOR = "drill=true,ephemeral=true"
CREDENTIALS = ("litestream.aws", "manifest.netrc", "keys.netrc", "recovery.agekey", "pgbackrest.conf")
VM_USER = "headmesh-drill"


class DrillError(Exception):
    pass


class ProviderError(DrillError):
    def __init__(self, method, status, code):
        self.status = status
        self.code = code
        super().__init__(f"Hetzner {method} failed with HTTP {status} ({code})")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


class Hetzner:
    def __init__(self, token, endpoint=API_URL):
        self.token = token
        self.endpoint = endpoint
        self.opener = urllib.request.build_opener(NoRedirect())

    def request(self, method, path, payload=None):
        request = urllib.request.Request(
            self.endpoint + path,
            data=json.dumps(payload).encode() if payload is not None else None,
            headers={"Authorization": f"Bearer {self.token}", "Content-Type": "application/json"},
            method=method,
        )
        try:
            with self.opener.open(request, timeout=20) as response:
                return json.load(response)
        except urllib.error.HTTPError as exc:
            with exc:
                try:
                    code = json.load(exc).get("error", {}).get("code", "unknown")
                except (ValueError, AttributeError):
                    code = "unknown"
            if not isinstance(code, str) or not re.fullmatch(r"[a-z0-9_]+", code):
                code = "unknown"
            raise ProviderError(method, exc.code, code) from None
        except (OSError, ValueError):
            raise DrillError(f"Hetzner {method} did not provide a usable response") from None

    def server(self, server_id):
        if type(server_id) is not int or server_id < 1:
            raise DrillError("Hetzner did not provide a valid server ID")
        try:
            return self.request("GET", f"/servers/{server_id}")["server"]
        except ProviderError as exc:
            if exc.status == 404:
                return None
            raise

    def drills(self, run=None):
        selector = SELECTOR if run is None else f"{SELECTOR},{RUN_LABEL}={run}"
        page = 1
        while page:
            query = urllib.parse.urlencode({"label_selector": selector, "per_page": 50, "page": page})
            result = self.request("GET", f"/servers?{query}")
            yield from result["servers"]
            page = result["meta"]["pagination"]["next_page"]

    def wait_action(self, action, deadline):
        while action["status"] == "running" and time.monotonic() < deadline:
            time.sleep(2)
            action = self.request("GET", f"/actions/{action['id']}")["action"]
        if action["status"] != "success":
            raise DrillError(f"Hetzner action {action['id']} ended with status {action['status']}")

    def delete_drill(self, server_id, run, older_than=None):
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            server = self.server(server_id)
            if server is None:
                return
            if managed_run(server) != run:
                raise DrillError(f"Server {server_id} no longer has the drill's ownership labels")
            if older_than is not None and created_at(server) >= older_than:
                return
            try:
                action = self.request("DELETE", f"/servers/{server_id}")["action"]
            except ProviderError as exc:
                if exc.status == 404:
                    return
                if exc.code != "locked":
                    raise
                time.sleep(2)
                continue
            self.wait_action(action, deadline)
            while time.monotonic() < deadline:
                if self.server(server_id) is None:
                    return
                time.sleep(2)
            break
        raise DrillError(f"Hetzner did not confirm deletion of drill server {server_id}")


def managed_run(server):
    labels = server.get("labels", {})
    run = labels.get(RUN_LABEL, "")
    if isinstance(run, str) and re.fullmatch(r"[0-9a-f]{20}", run) and server.get("name") == f"hm-restore-{run}":
        if all(labels.get(key) == value for key, value in DRILL_LABELS.items()):
            return run
    return None


def created_at(server):
    return dt.datetime.fromisoformat(server["created"])


def janitor(api):
    cutoff = dt.datetime.now(dt.timezone.utc) - dt.timedelta(hours=2)
    failed = []
    for server in list(api.drills()):
        run = managed_run(server)
        if run is not None and created_at(server) < cutoff:
            try:
                api.delete_drill(server["id"], run, older_than=cutoff)
            except DrillError as exc:
                print(exc, file=sys.stderr)
                failed.append(server["id"])
    if failed:
        raise DrillError(f"Could not clean up drill server IDs {', '.join(map(str, failed))}")


def cloud_config(host_key, host_public_key, login_public_key):
    config = {
        "allow_public_ssh_keys": False,
        "disable_root": True,
        "ssh_pwauth": False,
        "ssh_deletekeys": True,
        "ssh_keys": {"ed25519_private": host_key, "ed25519_public": host_public_key},
        "users": [{"name": VM_USER, "shell": "/bin/bash", "lock_passwd": True,
                   "ssh_authorized_keys": [f"restrict {login_public_key}"]}],
        "package_update": True,
        "packages": ["age", "ca-certificates", "curl", "iproute2", "jq", "locales", "openssl", "pgbackrest",
                     "postgresql-18", "sqlite3"],
        "write_files": [{"path": "/etc/postgresql-common/createcluster.conf", "permissions": "0644",
                         "content": "create_main_cluster = false\n"}],
        "runcmd": [["locale-gen", "en_US.UTF-8"]],
    }
    return "#cloud-config\n" + json.dumps(config)


def check_inputs(directory, endpoint):
    target = urllib.parse.urlsplit(endpoint)
    if (target.scheme != "https" or not target.hostname or target.username is not None
            or target.query or target.fragment or target.path not in ("", "/")):
        raise DrillError("HEADMESH_R2_ENDPOINT must be a bare HTTPS endpoint")
    for name in CREDENTIALS:
        path = directory / name
        try:
            info = path.stat()
        except OSError:
            raise DrillError(f"Missing credential file {path}") from None
        if not stat.S_ISREG(info.st_mode) or info.st_mode & 0o077 or not path.read_bytes().strip():
            raise DrillError(f"{name} must be a nonempty private regular file")
    return target.hostname


def config_values(path, section, *keys):
    config = configparser.ConfigParser(interpolation=None)
    try:
        config.read_string(path.read_text())
        values = tuple(config.get(section, key).strip() for key in keys)
    except (configparser.Error, UnicodeError):
        raise DrillError(f"Fill {', '.join(keys)} in {path.name}") from None
    if not all(values):
        raise DrillError(f"Fill {', '.join(keys)} in {path.name}")
    return values


def netrc_pair(path, hostname):
    try:
        login, _, password = netrc.netrc(path).hosts[hostname]
    except (KeyError, netrc.NetrcParseError):
        raise DrillError(f"Fill the R2 hostname, login and password in {path.name}") from None
    if not login or not password:
        raise DrillError(f"Fill the R2 login and password in {path.name}")
    return login, password


def preflight_r2(directory, endpoint):
    hostname = check_inputs(directory, endpoint)
    aws = config_values(directory / "litestream.aws", "default", "aws_access_key_id", "aws_secret_access_key")
    pg_host, pg_key, pg_secret, _ = config_values(directory / "pgbackrest.conf", "global", "repo1-s3-endpoint",
                                                  "repo1-s3-key", "repo1-s3-key-secret", "repo1-cipher-pass")
    if pg_host != hostname:
        raise DrillError("Set repo1-s3-endpoint in pgbackrest.conf to the R2 hostname without https://")
    checks = (
        ("hm-hs1-litestream", "litestream.aws", aws),
        ("hm-pg", "pgbackrest.conf", (pg_key, pg_secret)),
        ("hm-pg", "manifest.netrc", netrc_pair(directory / "manifest.netrc", hostname)),
        ("hm-keys", "keys.netrc", netrc_pair(directory / "keys.netrc", hostname)),
    )
    with tempfile.TemporaryDirectory(prefix="headmesh-r2-check-") as scratch:
        auth = Path(scratch) / "r2.netrc"
        auth.touch(mode=0o600)
        seen = set()
        for bucket, source, keys in checks:
            entry = (bucket, keys)
            if entry in seen:
                continue
            seen.add(entry)
            login, password = keys
            auth.write_text(f"machine {hostname}\nlogin {json.dumps(login)}\npassword {json.dumps(password)}\n")
            result = subprocess.run(
                ["curl", "-q", "--silent", "--proto", "=https", "--max-time", "15", "--output", os.devnull,
                 "--write-out", "%{http_code}", "--netrc-file", auth, "--aws-sigv4", "aws:amz:auto:s3",
                 f"{endpoint.rstrip('/')}/{bucket}?list-type=2&max-keys=1"], capture_output=True, text=True,
            )
            if result.returncode or result.stdout != "200":
                status = result.stdout if re.fullmatch(r"[1-5][0-9]{2}", result.stdout) else "unavailable"
                raise DrillError(f"Could not list {bucket} using {source} ({status})")


def package_upload(work, directory, endpoint):
    package = work / "payload"
    package.mkdir(mode=0o700)
    subprocess.run(["bash", ROOT / "deploy/ha/package-restore.sh", package], check=True, timeout=600, stdout=sys.stderr)
    private = package / "private"
    private.mkdir(mode=0o700)
    for name in CREDENTIALS:
        with (private / name).open("xb") as dest:
            with (directory / name).open("rb") as source:
                shutil.copyfileobj(source, dest)
        (private / name).chmod(0o600)
    (private / "endpoint").write_text(endpoint + "\n")
    archive = work / "upload.tar"
    with tarfile.open(archive, "w") as output:
        for item in package.iterdir():
            output.add(item, arcname=item.name)
    return archive


def ssh_for(address, work, host_public_key):
    address = str(ipaddress.IPv4Address(address))
    hosts = work / "known_hosts"
    hosts.write_text(f"{address} {host_public_key}\n")
    return ["ssh", "-F", "/dev/null", "-T", "-i", str(work / "login"),
            "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={hosts}",
            "-o", "GlobalKnownHostsFile=/dev/null", "-o", "HostKeyAlgorithms=ssh-ed25519",
            "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=5",
            "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4", f"{VM_USER}@{address}"]


def run_remote(ssh, archive):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        probe = subprocess.run([*ssh, "true"], stdin=subprocess.DEVNULL, capture_output=True, timeout=10)
        if probe.returncode == 0:
            break
        time.sleep(3)
    else:
        raise DrillError("SSH never authenticated with the drill VM's pinned host key")

    subprocess.run([*ssh, "cloud-init status --wait"], stdin=subprocess.DEVNULL, stdout=sys.stderr,
                   check=True, timeout=480)
    subprocess.run([*ssh, "unshare --user --map-root-user --net ip link add restoreprobe type dummy"],
                   stdin=subprocess.DEVNULL, check=True, timeout=15)
    command = '''set -eu
umask 077
payload=$(mktemp -d)
trap 'rm -r -- "$payload"' EXIT
tar -xf - -C "$payload"
bash "$payload/remote-restore.sh" "$payload"'''
    with archive.open("rb") as upload:
        result = subprocess.run([*ssh, command], stdin=upload, stdout=subprocess.PIPE, text=True, timeout=1080)
    lines = result.stdout.splitlines()
    for line in lines[:-1] if result.returncode == 0 else lines:
        print(line, file=sys.stderr)
    result.check_returncode()
    if not lines:
        raise DrillError("The restore returned no verification report")
    try:
        report = json.loads(lines[-1])
        if any(type(report[k]) is not int or report[k] < 0 for k in ("verification_seconds", "manifest_age_seconds")):
            raise ValueError
    except (KeyError, TypeError, ValueError):
        raise DrillError("The restore returned an invalid verification report") from None
    return report


def drill(api, directory, endpoint, firewall):
    preflight_r2(directory, endpoint)
    with tempfile.TemporaryDirectory(prefix="headmesh-restore-drill-") as path:
        work = Path(path)
        archive = package_upload(work, directory, endpoint)
        for name in ("host", "login"):
            subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", f"headmesh-drill-{name}",
                            "-f", work / name], check=True)
        host_public_key = (work / "host.pub").read_text().strip()
        run = secrets.token_hex(10)
        options = {
            "name": f"hm-restore-{run}", "server_type": "cx23", "location": "fsn1", "image": "ubuntu-26.04",
            "labels": {**DRILL_LABELS, RUN_LABEL: run},
            "public_net": {"enable_ipv4": True, "enable_ipv6": False},
            "user_data": cloud_config((work / "host").read_text(), host_public_key,
                                      (work / "login.pub").read_text().strip()),
        }
        if firewall is not None:
            options["firewalls"] = [{"firewall": firewall}]
        server_id = None
        start = time.monotonic()
        try:
            print(f"Creating Hetzner restore drill {run}", file=sys.stderr)
            created = api.request("POST", "/servers", options)
            server = created["server"]
            server_id = server["id"]
            if managed_run(server) != run:
                raise DrillError("Hetzner did not return the expected drill identity")
            deadline = time.monotonic() + 180
            api.wait_action(created["action"], deadline)
            for action in created.get("next_actions", []):
                api.wait_action(action, deadline)
            server = api.server(server_id)
            if server is None or managed_run(server) != run:
                raise DrillError("Drill server disappeared or changed ownership before SSH")
            ssh = ssh_for(server["public_net"]["ipv4"]["ip"], work, host_public_key)
            print(f"Waiting for the pinned SSH host key on drill server {server_id}", file=sys.stderr)
            remote = run_remote(ssh, archive)
            elapsed = int(time.monotonic() - start)
        finally:
            if server_id is not None:
                api.delete_drill(server_id, run)
            else:
                # The API may have committed a create whose response never reached the runner.
                for candidate in api.drills(run):
                    if managed_run(candidate) == run:
                        api.delete_drill(candidate["id"], run)
        return {"run": run, "server_id": server_id, "rto_seconds": elapsed,
                "recovery_point_age_seconds": remote["manifest_age_seconds"],
                "service_verification_seconds": remote["verification_seconds"], "server_deleted": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    run = commands.add_parser("run", help="provision a disposable VM and verify R2 recovery")
    run.add_argument("--credentials", required=True, type=Path)
    run.add_argument("--firewall-id", type=int)
    preflight = commands.add_parser("preflight", help="verify the R2 credentials before starting a cloud drill")
    preflight.add_argument("--credentials", required=True, type=Path)
    commands.add_parser("janitor", help="delete expired Headmesh restore drill VMs")
    args = parser.parse_args()
    endpoint = os.getenv("HEADMESH_R2_ENDPOINT", "")
    if args.command == "preflight":
        preflight_r2(args.credentials, endpoint)
        print("Read-only credential checks passed for all three Headmesh backup buckets")
        return
    token = os.environ.pop("HCLOUD_TOKEN", "").strip()
    if not token:
        raise DrillError("HCLOUD_TOKEN is required for the dedicated Hetzner recovery project")
    api = Hetzner(token)
    if args.command == "janitor":
        janitor(api)
    else:
        print(json.dumps(drill(api, args.credentials, endpoint, args.firewall_id)))


def terminate(_signum, _frame):
    raise DrillError("Termination requested")


if __name__ == "__main__":
    os.umask(0o077)
    signal.signal(signal.SIGTERM, terminate)
    try:
        main()
    except (DrillError, OSError, ValueError, KeyError, subprocess.SubprocessError) as exc:
        print(f"Restore drill failed: {exc}", file=sys.stderr)
        sys.exit(1)
