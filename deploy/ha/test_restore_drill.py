import contextlib
import datetime as dt
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import io
import json
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch

from restore_drill import CREDENTIALS, DRILL_LABELS, RUN_LABEL, DrillError, Hetzner, drill, janitor, preflight_r2, run_remote


class CloudFixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        options = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        state = self.server.fixture
        state["create"] = options
        record = {"id": 1, "labels": options["labels"], "name": options["name"],
                  "public_net": {"ipv4": {"ip": "192.0.2.10"}}}
        state["servers"][1] = record
        if state.get("lose_create_response"):
            self.reply(502, {"error": {"code": "upstream"}})
            return
        self.reply(201, {"server": record, "action": {"id": 41, "status": "running"}})

    def do_GET(self):
        state = self.server.fixture
        if self.path.startswith("/servers?"):
            self.reply(200, {"servers": list(state["servers"].values()), "meta": {"pagination": {"next_page": None}}})
        elif self.path.startswith("/actions/"):
            ident = int(self.path.rsplit("/", 1)[1])
            state["actions"].append(ident)
            self.reply(200, {"action": {"id": ident, "status": "success"}})
        else:
            ident = int(self.path.rsplit("/", 1)[1])
            if ident in state["servers"]:
                self.reply(200, {"server": state["servers"][ident]})
            else:
                self.reply(404, {"error": {"code": "not_found"}})

    def do_DELETE(self):
        ident = int(self.path.rsplit("/", 1)[1])
        state = self.server.fixture
        state["deleted"].append(ident)
        del state["servers"][ident]
        self.reply(200, {"action": {"id": 42, "status": "running"}})


@contextlib.contextmanager
def provider():
    with ThreadingHTTPServer(("127.0.0.1", 0), CloudFixture) as server:
        server.fixture = {"servers": {}, "deleted": [], "actions": []}
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield Hetzner("fixture", f"http://127.0.0.1:{server.server_port}"), server.fixture
        finally:
            server.shutdown()
            thread.join()


def credentials(path):
    for filename in CREDENTIALS:
        item = path / filename
        item.write_text("fixture-private-material")
        item.chmod(0o600)


