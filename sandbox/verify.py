#!/usr/bin/env python3
"""Verify the ARTEX VM and its inference-only gateway before use."""

import argparse
import hashlib
import ipaddress
import json
import re
import shlex
import subprocess
import sys
import uuid
from pathlib import Path

ROOT = Path(__file__).resolve().parent
GATEWAY = ROOT / "inference-gateway"
NETWORK = "artex-inference"


def fields(info):
    return {k: v.strip().strip('"') for line in info.splitlines() if '=' in line for k, v in [line.split('=', 1)]}


def setting_errors(info, vm_id, gateway=False, cable=True):
    f = fields(info)
    expected = {
        "UUID": vm_id, "name": "artex-inference-gateway" if gateway else "artex-sandbox", "VMState": "running",
        "clipboard": "disabled", "clipboard_file_transfers": "off", "draganddrop": "disabled", "vrde": "off",
        "usb": "off", "ehci": "off", "xhci": "off",
        **{f"uart{i}": "off" for i in range(1, 5)}, **{f"lpt{i}": "off" for i in range(1, 3)},
    }
    if gateway:
        expected.update({"nic1": "nat", "cableconnected1": "on", "nic2": "intnet", "intnet2": NETWORK, "cableconnected2": "on" if cable else "off"})
        expected.update({f"nic{i}": "none" for i in range(3, 9)})
    else:
        expected.update({"nic1": "intnet", "intnet1": NETWORK, "cableconnected1": "on"})
        expected.update({f"nic{i}": "none" for i in range(2, 9)})
    errors = [f"{k}: expected {v!r}, got {f.get(k)!r}" for k, v in expected.items() if f.get(k) != v]
    forwards = [v for k, v in f.items() if k.startswith("Forwarding(")]
    expected_forward = ["ssh,tcp,127.0.0.1,2223,,22"] if gateway else []
    if forwards != expected_forward:
        errors.append("unexpected port forwarding")
    if any("SharedFolder" in k for k in f) or "AttachedHostPCI" in f:
        errors.append("shared folder or host PCI device present")
    return errors


def prestart_app_errors(info, expected_id):
    """Reject resumed machine state; cold boots reapply the Vagrant settings."""
    f = fields(info)
    if f.get("UUID") != expected_id or f.get("name") != "artex-sandbox":
        return ["ARTEX VM identity differs from the configured VM"]
    if f.get("VMState") == "poweroff":
        return []
    if f.get("VMState") == "running":
        return setting_errors(info, expected_id)
    return ["ARTEX VM must be powered off or running with verified settings; saved or paused states cannot be resumed"]


def prestart_gateway_errors(info, expected_id):
    f = fields(info)
    if f.get("UUID") != expected_id or f.get("name") != "artex-inference-gateway":
        return ["gateway VM identity differs from the configured VM"]
    if f.get("VMState") == "poweroff":
        return []
    if f.get("VMState") == "running" and f.get("cableconnected2") in ("on", "off"):
        return setting_errors(info, expected_id, gateway=True, cable=f["cableconnected2"] == "on")
    return ["gateway VM must be powered off or running with verified settings; saved or paused states cannot be resumed"]


def clean_nft(value):
    if isinstance(value, dict):
        return {k: clean_nft(v) for k, v in value.items() if k not in ("handle", "metainfo")}
    if isinstance(value, list):
        return [clean_nft(v) for v in value if not (isinstance(v, dict) and "metainfo" in v)]
    return value


