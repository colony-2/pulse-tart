#!/bin/bash
# Allocation-only Ubuntu daemon. All mutable Docker data is on a bounded disk.
set -euo pipefail
set +x
umask 077
[[ $EUID == 0 && $(uname -m) == aarch64 ]]
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == 24.04 ]]
root=/var/lib/pulse-docker
version=$1
disk=$2
export DEBIAN_FRONTEND=noninteractive
if ! command -v docker >/dev/null || [[ $(docker --version) != *" $version,"* ]]; then
 apt-get update
 selected=$(apt-cache madison docker.io | awk -v v="$version" '$3 ~ "^([0-9]+:)?" v "([+~.-]|$)" {print $3;exit}')
 [[ -n $selected ]]
 apt-get install -y "docker.io=$selected" docker-buildx docker-compose-v2 openssh-server e2fsprogs
fi
# A failed preparation may retry on this same unused allocation. Never wipe it.
if [[ ! -f $root/disk-ready ]]; then
 systemctl stop docker.service docker.socket || true
 test ! -e /var/db/pulse-tart/intent
 # Source images may cache software, but runtime identities are allocation-local.
 rm -rf /var/lib/docker /var/lib/containerd
 install -d -m 700 /var/lib/docker
 if [[ ! -f $root/engine.ext4 ]]; then
  truncate -s "${disk}G" "$root/engine.ext4"
  mkfs.ext4 -q -F "$root/engine.ext4"
 fi
 mountpoint -q /var/lib/docker || mount -o loop "$root/engine.ext4" /var/lib/docker
 touch "$root/disk-ready"
fi
mountpoint -q /var/lib/docker
# Keep image/container writable state inside the bounded Docker filesystem.
install -d /etc/docker
printf '%s\n' '{"features":{"containerd-snapshotter":false},"data-root":"/var/lib/docker"}' > /etc/docker/daemon.json
systemctl enable --now docker
[[ $(docker version --format '{{.Server.Version}}') == "$version" ]]
[[ $(docker info --format '{{.DockerRootDir}}') == /var/lib/docker ]]
if [[ -f $root/authorized_keys ]]; then
 id pulseengine >/dev/null 2>&1 || useradd -m -s /bin/sh pulseengine
 usermod -aG docker pulseengine
 install -d -m 700 -o pulseengine -g pulseengine /home/pulseengine/.ssh
 install -m 600 -o pulseengine -g pulseengine "$root/authorized_keys" /home/pulseengine/.ssh/authorized_keys
 cat > "$root/sshd.conf" <<CONF
Port 2222
HostKey $root/host-key
PidFile $root/sshd.pid
AuthorizedKeysFile /home/pulseengine/.ssh/authorized_keys
AllowUsers pulseengine
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
UsePAM yes
AllowTcpForwarding local
AllowStreamLocalForwarding local
GatewayPorts no
X11Forwarding no
PermitTTY no
ForceCommand /bin/false
CONF
 cat > /etc/systemd/system/pulse-docker-ssh.service <<UNIT
[Unit]
After=network.target docker.service
[Service]
ExecStart=/usr/sbin/sshd -D -f $root/sshd.conf
Restart=always
RestartSec=1
[Install]
WantedBy=multi-user.target
UNIT
 /usr/sbin/sshd -t -f "$root/sshd.conf"
 systemctl daemon-reload
 systemctl enable --now pulse-docker-ssh
fi
if id pulseworker >/dev/null 2>&1; then usermod -aG docker pulseworker; fi
engine_id=$(docker info --format '{{.ID}}')
printf 'PULSE_DOCKER_ENGINE_READY %s\n' "$engine_id"
