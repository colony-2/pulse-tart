#!/bin/bash
# Run in a disposable image builder. The provider/updater waits for clean shutdown.
set -euo pipefail
set +x
umask 077
bundle=$1
seconds=$2
expected_os=$3
mode=${4:-install}
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID == 0 ]]
[[ $seconds =~ ^[0-9]+$ && $seconds -ge 60 && $seconds -le 7200 ]]
[[ $expected_os =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]
[[ $(/usr/bin/sw_vers -productVersion) == "$expected_os" ]]
state=/var/db/pulse-tart-base
mkdir -p "$state"
if [[ ${PULSE_BASE_SERVICE:-0} != 1 ]]; then
  [[ ! -e $state/failed ]] || { echo PULSE_BASE_FAILED; exit 1; }
  if [[ ! -f $state/ready ]]; then
    if [[ $mode == install && ! -d $state/setup ]]; then cp -R "$bundle" "$state/setup"; fi
    cp "$0" "$state/prepare-base.sh"
    cat > /Library/LaunchDaemons/dev.pulse-tart.base.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>Label</key><string>dev.pulse-tart.base</string>
<key>ProgramArguments</key><array><string>/bin/bash</string><string>$state/prepare-base.sh</string><string>$state/setup</string><string>$seconds</string><string>$expected_os</string><string>$mode</string></array>
<key>EnvironmentVariables</key><dict><key>PULSE_BASE_SERVICE</key><string>1</string><key>HOME</key><string>/var/root</string></dict>
<key>RunAtLoad</key><true/><key>StandardOutPath</key><string>/var/log/pulse-tart-base.log</string><key>StandardErrorPath</key><string>/var/log/pulse-tart-base.log</string></dict></plist>
PLIST
    chmod 600 /Library/LaunchDaemons/dev.pulse-tart.base.plist
    launchctl print system/dev.pulse-tart.base >/dev/null 2>&1 || launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.base.plist
  fi
  for ((i=0;i<seconds;i++)); do
    if [[ -f $state/ready ]]; then
      echo PULSE_BASE_READY
      sync
      /sbin/shutdown -h now
      exit 0
    fi
    if [[ -f $state/failed ]]; then echo PULSE_BASE_FAILED; tail -c 6000 /var/log/pulse-tart-base.log; exit 1; fi
    sleep 1
  done
  echo PULSE_BASE_FAILED
  /sbin/shutdown -h now
  exit 1
fi
(sleep "$seconds"; touch "$state/failed"; /sbin/shutdown -h now) &
guard=$!
trap 'kill "$guard" 2>/dev/null || true' EXIT
trap 'touch "$state/failed"; sleep 5; /sbin/shutdown -h now' ERR
[[ $(/usr/bin/sw_vers -productVersion) == "$expected_os" ]]
if [[ $mode == install ]]; then
  /bin/bash "$bundle/prepare-image.sh" "$bundle"
  rm -f /Library/LaunchDaemons/dev.pulse-tart.template.plist
  rm -rf /var/db/pulse-tart-template
fi
for tool in c2j pulse-tart-guest git node pnpm uv python3; do test -x "/opt/pulse-tart/bin/$tool"; done
test -f /opt/pulse-tart-image/bootstrap-guest.sh
test -f /opt/pulse-tart-image/prepare-template.sh
/opt/pulse-tart/bin/c2j version
rm -f /Library/LaunchDaemons/dev.pulse-tart.base.plist
rm -rf "$state/setup"
sync
kill "$guard" 2>/dev/null || true
wait "$guard" 2>/dev/null || true
trap - EXIT
printf 'ready\n' > "$state/ready"
