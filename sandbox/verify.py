#!/usr/bin/env python3
"""Check the ARTEX VM's VirtualBox settings without changing the VM."""

import re
import subprocess
import sys
import uuid
import xml.etree.ElementTree as ET
from pathlib import Path


def setting_errors(info, details, xml, vm_id):
    errors = []
    fields = {}
    for line in info.splitlines():
        key, separator, value = line.partition("=")
        if separator:
            fields[key] = value.strip().strip('"')

    required = {
        "UUID": vm_id,
        "name": "artex-sandbox",
        "VMState": "running",
        "nic1": "nat",
        "clipboard": "disabled",
        "clipboard_file_transfers": "off",
        "draganddrop": "disabled",
        "vrde": "off",
        **{f"nic{i}": "none" for i in range(2, 9)},
    }
    for key, expected in required.items():
        if fields.get(key) != expected:
            errors.append(f"{key}: expected {expected!r}, got {fields.get(key)!r}")

    forwards = [value.split(",") for key, value in fields.items() if key.startswith("Forwarding(")]
    if len(forwards) != 1 or len(forwards[0]) != 6:
        errors.append("expected exactly one SSH forwarding rule")
    else:
        name, protocol, host, port, guest, guest_port = forwards[0]
        expected = ("ssh", "tcp", "127.0.0.1", "", "22")
        if (name, protocol, host, guest, guest_port) != expected or not port.isdigit() or not 1024 < int(port) <= 65535:
            errors.append("SSH forwarding is not restricted to host loopback")

    if any("SharedFolder" in key for key in fields):
        errors.append("VirtualBox reports a shared folder")
    matches = re.findall(r"LocalhostReachable:\s*([01])", details)
    if matches != ["0"]:
        errors.append("effective NAT host-loopback restriction is not confirmed")

    try:
        root = ET.fromstring(xml)
        adapters = [e for e in root.iter() if e.tag.rsplit("}", 1)[-1] == "Adapter" and e.get("slot") == "0"]
        nats = [e for adapter in adapters for e in adapter if e.tag.rsplit("}", 1)[-1] == "NAT"]
        if len(nats) != 1 or nats[0].get("localhost-reachable") != "false":
            errors.append("saved NAT host-loopback restriction is not confirmed")
        if any(e.tag.rsplit("}", 1)[-1] == "SharedFolder" for e in root.iter()):
            errors.append("saved configuration contains a shared folder")
    except ET.ParseError:
        errors.append("saved VirtualBox configuration is invalid")
    return errors


def main():
    state = Path(__file__).resolve().parent / ".vagrant/machines/default/virtualbox/id"
    try:
        vm_id = str(uuid.UUID(state.read_text().strip()))
        def show(*args):
            return subprocess.run(
                ["VBoxManage", "showvminfo", vm_id, *args],
                check=True, capture_output=True, text=True, timeout=15,
            ).stdout
        info = show("--machinereadable")
        match = re.search(r'^CfgFile="(.*)"$', info, re.MULTILINE)
        if not match:
            raise ValueError("VirtualBox did not report the saved configuration path")
        errors = setting_errors(info, show("--details"), Path(match.group(1)).read_text(), vm_id)
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        print(f"VM settings could not be verified: {exc}", file=sys.stderr)
        return 1
    if errors:
        for error in errors:
            print(f"FAIL: {error}", file=sys.stderr)
        return 1
    print("VM settings passed. Target/model egress and host-network isolation still require independent enforcement and testing.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
