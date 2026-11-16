import contextlib
import io
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import verify
from verify import setting_errors


VM_ID = "12345678-1234-4234-8234-123456789abc"
INFO = f'''UUID="{VM_ID}"
name="artex-sandbox"
VMState="running"
nic1="nat"
''' + "".join(f'nic{i}="none"\n' for i in range(2, 9)) + '''clipboard="disabled"
clipboard_file_transfers="off"
draganddrop="disabled"
vrde="off"
Forwarding(0)="ssh,tcp,127.0.0.1,2222,,22"
'''
DETAILS = "LocalhostReachable: 0, ForwardBroadcast: 0"
XML = '<VirtualBox><Machine><Hardware><Network><Adapter slot="0"><NAT localhost-reachable="false" /></Adapter></Network></Hardware></Machine></VirtualBox>'


class IsolationSettingsTest(unittest.TestCase):
    def test_expected_settings_pass(self):
        self.assertEqual(setting_errors(INFO, DETAILS, XML, VM_ID), [])

    def test_missing_or_enabled_saved_nat_fails(self):
        for xml in (XML.replace(' localhost-reachable="false"', ''), XML.replace('"false"', '"true"')):
            with self.subTest(xml=xml):
                self.assertIn("saved NAT host-loopback restriction is not confirmed", setting_errors(INFO, DETAILS, xml, VM_ID))

    def test_effective_nat_fails_closed(self):
        for details in ("", "LocalhostReachable: 1"):
            self.assertIn("effective NAT host-loopback restriction is not confirmed", setting_errors(INFO, details, XML, VM_ID))

    def test_shared_folder_and_additional_adapter_fail(self):
        info = INFO.replace('nic2="none"', 'nic2="bridged"') + 'SharedFolderNameMachineMapping1="data"\n'
        errors = setting_errors(info, DETAILS, XML, VM_ID)
        self.assertTrue(any(error.startswith("nic2:") for error in errors))
        self.assertIn("VirtualBox reports a shared folder", errors)

    def test_public_ssh_or_extra_forward_fails(self):
        for info in (INFO.replace('127.0.0.1', '0.0.0.0'), INFO + 'Forwarding(1)="web,tcp,127.0.0.1,8080,,80"\n'):
            self.assertTrue(setting_errors(info, DETAILS, XML, VM_ID))

    def test_stopped_machine_fails(self):
        self.assertTrue(setting_errors(INFO.replace('VMState="running"', 'VMState="poweroff"'), DETAILS, XML, VM_ID))

    def test_wrong_vm_and_invalid_xml_fail(self):
        errors = setting_errors(INFO, DETAILS, "<broken", "different-id")
        self.assertTrue(any(error.startswith("UUID:") for error in errors))
        self.assertIn("saved VirtualBox configuration is invalid", errors)

    def test_main_rejects_actual_persistence_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            state = root / ".vagrant/machines/default/virtualbox/id"
            state.parent.mkdir(parents=True)
            state.write_text(VM_ID)
            config = root / "vm.vbox"
            config.write_text(XML.replace(' localhost-reachable="false"', ''))
            info = INFO + f'CfgFile="{config}"\n'
            replies = [SimpleNamespace(stdout=info), SimpleNamespace(stdout=DETAILS)]
            output = io.StringIO()
            with patch.object(verify, "__file__", str(root / "verify.py")), \
                    patch.object(verify.subprocess, "run", side_effect=replies), \
                    contextlib.redirect_stderr(output):
                self.assertEqual(verify.main(), 1)
            self.assertIn("saved NAT host-loopback restriction is not confirmed", output.getvalue())


if __name__ == "__main__":
    unittest.main()
