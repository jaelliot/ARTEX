#!/usr/bin/env python3
"""Local, single-job entry point for the offline worker prototype."""

import argparse
import json
import os
import select
import signal
import socket
import socketserver
import stat
import subprocess
import tempfile
import threading
import time
from pathlib import Path

MAX_REQUEST = 65536
MAX_OUTPUT = 2 * 1024 * 1024
WORKER = Path(__file__).resolve().with_name("offline-worker")


def parse_request(line):
    if len(line) > MAX_REQUEST or not line.endswith(b"\n"):
        raise ValueError("request is too large or incomplete")
    try:
        request = json.loads(line)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("invalid JSON request") from exc
    if not isinstance(request, dict) or set(request) != {"script"} or not isinstance(request["script"], str):
        raise ValueError("request must contain only a script string")
    if "\x00" in request["script"]:
        raise ValueError("script must not contain NUL")
    return request["script"].encode("utf-8")


def run_worker(script, worker=WORKER, canceled=None):
    # The worker has its own timeout and container cleanup. This outer bound
    # covers the launcher too, and the file limit bounds its combined output.
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(
            ["/usr/bin/prlimit", f"--fsize={MAX_OUTPUT}:{MAX_OUTPUT}", "--", str(worker)],
            stdin=subprocess.PIPE, stdout=output, stderr=subprocess.STDOUT,
            env={"PATH": "/usr/bin:/bin", "HOME": "/var/empty"},
            start_new_session=True,
        )
        stopping = False

        def stop():
            nonlocal stopping
            stopping = True
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.communicate(timeout=20)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                try:
                    process.communicate(timeout=5)
                except subprocess.TimeoutExpired as exc:
                    raise SystemExit("worker did not stop; broker is shutting down") from exc

        deadline = time.monotonic() + 180
        pending_input = script
        try:
            while True:
                if canceled is not None and canceled.is_set():
                    stop()
                    return {"ok": False, "error": "client disconnected"}
                if time.monotonic() >= deadline:
                    stop()
                    return {"ok": False, "error": "worker timed out"}
                try:
                    process.communicate(pending_input, timeout=0.2)
                    break
                except subprocess.TimeoutExpired:
                    pending_input = None
        finally:
            if not stopping and process.poll() is None:
                stop()
        output.seek(0)
        body = output.read(MAX_OUTPUT).decode("utf-8", errors="replace")
        return {"ok": process.returncode == 0, "output": body}


class WorkerHandler(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(5)
        try:
            script = parse_request(self.rfile.readline(MAX_REQUEST + 1))
            canceled, finished = threading.Event(), threading.Event()

            def watch_client():
                while not finished.is_set():
                    try:
                        ready, _, _ = select.select([self.connection], [], [], 0.2)
                        if ready and not self.connection.recv(1, socket.MSG_PEEK):
                            canceled.set()
                            return
                        if ready:
                            canceled.set()
                            return
                    except OSError:
                        canceled.set()
                        return

            watcher = threading.Thread(target=watch_client, daemon=True)
            watcher.start()
            try:
                response = run_worker(script, canceled=canceled)
            finally:
                finished.set()
                watcher.join(timeout=1)
        except (OSError, ValueError) as exc:
            response = {"ok": False, "error": str(exc)}
        try:
            self.wfile.write(json.dumps(response, ensure_ascii=True).encode("ascii") + b"\n")
        except (BrokenPipeError, ConnectionResetError):
            pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("socket", type=Path, help="new Unix socket path in a trusted directory")
    parser.add_argument("--group", type=int, help="numeric group allowed to connect")
    args = parser.parse_args()
    if args.group is not None and not 0 <= args.group < 2**32 - 1:
        parser.error("group must be a non-negative numeric group ID")
    path = args.socket
    if not path.is_absolute() or path.parent.resolve(strict=True) != path.parent:
        parser.error("socket must have an absolute path without symlinked parent directories")
    parent = path.parent.stat()
    if parent.st_uid != os.geteuid() or parent.st_mode & 0o022:
        parser.error("socket directory must be owned by the broker and not group/world writable")
    if os.path.lexists(path):
        parser.error("socket path already exists; inspect and remove it before restarting")
    old_umask = os.umask(0o177)
    try:
        server = socketserver.UnixStreamServer(str(path), WorkerHandler)
    finally:
        os.umask(old_umask)
    identity = os.lstat(path)
    try:
        if args.group is not None:
            os.chown(path, -1, args.group)
            os.chmod(path, 0o660)
        def stop(_signum, _frame):
            raise KeyboardInterrupt
        signal.signal(signal.SIGTERM, stop)
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        try:
            current = os.lstat(path)
            if stat.S_ISSOCK(current.st_mode) and (current.st_dev, current.st_ino) == (identity.st_dev, identity.st_ino):
                path.unlink()
        except FileNotFoundError:
            pass


if __name__ == "__main__":
    main()
