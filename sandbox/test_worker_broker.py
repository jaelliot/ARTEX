import json
import os
import shlex
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import worker_broker as broker


class WorkerBrokerTest(unittest.TestCase):
    def test_broker_rejects_untrusted_installation(self):
        broker.validate_installed_file(Path(sys.executable).resolve())
        with tempfile.TemporaryDirectory() as tmp:
            worker = Path(tmp) / "worker"
            worker.write_text("#!/bin/sh\nexit 0\n")
            worker.chmod(0o700)
            with self.assertRaises(ValueError):
                broker.validate_installed_file(worker)
            link = Path(tmp) / "linked-worker"
            link.symlink_to(Path(sys.executable).resolve())
            with self.assertRaises(ValueError):
                broker.validate_installed_file(link)

    def test_uncertain_cleanup_stops_broker(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = Path(tmp) / "worker"
            worker.write_text("#!/bin/sh\nexit 125\n")
            worker.chmod(0o700)
            with self.assertRaises(SystemExit):
                broker.run_worker(b"test", worker)

    def test_request_is_a_bounded_script_only(self):
        self.assertEqual(broker.parse_request(b'{"script":"echo hello"}\n'), b"echo hello")
        for line in (b'{}\n', b'{"script": 1}\n', b'{"script":"x","image":"other"}\n',
                     b'{"script":"\\u0000"}\n', b'{"script":"x"}', b'x' * (broker.MAX_REQUEST + 1)):
            with self.subTest(line=line[:80]):
                with self.assertRaises(ValueError):
                    broker.parse_request(line)

    def test_script_is_passed_as_stdin_with_minimal_environment(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = Path(tmp) / "worker"
            worker.write_text("#!/bin/sh\nprintf 'home=%s\\n' \"$HOME\"\ncat\n")
            worker.chmod(0o700)
            with patch.dict(os.environ, {"ARTEX_TEST_SECRET": "not-for-worker"}):
                result = broker.run_worker(b'echo "$(not-a-host-command)"\n', worker)
            self.assertEqual(result, {"ok": True, "output": 'home=/var/empty\necho "$(not-a-host-command)"\n'})

    def test_output_limit_stops_broker_if_launcher_is_killed(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = Path(tmp) / "worker"
            worker.write_text("#!/bin/sh\nhead -c 3145728 /dev/zero\n")
            worker.chmod(0o700)
            with self.assertRaises(SystemExit):
                broker.run_worker(b"ignored", worker)

    def test_offline_worker_uses_pinned_image_and_cleans_up(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            docker = root / "docker"
            docker.write_text('''#!/bin/sh
case "$1 $2" in
  "image inspect")
    if [ "$4" = '{{.Id}}' ]; then echo sha256:fixture; else echo 0; fi ;;
  "inspect "*) exit 1 ;;
  "create "*) printf '%s\\n' "$@" > "$FAKE_DIR/create.args" ;;
  "start "*) cat ;;
  "rm -f") touch "$FAKE_DIR/removed" ;;
  *) exit 1 ;;
esac
''')
            docker.chmod(0o700)
            launcher = root / "launcher"
            launcher.write_text("#!/bin/sh\nexport FAKE_DIR=" + shlex.quote(tmp) + "\n"
                                "export PATH=" + shlex.quote(tmp) + ":/usr/bin:/bin\n"
                                "exec " + shlex.quote(str(broker.WORKER)) + "\n")
            launcher.chmod(0o700)
            result = broker.run_worker(b"echo harmless\n", launcher)
            self.assertEqual(result, {"ok": True, "output": "echo harmless\n"})
            args = (root / "create.args").read_text().splitlines()
            self.assertIn("sha256:fixture", args)
            self.assertIn("--read-only", args)
            self.assertIn("none", args)
            self.assertIn("ALL", args)
            self.assertTrue((root / "removed").exists())

    def test_failed_docker_removal_stops_broker(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            docker = root / "docker"
            docker.write_text('''#!/bin/sh
case "$1 $2" in
  "image inspect")
    if [ "$4" = '{{.Id}}' ]; then echo sha256:fixture; else echo 0; fi ;;
  "inspect "*) exit 1 ;;
  "create "*) exit 0 ;;
  "start "*) cat ;;
  "rm -f") exit 1 ;;
  *) exit 1 ;;
esac
''')
            docker.chmod(0o700)
            launcher = root / "launcher"
            launcher.write_text("#!/bin/sh\nexport PATH=" + shlex.quote(tmp) + ":/usr/bin:/bin\n"
                                "exec " + shlex.quote(str(broker.WORKER)) + "\n")
            launcher.chmod(0o700)
            with self.assertRaises(SystemExit):
                broker.run_worker(b"echo harmless\n", launcher)

    def test_interrupted_broker_stops_its_worker(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = Path(tmp) / "worker"
            started, stopped = Path(tmp) / "started", Path(tmp) / "stopped"
            worker.write_text("#!/bin/sh\ntrap 'touch " + shlex.quote(str(stopped)) + "' EXIT\n"
                              "trap 'exit 143' TERM\n"
                              "touch " + shlex.quote(str(started)) + "\nsleep 20\n")
            worker.chmod(0o700)
            code = ("import signal,sys,worker_broker as b\n"
                    "def stop(*_): raise KeyboardInterrupt\n"
                    "signal.signal(signal.SIGTERM, stop)\n"
                    "try: b.run_worker(b'test', sys.argv[1])\n"
                    "except KeyboardInterrupt: pass\n")
            process = subprocess.Popen([sys.executable, "-c", code, str(worker)])
            try:
                deadline = time.monotonic() + 5
                while not started.exists() and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(started.exists(), "fake worker did not start")
                process.terminate()
                self.assertEqual(process.wait(timeout=5), 0)
                self.assertTrue(stopped.exists(), "worker cleanup did not run")
            finally:
                if process.poll() is None:
                    process.kill()
                    process.wait()

    def test_socket_dispatch_rejects_extra_fields(self):
        with tempfile.TemporaryDirectory() as tmp:
            socket_path = str(Path(tmp) / "worker.sock")
            with socketserver_for_test(socket_path) as server:
                thread = threading.Thread(target=server.handle_request)
                thread.start()
                with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                    client.connect(socket_path)
                    client.sendall(b'{"script":"x","image":"other"}\n')
                    data = client.makefile("rb").readline()
                thread.join(timeout=5)
                self.assertFalse(thread.is_alive())
                self.assertFalse(json.loads(data)["ok"])

    def test_socket_disconnect_stops_worker(self):
        with tempfile.TemporaryDirectory() as tmp:
            worker = Path(tmp) / "worker"
            started, stopped = Path(tmp) / "started", Path(tmp) / "stopped"
            worker.write_text("#!/bin/sh\ntrap 'touch " + shlex.quote(str(stopped)) + "' EXIT\n"
                              "trap 'exit 143' TERM\n"
                              "touch " + shlex.quote(str(started)) + "\nsleep 20\n")
            worker.chmod(0o700)
            run_worker = broker.run_worker
            with socketserver_for_test(str(Path(tmp) / "worker.sock")) as server:
                with patch.object(broker, "run_worker", lambda script, canceled: run_worker(script, worker, canceled)):
                    thread = threading.Thread(target=server.handle_request)
                    thread.start()
                    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                        client.connect(str(Path(tmp) / "worker.sock"))
                        client.sendall(b'{"script":"test"}\n')
                        deadline = time.monotonic() + 5
                        while not started.exists() and time.monotonic() < deadline:
                            time.sleep(0.02)
                        self.assertTrue(started.exists(), "fake worker did not start")
                    thread.join(timeout=5)
                    self.assertFalse(thread.is_alive(), "broker did not stop after disconnect")
                    self.assertTrue(stopped.exists(), "worker cleanup did not run")


def socketserver_for_test(path):
    return broker.socketserver.UnixStreamServer(path, broker.WorkerHandler)


if __name__ == "__main__":
    unittest.main()
