#!/bin/bash
# Configure an unused warm primary; never accept a lease here.
set -euo pipefail
set +x
umask 077
root=/var/lib/pulse-docker
[[ $EUID == 0 ]]
/opt/pulse-tart/bin/c2j version >/dev/null
install -d -m 755 /usr/local/lib/docker /usr/local/lib/docker/cli-plugins
for plugin in buildx compose; do ln -sf "/opt/pulse-tart/bin/docker-$plugin" "/usr/local/lib/docker/cli-plugins/docker-$plugin"; done
/opt/pulse-tart/bin/docker --version >/dev/null
/opt/pulse-tart/bin/docker buildx version >/dev/null
/opt/pulse-tart/bin/docker compose version >/dev/null
if [[ $(uname -s) == Darwin ]]; then
 install -d -m 700 -o pulseworker -g staff /Users/pulseworker/work /Users/pulseworker/work/tmp
 # A separate service user keeps relay lifetime out of worker-process recovery.
 if ! id pulsedocker >/dev/null 2>&1; then
  ! dscl . -list /Users UniqueID | awk '$2 == 55051 {found=1} END {exit !found}'
  dscl . -create /Users/pulsedocker
  dscl . -create /Users/pulsedocker UniqueID 55051
  dscl . -create /Users/pulsedocker PrimaryGroupID 20
  dscl . -create /Users/pulsedocker NFSHomeDirectory /var/empty
  dscl . -create /Users/pulsedocker UserShell /usr/bin/false
  dscl . -create /Users/pulsedocker Password '*'
  dscl . -create /Users/pulsedocker IsHidden 1
 fi
 [[ $(id -u pulsedocker) == 55051 ]]
 chown -R pulsedocker:staff "$root"
 chmod 750 "$root"
 # launchd opens these paths as the service user.
 touch /var/log/pulse-tart-docker.log
 chown pulsedocker:staff /var/log/pulse-tart-docker.log
 chmod 600 /var/log/pulse-tart-docker.log
 cat > /Library/LaunchDaemons/dev.pulse-tart.docker.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>dev.pulse-tart.docker</string>
<key>UserName</key><string>pulsedocker</string>
<key>ProgramArguments</key><array><string>/opt/pulse-tart/bin/pulse-tart-guest</string><string>docker-relay</string><string>$root/relay.json</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>/var/log/pulse-tart-docker.log</string><key>StandardErrorPath</key><string>/var/log/pulse-tart-docker.log</string></dict></plist>
PLIST
 chmod 600 /Library/LaunchDaemons/dev.pulse-tart.docker.plist
 launchctl print system/dev.pulse-tart.docker >/dev/null 2>&1 || launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.docker.plist
 ln -sf "$root/docker.sock" /var/run/docker.sock
else
 install -d -m 700 -o pulseworker -g pulseworker /home/pulseworker/work /home/pulseworker/work/tmp
fi