def policy_errors(snapshot, config, expected_nft, firewall, caddy):
    errors = []
    expected = json.loads(json.dumps(expected_nft).replace("@UPSTREAM_IP@", config["ip"]))
    if clean_nft(snapshot["nft"]) != expected:
        errors.append("live gateway firewall differs from the approved policy")
    for key in ("firewall", "proxy"):
        if snapshot["services"].get(key) != "active":
            errors.append(f"gateway {key} service is not active")
    if snapshot["ipv4_forward"] != "0" or snapshot["ipv6_forward"] != "0":
        errors.append("gateway IP forwarding is enabled")
    if snapshot["firewall_sha256"] != hashlib.sha256(firewall).hexdigest():
        errors.append("saved gateway firewall differs from the approved configuration")
    if snapshot["caddyfile_sha256"] != hashlib.sha256(caddy).hexdigest():
        errors.append("gateway proxy configuration differs from the approved configuration")
    if snapshot["caddy_sha256"] != config["caddy_sha256"]:
        errors.append("running gateway proxy binary has changed")
    if snapshot["addresses"] != [config["ip"]]:
        errors.append("gateway inference DNS does not resolve exclusively to the pinned address")
    ssh = snapshot["sshd"]
    for line in ("passwordauthentication no", "kbdinteractiveauthentication no", "allowtcpforwarding local", "permitopen 192.0.2.2:22", "gatewayports no", "allowusers vagrant"):
        if line not in ssh:
            errors.append(f"gateway SSH setting missing: {line}")
    if not snapshot["listener"]:
        errors.append("gateway proxy is not bound exclusively to the internal address")
    return errors


def run(args, *, timeout=20):
    return subprocess.run(args, check=True, capture_output=True, text=True, timeout=timeout).stdout


def vm_id(path):
    return str(uuid.UUID(path.read_text().strip()))


def snapshot(host):
    # This code runs over the pinned host-only management SSH connection.
    script = '''import hashlib,json,os,socket,subprocess,sys
from pathlib import Path
def cmd(*a): return subprocess.check_output(a,text=True,timeout=10)
def digest(p): return hashlib.sha256(Path(p).read_bytes()).hexdigest()
pid=cmd("systemctl","show","--property=MainPID","--value","artex-proxy").strip()
listeners=[line.split()[3] for line in cmd("ss","-lnt").splitlines()[1:] if line.split()[3].endswith(":8080")]
addresses=sorted({x[4][0] for x in socket.getaddrinfo(sys.argv[1],443,socket.AF_INET,socket.SOCK_STREAM)})
print(json.dumps({"nft":json.loads(cmd("nft","-j","list","ruleset")),"services":{"firewall":cmd("systemctl","is-active","artex-firewall").strip(),"proxy":cmd("systemctl","is-active","artex-proxy").strip()},"ipv4_forward":Path("/proc/sys/net/ipv4/ip_forward").read_text().strip(),"ipv6_forward":Path("/proc/sys/net/ipv6/conf/all/forwarding").read_text().strip(),"firewall_sha256":digest("/etc/artex-gateway/firewall.nft"),"caddyfile_sha256":digest("/etc/artex-gateway/Caddyfile"),"caddy_sha256":digest("/proc/"+pid+"/exe"),"addresses":addresses,"sshd":cmd("sshd","-T").splitlines(),"listener":listeners==["192.0.2.1:8080"]}))'''
    state = GATEWAY / ".vagrant/machines/default/virtualbox"
    remote = "sudo -n /usr/bin/python3 -c " + shlex.quote(script) + " " + shlex.quote(host)
    return json.loads(run(["ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=yes", "-o", f"UserKnownHostsFile={state / 'gateway-known-hosts'}", "-i", str(state / "private_key"), "-p", "2223", "vagrant@127.0.0.1", remote], timeout=25))


