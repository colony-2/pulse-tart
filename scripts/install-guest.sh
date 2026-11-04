#!/bin/bash
# Shared installation path for disposable workers and prepared base images.
set -euo pipefail
set +x
umask 077
bundle=$1
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID == 0 ]]
[[ -f "$bundle/environment.sha256" ]]
mkdir -p /var/db/pulse-tart
chmod 700 /var/db/pulse-tart
manifest=/opt/pulse-tart-image
# A matching prepared image avoids flake evaluation and network access.
if [[ -f "$manifest/environment.sha256" && -f "$manifest/store-path" ]] &&
   cmp -s "$bundle/environment.sha256" "$manifest/environment.sha256" &&
   [[ -L /opt/pulse-tart && $(readlink /opt/pulse-tart) == "$(cat "$manifest/store-path")" ]] &&
   [[ $(/nix/var/nix/profiles/default/bin/nix --version) == 'nix (Nix) 2.35.4' ]] &&
   [[ $(id -u pulseworker) == 55050 ]]; then
  valid=1
  for tool in c2j pulse-tart-guest git node pnpm uv python3; do
    [[ -x /opt/pulse-tart/bin/$tool ]] || valid=0
  done
  if [[ $valid == 1 ]]; then
    echo PULSE_IMAGE_CACHE_HIT
    exit 0
  fi
fi
export HOME=/var/root
export PATH=/nix/var/nix/profiles/default/bin:/usr/bin:/bin:/usr/sbin:/sbin
if ! command -v nix >/dev/null; then
  # Fixed installer version and digest; upstream Nix selection is embedded in this release.
  installer=/var/db/pulse-tart/nix-installer
  curl --fail --location --retry 3 --max-time 300 'https://github.com/DeterminateSystems/nix-installer/releases/download/v3.24.1/nix-installer-aarch64-darwin' -o "$installer"
  echo '34199f983d1a08e76a4fb33b28691faa637fc4a8677b4de20bd951126e1bdada  '"$installer" | shasum -a 256 -c -
  chmod 700 "$installer"
  "$installer" install --no-confirm --prefer-upstream-nix --diagnostic-endpoint ''
fi
nix --version
[[ $(nix --version) == "nix (Nix) 2.35.4" ]]
export NIX_SSL_CERT_FILE=/etc/ssl/cert.pem
export SSL_CERT_FILE=/etc/ssl/cert.pem
target=$(cat "$bundle/profile")
[[ $target == default || $target == integration ]]
nix --extra-experimental-features 'nix-command flakes' build --no-write-lock-file --out-link /opt/pulse-tart "$bundle#$target"
/opt/pulse-tart/bin/c2j version
/opt/pulse-tart/bin/pulse-tart-guest version
/opt/pulse-tart/bin/python3 --version
/opt/pulse-tart/bin/node --version
/opt/pulse-tart/bin/git --version
if ! dscl . -read /Users/pulseworker >/dev/null 2>&1; then
  if dscl . -list /Users UniqueID | awk '$2 == 55050 { found=1 } END { exit !found }'; then exit 1; fi
  dscl . -create /Users/pulseworker
  dscl . -create /Users/pulseworker UniqueID 55050
  dscl . -create /Users/pulseworker PrimaryGroupID 20
  dscl . -create /Users/pulseworker UserShell /bin/bash
  dscl . -create /Users/pulseworker NFSHomeDirectory /Users/pulseworker
  dscl . -create /Users/pulseworker Password '*'
  dscl . -create /Users/pulseworker IsHidden 1
fi
[[ $(id -u pulseworker) == 55050 ]]
mkdir -p /Users/pulseworker/.cache
chown -R 55050:20 /Users/pulseworker
chmod 700 /Users/pulseworker
