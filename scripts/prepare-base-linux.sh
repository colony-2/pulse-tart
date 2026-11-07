#!/bin/bash
# Disposable ARM64 Ubuntu worker image builder. No leases or runtime credentials.
set -euo pipefail
set +x
[[ $(uname -s) == Linux && $(uname -m) == aarch64 && $EUID == 0 ]]
bundle=$1
expected=$3
. /etc/os-release
[[ $ID == ubuntu && $VERSION_ID == "$expected" ]]
if [[ ${PULSE_LINUX_BUILD_SERVICE:-0} != 1 ]]; then
 install -d -m 700 /var/lib/pulse-tart-build
 cp -a "$bundle"/. /var/lib/pulse-tart-build/
 if ! systemctl cat pulse-tart-build.service >/dev/null 2>&1; then
  cat > /etc/systemd/system/pulse-tart-build.service <<UNIT
[Unit]
After=network-online.target
Wants=network-online.target
[Service]
Type=oneshot
Environment=PULSE_LINUX_BUILD_SERVICE=1
ExecStart=/bin/bash /var/lib/pulse-tart-build/prepare-base-linux.sh /var/lib/pulse-tart-build $2 $3
TimeoutStartSec=$2
StandardOutput=append:/var/log/pulse-tart-build.log
StandardError=append:/var/log/pulse-tart-build.log
UNIT
  systemctl daemon-reload
  systemctl start --no-block pulse-tart-build.service
 fi
 for ((i=0;i<$2;i++)); do
  if [[ -f /var/lib/pulse-tart-build/ready ]]; then echo PULSE_BASE_READY; sync; shutdown -h now; exit 0; fi
  if systemctl is-failed --quiet pulse-tart-build; then echo PULSE_BASE_FAILED; tail -c 10000 /var/log/pulse-tart-build.log; exit 1; fi
  sleep 1
 done
 exit 1
fi
export DEBIAN_FRONTEND=noninteractive HOME=/root
apt-get update
apt-get install -y nix-bin nix-setup-systemd curl ca-certificates git xz-utils
systemctl enable --now nix-daemon.socket
export PATH=/nix/var/nix/profiles/default/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin
mkdir -p /etc/nix
printf 'experimental-features = nix-command flakes\nfilter-syscalls = false\n' > /etc/nix/nix.conf
systemctl restart nix-daemon
/bin/bash "$bundle/resolve-c2j-vendor.sh" "$bundle"
nix build --no-write-lock-file --out-link /opt/pulse-tart "$bundle#$(cat "$bundle/profile")"
id pulseworker >/dev/null 2>&1 || useradd --uid 55050 --user-group --create-home --shell /bin/bash pulseworker
[[ $(id -u pulseworker) == 55050 && $(id -g pulseworker) == 55050 ]]
install -d -m 700 -o pulseworker -g pulseworker /home/pulseworker /home/pulseworker/work /home/pulseworker/work/tmp
mkdir -p /opt/pulse-tart-image
cp "$bundle/bootstrap-guest.sh" /opt/pulse-tart-image/
/opt/pulse-tart/bin/c2j version
/opt/pulse-tart/bin/pulse-tart-guest version
rm -f /etc/systemd/system/pulse-tart-build.service
sync
touch /var/lib/pulse-tart-build/ready
