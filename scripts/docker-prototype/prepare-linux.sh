#!/bin/bash
# Disposable Ubuntu guest only. Never run this on the physical host.
set -euo pipefail
[[ $(uname -s) == Linux && $(uname -m) == aarch64 && $EUID == 0 ]]
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y docker.io docker-buildx docker-compose-v2 git curl python3 python3-venv ca-certificates openssh-server
systemctl enable --now docker
id pulseworker >/dev/null 2>&1 || useradd --uid 55050 --create-home --shell /bin/bash pulseworker
usermod -aG docker pulseworker
install -d -o pulseworker -g pulseworker -m 0700 /Users/pulseworker/work /Users/pulseworker/work/tmp
# The daemon uses its private VM disk and exposes no unauthenticated TCP port.
test "$(docker info --format '{{.DockerRootDir}}')" = /var/lib/docker
sudo -u pulseworker -H docker version
sudo -u pulseworker -H docker buildx version
sudo -u pulseworker -H docker compose version
uname -a
cat /etc/os-release
printf 'PULSE_DOCKER_PREPARED\n'