class RestoreDrillTest(unittest.TestCase):
    @patch("restore_drill.preflight_r2")
    @patch("restore_drill.time.sleep")
    @patch("restore_drill.package_upload")
    @patch("restore_drill.run_remote")
    def test_runs_only_with_pinned_host_and_confirms_deletion(self, remote, package, _, preflight):
        with provider() as (api, state), tempfile.TemporaryDirectory() as folder:
            directory = Path(folder)
            credentials(directory)

            def verify_ssh(ssh, _archive):
                self.assertIn("StrictHostKeyChecking=yes", ssh)
                known = next(arg.split("=", 1)[1] for arg in ssh if arg.startswith("UserKnownHostsFile="))
                self.assertTrue(Path(known).read_text().startswith("192.0.2.10 ssh-ed25519 "))
                self.assertEqual(state["actions"], [41])
                return {"verification_seconds": 12, "manifest_age_seconds": 31}

            remote.side_effect = verify_ssh
            report = drill(api, directory, "https://fixture.example.test", 12)
            preflight.assert_called_once()
            package.assert_called_once()
            self.assertEqual(state["create"]["server_type"], "cx23")
            self.assertEqual(state["create"]["firewalls"], [{"firewall": 12}])
            self.assertNotIn("fixture-private-material", state["create"]["user_data"])
            cloud_init = json.loads(state["create"]["user_data"].removeprefix("#cloud-config\n"))
            self.assertIn("locales", cloud_init["packages"])
            self.assertIn(["locale-gen", "en_US.UTF-8"], cloud_init["runcmd"])
            self.assertEqual(state["actions"], [41, 42])
            self.assertEqual(state["deleted"], [1])
            self.assertEqual(state["servers"], {})
            self.assertTrue(report["server_deleted"])
            self.assertEqual(report["recovery_point_age_seconds"], 31)

    @patch("restore_drill.preflight_r2")
    @patch("restore_drill.time.sleep")
    @patch("restore_drill.package_upload")
    @patch("restore_drill.run_remote", side_effect=DrillError("unusable SSH"))
    def test_cleans_after_remote_failure_and_ambiguous_create(self, remote, _package, _sleep, _preflight):
        for lose_response in (False, True):
            with self.subTest(lose_response=lose_response), provider() as (api, state), tempfile.TemporaryDirectory() as folder:
                credentials(Path(folder))
                state["lose_create_response"] = lose_response
                with self.assertRaises(DrillError):
                    drill(api, Path(folder), "https://fixture.example.test", None)
                self.assertEqual(state["deleted"], [1])
                self.assertEqual(state["servers"], {})
            if not lose_response:
                remote.assert_called_once()
                remote.reset_mock()
            else:
                remote.assert_not_called()

    @patch("restore_drill.subprocess.run")
    def test_preflight_checks_the_bucket_specific_files(self, command):
        with tempfile.TemporaryDirectory() as folder:
            directory = Path(folder)
            credentials(directory)
            files = {
                "litestream.aws": "[default]\naws_access_key_id=id-one\naws_secret_access_key=one-secret\n",
                "pgbackrest.conf": "[global]\nrepo1-s3-endpoint=fixture.example.test\nrepo1-s3-key=id-two\n"
                                   "repo1-s3-key-secret=two-secret\nrepo1-cipher-pass=fixture-pass\n",
                "manifest.netrc": "machine fixture.example.test login id-two password two-secret\n",
                "keys.netrc": "machine fixture.example.test login id-three password three-secret\n",
            }
            for name, data in files.items():
                (directory / name).write_text(data)
            command.return_value = subprocess.CompletedProcess([], 0, stdout="200")
            preflight_r2(directory, "https://fixture.example.test")
            calls = [call.args[0] for call in command.call_args_list]
            self.assertEqual([args[-1].split("/")[-1].split("?")[0] for args in calls],
                             ["hm-hs1-litestream", "hm-pg", "hm-keys"])
            self.assertTrue(all("--netrc-file" in args for args in calls))
            self.assertNotIn("two-secret", str(calls))
            command.reset_mock()
            (directory / "keys.netrc").write_text('machine ""\n login ""\n password ""\n')
            with self.assertRaisesRegex(DrillError, "keys.netrc"):
                preflight_r2(directory, "https://fixture.example.test")
            command.assert_not_called()

    @patch("restore_drill.time.sleep")
    def test_janitor_ignores_recent_and_other_ephemeral_servers(self, _):
        with provider() as (api, state):
            now = dt.datetime.now(dt.timezone.utc)
            run = "a" * 20
            old = {"name": f"hm-restore-{run}", "created": (now - dt.timedelta(hours=3)).isoformat(),
                   "labels": {**DRILL_LABELS, RUN_LABEL: run}}
            state["servers"].update({
                1: {**old, "id": 1},
                2: {**old, "id": 2, "created": now.isoformat()},
                3: {**old, "id": 3, "labels": {"ephemeral": "true"}},
                4: {**old, "id": 4, "name": "some-other-server"},
            })
            janitor(api)
            self.assertEqual(state["deleted"], [1])

    def test_cleanup_rechecks_ownership(self):
        with provider() as (api, state):
            state["servers"][1] = {"id": 1, "name": "primary", "labels": {"ephemeral": "true"}}
            with self.assertRaises(DrillError):
                api.delete_drill(1, "a" * 20)
            self.assertEqual(state["deleted"], [])

    @patch("restore_drill.subprocess.run")
    def test_upload_happens_after_ssh_and_cloud_init(self, command):
        output = json.dumps({"verification_seconds": 9, "manifest_age_seconds": 11})
        command.side_effect = [subprocess.CompletedProcess([], 0)] * 3 + [
                               subprocess.CompletedProcess([], 0, stdout=output + "\n")]
        with tempfile.TemporaryDirectory() as folder, contextlib.redirect_stderr(io.StringIO()):
            archive = Path(folder) / "archive"
            archive.write_bytes(b"synthetic upload")
            self.assertEqual(run_remote(["ssh"], archive)["manifest_age_seconds"], 11)
        calls = command.call_args_list
        self.assertEqual(calls[0].args[0], ["ssh", "true"])
        self.assertEqual(calls[1].args[0], ["ssh", "cloud-init status --wait"])
        self.assertIn("unshare --user", calls[2].args[0][-1])
        self.assertEqual(calls[3].kwargs["stdin"].name, str(archive))

    @patch("restore_drill.subprocess.run")
    def test_failed_vm_preflight_prevents_upload(self, command):
        command.side_effect = [subprocess.CompletedProcess([], 0), subprocess.CompletedProcess([], 0),
                               subprocess.CalledProcessError(1, "network namespace probe")]
        with self.assertRaises(subprocess.CalledProcessError):
            run_remote(["ssh"], Path("this-archive-must-not-be-opened"))
        self.assertEqual(command.call_count, 3)


if __name__ == "__main__":
    unittest.main()
