#!/usr/bin/env python3
"""Install the inference-only gateway while its internal cable is disconnected."""

import argparse
import hashlib
import io
import ipaddress
import json
import re
import subprocess
import sys
import tarfile
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent
STATE = ROOT / ".vagrant/machines/default/virtualbox"
sys.path.insert(0, str(ROOT.parent))
from verify import setting_errors  # noqa: E402


def run(args, *, data=None):
    return subprocess.run(args, input=data, cwd=ROOT, check=True, capture_output=True, timeout=180).stdout


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", required=True, help="Inference service DNS name")
    parser.add_argument("--ip", required=True, help="Pinned inference service IPv4 address")
    parser.add_argument("--caddy", required=True, type=Path, help="Reviewed local Caddy binary")
    args = parser.parse_args()
    host = args.host.lower().rstrip(".")
    if len(host) > 253 or not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", host) or ".." in host:
        parser.error("invalid DNS name")
    try:
        ip = ipaddress.IPv4Address(args.ip)
    except ipaddress.AddressValueError:
        parser.error("--ip must be a single IPv4 address")
    if ip.is_loopback or ip.is_multicast or ip.is_unspecified or ip.is_link_local:
        parser.error("--ip must be a unicast address outside the gateway")
    binary = args.caddy.resolve(strict=True)
    if not binary.is_file() or binary.stat().st_size > 100 * 1024 * 1024:
        parser.error("invalid Caddy binary")
    vm_id = str(uuid.UUID((STATE / "id").read_text().strip()))
    info = run(["VBoxManage", "showvminfo", vm_id, "--machinereadable"]).decode()
    errors = setting_errors(info, vm_id, gateway=True, cable=False)
    if errors:
        raise SystemExit("Gateway settings are not safe for provisioning: " + "; ".join(errors))
    files = {}
    for name in ("firewall.nft", "Caddyfile"):
        files[name] = (ROOT / (name + ".template")).read_text().replace("@UPSTREAM_IP@", str(ip)).replace("@UPSTREAM_HOST@", host).encode()
    for name in ("artex-firewall.service", "artex-proxy.service", "10-artex-inference.network", "00-artex-gateway.conf", "install.sh"):
        files[name] = (ROOT / name).read_bytes()
    files["host-entry"] = f"{ip} {host} # artex-inference\n".encode()
    files["artex-caddy"] = binary.read_bytes()
    bundle = io.BytesIO()
    with tarfile.open(fileobj=bundle, mode="w") as archive:
        for name, data in files.items():
            item = tarfile.TarInfo(name)
            item.size = len(data)
            item.mode = 0o755 if name in ("install.sh", "artex-caddy") else 0o644
            archive.addfile(item, io.BytesIO(data))
    run(["vagrant", "ssh", "-c", "umask 077; mkdir -p ~/artex-gateway-stage; tar -xf - -C ~/artex-gateway-stage"], data=bundle.getvalue())
    run(["vagrant", "ssh", "-c", "~/artex-gateway-stage/artex-caddy validate --config ~/artex-gateway-stage/Caddyfile --adapter caddyfile >/dev/null && sudo -n nft --check -f ~/artex-gateway-stage/firewall.nft && bash ~/artex-gateway-stage/install.sh"])
    key = run(["vagrant", "ssh", "-c", "cat /etc/ssh/ssh_host_ed25519_key.pub"]).decode().strip().split()
    if len(key) < 2 or key[0] != "ssh-ed25519":
        raise SystemExit("Gateway did not provide an Ed25519 SSH host key")
    (STATE / "gateway-known-hosts").write_text(f"[127.0.0.1]:2223 {key[0]} {key[1]}\n")
    (ROOT / ".gateway.local").write_text(json.dumps({"host": host, "ip": str(ip), "caddy_sha256": hashlib.sha256(files["artex-caddy"]).hexdigest()}, indent=2) + "\n")
    print("Gateway configured. Verify it before connecting its internal cable.")


if __name__ == "__main__":
    main()