def dhcp_errors(info):
    blocks = re.split(r'(?=^NetworkName:)', info, flags=re.M)
    matches = [b for b in blocks if re.search(r'^NetworkName:\s+artex-inference\s*$', b, re.M)]
    if len(matches) != 1:
        return ["dedicated DHCP service is missing or ambiguous"]
    b = matches[0]
    required = {"Dhcpd IP": "192.0.2.10", "LowerIPAddress": "192.0.2.2", "UpperIPAddress": "192.0.2.2", "NetworkMask": "255.255.255.0", "Enabled": "Yes", "Forced options": "None", "Suppressed opts.": "None", "Groups": "None", "Individual Configs": "None"}
    actual = {key.strip(): value.strip() for line in b.splitlines() if ':' in line for key, value in [line.split(':', 1)]}
    options = re.findall(r'^\s*(\d+)/(?:legacy|hex):', b, re.M)
    return [] if all(actual.get(key) == value for key, value in required.items()) and options == ["1"] else ["dedicated DHCP service has unexpected settings"]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gateway-only", action="store_true")
    parser.add_argument("--pre-start", action="store_true", help="also check the ARTEX VM before starting it")
    parser.add_argument("--gateway-pre-start", action="store_true", help="check gateway VM state before boot")
    parser.add_argument("--cable-off", action="store_true", help="check gateway before connecting its internal cable")
    args = parser.parse_args()
    if sum((args.gateway_only, args.pre_start, args.gateway_pre_start)) > 1:
        parser.error("verification modes cannot be combined")
    if args.cable_off and not args.gateway_only:
        parser.error("--cable-off requires --gateway-only")
    try:
        if args.gateway_pre_start:
            state = GATEWAY / ".vagrant/machines/default/virtualbox/id"
            if state.exists():
                gateway_id = vm_id(state)
                errors = prestart_gateway_errors(run(["VBoxManage", "showvminfo", gateway_id, "--machinereadable"]), gateway_id)
                if errors:
                    raise ValueError("; ".join(errors))
            print("Gateway pre-start state verified")
            return 0
        config = json.loads((GATEWAY / ".gateway.local").read_text())
        config["ip"] = str(ipaddress.IPv4Address(config["ip"]))
        if not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?", config["host"]) or not re.fullmatch(r"[0-9a-f]{64}", config["caddy_sha256"]):
            raise ValueError("invalid local gateway configuration")
        gateway_id = vm_id(GATEWAY / ".vagrant/machines/default/virtualbox/id")
        app_state = ROOT / ".vagrant/machines/default/virtualbox/id"
        app_id = vm_id(app_state) if app_state.exists() else None
        info = run(["VBoxManage", "showvminfo", gateway_id, "--machinereadable"])
        errors = setting_errors(info, gateway_id, gateway=True, cable=not args.cable_off)
        if args.pre_start and app_id is not None:
            errors += prestart_app_errors(run(["VBoxManage", "showvminfo", app_id, "--machinereadable"]), app_id)
        elif not args.gateway_only and not args.pre_start:
            if app_id is None:
                raise ValueError("ARTEX VM has not been created")
            errors += setting_errors(run(["VBoxManage", "showvminfo", app_id, "--machinereadable"]), app_id)
        errors += dhcp_errors(run(["VBoxManage", "list", "dhcpservers"]))
        for other in re.findall(r'\{([0-9a-fA-F-]+)\}', run(["VBoxManage", "list", "runningvms"])):
            f = fields(run(["VBoxManage", "showvminfo", other, "--machinereadable"]))
            if other not in (gateway_id, app_id) and any(f.get(f"intnet{i}") == NETWORK and f.get(f"nic{i}") == "intnet" for i in range(1, 9)):
                errors.append("another running VM is attached to the internal network")
        firewall = (GATEWAY / "firewall.nft.template").read_text().replace("@UPSTREAM_IP@", config["ip"]).encode()
        caddy = (GATEWAY / "Caddyfile.template").read_text().replace("@UPSTREAM_HOST@", config["host"]).encode()
        expected = json.loads((GATEWAY / "firewall.expected.json").read_text())
        errors += policy_errors(snapshot(config["host"]), config, expected, firewall, caddy)
    except (OSError, ValueError, KeyError, json.JSONDecodeError, subprocess.SubprocessError) as exc:
        print(f"Verification failed: {exc}", file=sys.stderr)
        return 1
    if errors:
        for error in errors:
            print(f"FAIL: {error}", file=sys.stderr)
        return 1
    print("Gateway and pre-start policy verified" if args.pre_start else "Gateway policy verified" if args.gateway_only else "ARTEX isolation and gateway policy verified")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
