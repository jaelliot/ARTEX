import copy
import hashlib
import json
import unittest
from pathlib import Path

from verify import dhcp_errors, policy_errors, prestart_app_errors, prestart_gateway_errors, setting_errors

VM_ID = "12345678-1234-4234-8234-123456789abc"
COMMON = '''VMState="running"
clipboard="disabled"
clipboard_file_transfers="off"
draganddrop="disabled"
vrde="off"
usb="off"
ehci="off"
xhci="off"
''' + ''.join(f'uart{i}="off"\n' for i in range(1, 5)) + ''.join(f'lpt{i}="off"\n' for i in range(1, 3))
APP = f'UUID="{VM_ID}"\nname="artex-sandbox"\nnic1="intnet"\nintnet1="artex-inference"\ncableconnected1="on"\n' + ''.join(f'nic{i}="none"\n' for i in range(2, 9)) + COMMON
GATEWAY = f'UUID="{VM_ID}"\nname="artex-inference-gateway"\nnic1="nat"\ncableconnected1="on"\nnic2="intnet"\nintnet2="artex-inference"\ncableconnected2="on"\nForwarding(0)="ssh,tcp,127.0.0.1,2223,,22"\n' + ''.join(f'nic{i}="none"\n' for i in range(3, 9)) + COMMON
DHCP = '''NetworkName:    artex-inference
Dhcpd IP:       192.0.2.10
LowerIPAddress: 192.0.2.2
UpperIPAddress: 192.0.2.2
NetworkMask:    255.255.255.0
Enabled:        Yes
    Forced options:   None
    Suppressed opts.: None
        1/legacy: 255.255.255.0
Groups:               None
Individual Configs:   None
'''


class IsolationSettingsTest(unittest.TestCase):
    def test_prestart_rejects_saved_or_unsafe_running_machine(self):
        self.assertEqual(prestart_app_errors(APP, VM_ID), [])
        self.assertEqual(prestart_app_errors(APP.replace('VMState="running"', 'VMState="poweroff"'), VM_ID), [])
        for state in ('saved', 'paused', 'aborted'):
            self.assertTrue(prestart_app_errors(APP.replace('VMState="running"', f'VMState="{state}"'), VM_ID))
        self.assertTrue(prestart_app_errors(APP.replace('nic2="none"', 'nic2="nat"'), VM_ID))
        self.assertTrue(prestart_app_errors(APP, 'different-id'))
        self.assertEqual(prestart_gateway_errors(GATEWAY, VM_ID), [])
        self.assertEqual(prestart_gateway_errors(GATEWAY.replace('cableconnected2="on"', 'cableconnected2="off"'), VM_ID), [])
        self.assertEqual(prestart_gateway_errors(GATEWAY.replace('VMState="running"', 'VMState="poweroff"'), VM_ID), [])
        for state in ('saved', 'paused', 'aborted'):
            self.assertTrue(prestart_gateway_errors(GATEWAY.replace('VMState="running"', f'VMState="{state}"'), VM_ID))
        self.assertTrue(prestart_gateway_errors(GATEWAY.replace('nic3="none"', 'nic3="bridged"'), VM_ID))

    def test_expected_vms_pass(self):
        self.assertEqual(setting_errors(APP, VM_ID), [])
        self.assertEqual(setting_errors(GATEWAY, VM_ID, gateway=True), [])
        self.assertEqual(setting_errors(GATEWAY.replace('cableconnected2="on"', 'cableconnected2="off"'), VM_ID, gateway=True, cable=False), [])

    def test_extra_adapters_or_host_access_fail(self):
        for slot in range(2, 9):
            self.assertTrue(setting_errors(APP.replace(f'nic{slot}="none"', f'nic{slot}="nat"'), VM_ID))
        for field in ("usb", "ehci", "xhci", "uart1", "lpt1"):
            self.assertTrue(setting_errors(APP.replace(f'{field}="off"', f'{field}="on"'), VM_ID))
        self.assertTrue(setting_errors(APP + 'SharedFolderNameMachineMapping1="host"\n', VM_ID))
        self.assertTrue(setting_errors(APP + 'AttachedHostPCI=00:01.0,00:02.0\n', VM_ID))
        self.assertTrue(setting_errors(GATEWAY.replace('127.0.0.1', '0.0.0.0'), VM_ID, gateway=True))
        self.assertTrue(setting_errors(APP, "different-id"))

    def test_dhcp_must_not_advertise_router_or_dns(self):
        self.assertEqual(dhcp_errors(DHCP), [])
        self.assertTrue(dhcp_errors(DHCP + '        3/legacy: 192.0.2.1\n'))
        self.assertTrue(dhcp_errors(DHCP.replace('UpperIPAddress: 192.0.2.2', 'UpperIPAddress: 192.0.2.254')))

    def test_live_policy_must_match_approved_rules_and_services(self):
        root = Path(__file__).resolve().parent / "inference-gateway"
        expected = json.loads((root / 'firewall.expected.json').read_text())
        config = {'ip': '100.93.43.40', 'caddy_sha256': 'a' * 64}
        firewall, caddy = b'firewall', b'caddy'
        snapshot = {
            'nft': json.loads(json.dumps(expected).replace('@UPSTREAM_IP@', config['ip'])),
            'services': {'firewall': 'active', 'proxy': 'active'}, 'ipv4_forward': '0', 'ipv6_forward': '0',
            'firewall_sha256': hashlib.sha256(firewall).hexdigest(), 'caddyfile_sha256': hashlib.sha256(caddy).hexdigest(),
            'caddy_sha256': config['caddy_sha256'], 'addresses': [config['ip']], 'listener': True,
            'sshd': ['passwordauthentication no', 'kbdinteractiveauthentication no', 'allowtcpforwarding local', 'permitopen 192.0.2.2:22', 'gatewayports no', 'allowusers vagrant'],
        }
        self.assertEqual(policy_errors(snapshot, config, expected, firewall, caddy), [])
        for change in ('forward', 'rules', 'service', 'listener', 'ssh', 'address', 'binary'):
            bad = copy.deepcopy(snapshot)
            if change == 'forward': bad['ipv4_forward'] = '1'
            if change == 'rules': bad['nft']['nftables'].append({'rule': {'expr': [{'accept': None}]}})
            if change == 'service': bad['services']['firewall'] = 'inactive'
            if change == 'listener': bad['listener'] = False
            if change == 'ssh': bad['sshd'].remove('passwordauthentication no')
            if change == 'address': bad['addresses'] = ['10.0.0.2']
            if change == 'binary': bad['caddy_sha256'] = 'b' * 64
            with self.subTest(change=change):
                self.assertTrue(policy_errors(bad, config, expected, firewall, caddy))


if __name__ == '__main__':
    unittest.main()
