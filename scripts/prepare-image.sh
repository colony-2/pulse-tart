#!/bin/bash
# Run INSIDE a fresh macOS ARM64 builder VM. Never run on the physical Mac.
set -euo pipefail
set +x
umask 077
usage() {
  cat <<'HELP'
Usage: sudo /bin/bash prepare-image.sh [--shutdown] [--c2j latest|TAG|COMMIT] [EXTRACTED_BUNDLE_DIR]

Install the bundle's Nix/c2j/tool environment inside a fresh macOS ARM64 VM.
build-bundle.sh selects the latest c2j release by default. Use --c2j to override
its selection here, including --c2j latest to refresh an older bundle.
The directory defaults to this script's directory in the extracted guest bundle.
No worker service is started and no lease is accepted. Re-running on the same
unused builder is safe. --shutdown powers off after successful preparation.
Export/push the stopped VM separately using Tart or your image-building pipeline.
HELP
}
shutdown=0
bundle=
version=bundled
while [[ $# -gt 0 ]]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --shutdown) shutdown=1 ;;
    --c2j) [[ $# -ge 2 ]] || { usage >&2; exit 2; }; version=$2; shift ;;
    --*) echo "Unknown option: $1" >&2; exit 2 ;;
    *) if [[ -n $bundle ]]; then usage >&2; exit 2; fi; bundle=$1 ;;
  esac
  shift
done
bundle=${bundle:-$(cd "$(dirname "$0")" && pwd)}
if [[ $(uname -s) != Darwin || $(uname -m) != arm64 || $EUID != 0 ]]; then
  echo 'Run as root inside a fresh macOS ARM64 builder VM.' >&2
  exit 1
fi
for file in install-guest.sh select-c2j.sh flake.nix flake.lock profile; do
  [[ -f "$bundle/$file" ]] || { echo "Missing bundle file: $file" >&2; exit 1; }
done
profile=$(cat "$bundle/profile")
[[ $profile == default || $profile == integration || $profile == docker || $profile == docker-integration ]] || { echo 'Unknown guest bundle profile.' >&2; exit 1; }
# Refuse previously used workers. Never reset execution intent or delete leases.
for label in dev.pulse-tart.bootstrap dev.pulse-tart.guest; do
  if [[ -e /Library/LaunchDaemons/$label.plist ]] || launchctl print "system/$label" >/dev/null 2>&1; then
    echo 'This VM has runtime services. Start with a fresh image builder.' >&2
    exit 1
  fi
done
if /usr/bin/pgrep -u 55050 >/dev/null 2>&1; then
  echo 'Worker-user processes exist. Start with a fresh image builder.' >&2
  exit 1
fi
if [[ -d /var/db/pulse-tart ]]; then
  shopt -s nullglob dotglob
  for entry in /var/db/pulse-tart/*; do
    [[ $entry == /var/db/pulse-tart/nix-installer ]] && continue
    echo 'Runtime state exists. Start with a fresh image builder.' >&2
    exit 1
  done
  shopt -u nullglob dotglob
fi
if [[ $version != bundled ]]; then /bin/bash "$bundle/select-c2j.sh" "$bundle" "$version"; fi
/bin/bash "$bundle/install-guest.sh" "$bundle"
# Keep only executable setup entry points in the prepared image. c2j version
# reports the installed release; no version/fingerprint sidecar is needed.
mkdir -p /opt/pulse-tart-image
cp "$bundle/bootstrap-guest.sh" "$bundle/prepare-template.sh" /opt/pulse-tart-image/
chmod 755 /opt/pulse-tart-image /opt/pulse-tart-image/*.sh
rm -f /var/db/pulse-tart/nix-installer
rmdir /var/db/pulse-tart
sync
echo PULSE_IMAGE_PREPARED
/opt/pulse-tart/bin/c2j version
echo 'Power off cleanly, then export this VM as your base image.'
if [[ $shutdown == 1 ]]; then /sbin/shutdown -h now; fi
