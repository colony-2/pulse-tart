#!/bin/bash
# Run INSIDE a fresh macOS ARM64 builder VM. Never run on the physical Mac.
set -euo pipefail
set +x
umask 077
usage() {
  cat <<'HELP'
Usage: sudo /bin/bash prepare-image.sh [--shutdown] [EXTRACTED_BUNDLE_DIR]

Install the pinned Nix/c2j/tool environment inside a fresh macOS ARM64 VM.
The directory defaults to this script's directory in the extracted guest bundle.
No worker service is started and no lease is accepted. Re-running on the same
unused builder is safe. --shutdown powers off after successful preparation.
Export/push the stopped VM separately using Tart or your image-building pipeline.
HELP
}
shutdown=0
bundle=
for arg in "$@"; do
  case "$arg" in
    --help|-h) usage; exit 0 ;;
    --shutdown) shutdown=1 ;;
    --*) echo "Unknown option: $arg" >&2; exit 2 ;;
    *) if [[ -n $bundle ]]; then usage >&2; exit 2; fi; bundle=$arg ;;
  esac
done
bundle=${bundle:-$(cd "$(dirname "$0")" && pwd)}
if [[ $(uname -s) != Darwin || $(uname -m) != arm64 || $EUID != 0 ]]; then
  echo 'Run as root inside a fresh macOS ARM64 builder VM.' >&2
  exit 1
fi
for file in install-guest.sh flake.nix flake.lock profile environment.sha256; do
  [[ -f "$bundle/$file" ]] || { echo "Missing bundle file: $file" >&2; exit 1; }
done
profile=$(cat "$bundle/profile")
[[ $profile == default || $profile == integration ]] || { echo 'Unknown guest bundle profile.' >&2; exit 1; }
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
/bin/bash "$bundle/install-guest.sh" "$bundle"
# Record the environment identity separately from per-launch state. The profile
# symlink remains a Nix GC root; keep the store, daemon and worker account.
manifest=/opt/pulse-tart-image
mkdir -p "$manifest"
chmod 755 "$manifest"
readlink /opt/pulse-tart > "$manifest/store-path.tmp"
cp "$bundle/environment.sha256" "$manifest/environment.sha256.tmp"
chmod 644 "$manifest/"*.tmp
mv "$manifest/store-path.tmp" "$manifest/store-path"
mv "$manifest/environment.sha256.tmp" "$manifest/environment.sha256"
rm -f /var/db/pulse-tart/nix-installer
rmdir /var/db/pulse-tart
sync
echo PULSE_IMAGE_PREPARED
echo "Environment: $(cat "$manifest/environment.sha256")"
echo 'Power off cleanly, then export this VM as your base image.'
if [[ $shutdown == 1 ]]; then /sbin/shutdown -h now; fi
