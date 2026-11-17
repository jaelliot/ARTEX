#!/usr/bin/env python3
"""Dummy-only broker check; run in a networkless client container, never on a host."""

import argparse
import json
import os
import socket
import stat
import struct
from pathlib import Path


def request(path, script):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(30)
        client.connect(str(path))
        _, uid, _ = struct.unpack("3i", client.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        if uid != 0:
            raise RuntimeError("broker peer must be root")
        client.sendall(json.dumps({"script": script}).encode() + b"\n")
        response = json.loads(client.makefile("rb").readline(65536))
        if not response.get("ok"):
            raise RuntimeError("dummy worker request failed: " + str(response))
        return response["output"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("socket", type=Path)
    parser.add_argument("--hold", action="store_true", help="hold a dummy job until the fixture controller kills the broker")
    args = parser.parse_args()
    if os.geteuid() == 0 or os.listdir("/sys/class/net") != ["lo"]:
        raise RuntimeError("run this check as non-root in a networkless client container")
    info = args.socket.lstat()
    if not stat.S_ISSOCK(info.st_mode) or info.st_uid != 0 or stat.S_IMODE(info.st_mode) != 0o660:
        raise RuntimeError("expected a root-owned group-accessible broker socket")
    if not os.statvfs(args.socket.parent).f_flag & os.ST_RDONLY:
        raise RuntimeError("broker directory must be mounted read-only in the client")
    if Path("/var/run/docker.sock").exists():
        raise RuntimeError("the client must not receive a Docker socket")
    secret = args.socket.parent / "dummy-secret"
    if secret.exists():
        try:
            secret.read_bytes()
        except PermissionError:
            pass
        else:
            raise RuntimeError("client could read the fixture's private broker file")
    if args.hold:
        try:
            request(args.socket, "sleep 120")
        except (OSError, ValueError):
            print("broker-connection-closed")
            return
        raise RuntimeError("broker did not terminate the held fixture job")
    script = """python3 - <<'PY'
import json,os,pathlib
status = dict(line.split(':', 1) for line in pathlib.Path('/proc/self/status').read_text().splitlines() if ':' in line)
root_writable = True
try:
    pathlib.Path('/artex-dummy-write').write_text('fixture')
except OSError:
    root_writable = False
pathlib.Path('/work/task-artifact').write_text('fixture')
print(json.dumps(dict(uid=os.getuid(), capabilities=status['CapEff'].strip(),
    seccomp=status['Seccomp'].strip(), no_new_privs=status['NoNewPrivs'].strip(),
    apparmor=pathlib.Path('/proc/self/attr/current').read_text().strip(),
    interfaces=os.listdir('/sys/class/net'), root_writable=root_writable,
    docker_socket=pathlib.Path('/var/run/docker.sock').exists(),
    home=os.environ['HOME'], inherited_secret='ARTEX_DUMMY_SECRET' in os.environ,
    broker_mount=pathlib.Path('/broker').exists())))
PY
"""
    controls = json.loads(request(args.socket, script))
    expected = {"uid": 65534, "capabilities": "0000000000000000", "seccomp": "2",
                "no_new_privs": "1", "interfaces": ["lo"], "root_writable": False,
                "docker_socket": False, "home": "/work", "inherited_secret": False,
                "broker_mount": False}
    for key, value in expected.items():
        if controls[key] != value:
            raise RuntimeError(f"unexpected {key}: {controls[key]!r}")
    if "docker-default" not in controls["apparmor"] or "enforce" not in controls["apparmor"]:
        raise RuntimeError("Docker AppArmor profile is not enforced")
    if request(args.socket, "test ! -e /work/task-artifact && printf fresh-workspace") != "fresh-workspace":
        raise RuntimeError("worker files persisted between requests")
    print(json.dumps({"broker_peer_uid": 0, "client_docker_socket": False,
                      "socket_mount_read_only": True, "fresh_workspaces": True,
                      "worker_controls": controls}, sort_keys=True))


if __name__ == "__main__":
    main()
