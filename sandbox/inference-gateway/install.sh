#!/usr/bin/env bash
set -euo pipefail
stage=/home/vagrant/artex-gateway-stage
sudo -n systemctl disable --now docker.socket docker.service
sudo -n install -d -m 0755 /etc/artex-gateway
sudo -n install -m 0644 "$stage/firewall.nft" "$stage/Caddyfile" /etc/artex-gateway/
sudo -n install -m 0755 "$stage/artex-caddy" /usr/local/bin/artex-caddy
sudo -n install -m 0644 "$stage/artex-firewall.service" "$stage/artex-proxy.service" /etc/systemd/system/
sudo -n install -m 0644 "$stage/10-artex-inference.network" /etc/systemd/network/
sudo -n install -m 0644 "$stage/00-artex-gateway.conf" /etc/ssh/sshd_config.d/
printf '%s\n' 'net.ipv4.ip_forward=0' 'net.ipv6.conf.all.forwarding=0' | sudo -n tee /etc/sysctl.d/90-artex-gateway.conf >/dev/null
sudo -n sysctl -w net.ipv4.ip_forward=0 net.ipv6.conf.all.forwarding=0 >/dev/null
sudo -n sed -i '/ # artex-inference$/d' /etc/hosts
cat "$stage/host-entry" | sudo -n tee -a /etc/hosts >/dev/null
sudo -n sshd -t
sudo -n systemctl reload ssh
sudo -n ip link set enp0s8 up
sudo -n ip addr replace 192.0.2.1/24 dev enp0s8
sudo -n networkctl reload
sudo -n systemctl daemon-reload
sudo -n systemctl enable artex-firewall.service artex-proxy.service
sudo -n systemctl restart artex-firewall.service
sudo -n systemctl restart artex-proxy.service
