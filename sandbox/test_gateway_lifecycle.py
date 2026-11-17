import shlex
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent


class GatewayLifecycleTest(unittest.TestCase):
    def test_start_disconnects_on_every_failed_stage(self):
        for failure in ("", "gateway-up", "precheck", "postcheck", "app-up", "finalcheck"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                gateway = root / "inference-gateway"
                state = gateway / ".vagrant/machines/default/virtualbox"
                state.mkdir(parents=True)
                (state / "id").write_text("fixture-gateway")
                script = root / "start"
                script.write_bytes((ROOT / "start").read_bytes())
                bin_dir = root / "bin"
                bin_dir.mkdir()
                commands = {
                    "VBoxManage": '''#!/bin/sh
echo "$*" >> "$CALLS"
case "$1" in
list) echo 'NetworkName: artex-inference';;
showvminfo) echo 'cableconnected2="off"';;
esac
''',
                    "vagrant": '''#!/bin/sh
case "$PWD" in
*/inference-gateway) stage=gateway-up;;
*) stage=app-up;;
esac
echo "$stage" >> "$CALLS"
[ "$FAIL_AT" != "$stage" ]
''',
                    "python3": '''#!/bin/sh
case "$*" in
*--cable-off*) stage=precheck;;
*--gateway-only*) stage=postcheck;;
*) stage=finalcheck;;
esac
echo "$stage" >> "$CALLS"
[ "$FAIL_AT" != "$stage" ]
''',
                }
                for name, body in commands.items():
                    path = bin_dir / name
                    path.write_text(body)
                    path.chmod(0o700)
                calls = root / "calls"
                result = subprocess.run(
                    ["/bin/bash", str(script)], timeout=10, capture_output=True,
                    env={"PATH": f"{bin_dir}:/usr/bin:/bin", "HOME": tmp,
                         "CALLS": str(calls), "FAIL_AT": failure},
                )
                events = calls.read_text().splitlines()
                disconnect = "controlvm fixture-gateway setlinkstate2 off"
                if failure:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(disconnect, events)
                    self.assertEqual(events[-1], disconnect)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertNotIn(disconnect, events)
                    self.assertLess(events.index("postcheck"), events.index("app-up"))
                    self.assertEqual(events[-1], "finalcheck")

    def test_install_applies_firewall_before_interface_and_contains_failures(self):
        for failure in ("", "systemctl restart artex-firewall.service",
                        "systemctl restart artex-proxy.service",
                        "systemctl is-active --quiet artex-firewall.service artex-proxy.service"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "host-entry").write_text("192.0.2.20 model.invalid # fixture\n")
                script = root / "install.sh"
                script.write_text((ROOT / "inference-gateway/install.sh").read_text().replace(
                    "stage=/home/vagrant/artex-gateway-stage", "stage=" + shlex.quote(tmp)))
                bin_dir = root / "bin"
                bin_dir.mkdir()
                sudo = bin_dir / "sudo"
                sudo.write_text('''#!/bin/sh
shift
echo "$*" >> "$CALLS"
if [ "$1" = tee ]; then cat >/dev/null; fi
[ "$FAIL_AT" != "$*" ]
''')
                sudo.chmod(0o700)
                calls = root / "calls"
                result = subprocess.run(
                    ["/bin/bash", str(script)], timeout=10, capture_output=True,
                    env={"PATH": f"{bin_dir}:/usr/bin:/bin", "HOME": tmp,
                         "CALLS": str(calls), "FAIL_AT": failure},
                )
                events = calls.read_text().splitlines()
                if failure:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(events[-2:], ["ip link set enp0s8 down", "systemctl stop artex-proxy.service"])
                    if "firewall" in failure and failure.startswith("systemctl restart"):
                        self.assertNotIn("ip link set enp0s8 up", events)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertLess(events.index("systemctl restart artex-firewall.service"), events.index("ip link set enp0s8 up"))
                    self.assertLess(events.index("ip link set enp0s8 up"), events.index("systemctl restart artex-proxy.service"))


if __name__ == "__main__":
    unittest.main()
