#!/bin/bash
# Run inside a disposable macOS primary after uploading the native tools.
set -euo pipefail
[[ $(uname -s)/$(uname -m) == Darwin/arm64 ]]
[[ $EUID == 0 ]]
if ! dscl . -read /Groups/pulseworker >/dev/null 2>&1; then
 dscl . -create /Groups/pulseworker
 dscl . -create /Groups/pulseworker PrimaryGroupID 55050
fi
if ! id pulseworker >/dev/null 2>&1; then
 dscl . -create /Users/pulseworker
 dscl . -create /Users/pulseworker UniqueID 55050
 dscl . -create /Users/pulseworker PrimaryGroupID 55050
 dscl . -create /Users/pulseworker UserShell /bin/bash
 dscl . -create /Users/pulseworker NFSHomeDirectory /Users/pulseworker
 dscl . -create /Users/pulseworker IsHidden 1
fi
mkdir -p /Users/pulseworker/.docker/cli-plugins /var/lib/pulse-docker /usr/local/bin
chown -R pulseworker:pulseworker /Users/pulseworker
chmod 700 /Users/pulseworker /var/lib/pulse-docker
chmod 755 /opt/pulse-docker
chmod 755 /opt/pulse-docker/c2j /opt/pulse-docker/jobdb-fixture /opt/pulse-docker/engine-contract.sh /opt/pulse-docker/docker /opt/pulse-docker/docker-buildx /opt/pulse-docker/docker-compose
ln -sf /opt/pulse-docker/docker /usr/local/bin/docker
ln -sf /opt/pulse-docker/docker-buildx /Users/pulseworker/.docker/cli-plugins/docker-buildx
ln -sf /opt/pulse-docker/docker-compose /Users/pulseworker/.docker/cli-plugins/docker-compose
/usr/bin/git --version
sudo -u pulseworker env HOME=/Users/pulseworker /usr/local/bin/docker --version
sudo -u pulseworker env HOME=/Users/pulseworker /usr/local/bin/docker buildx version
sudo -u pulseworker env HOME=/Users/pulseworker /usr/local/bin/docker compose version
