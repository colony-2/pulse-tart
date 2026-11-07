#!/bin/bash
# Run inside a fresh builder guest; never accept a launch or a lease here.
set -euo pipefail
set +x
umask 077
seconds=$1
expected_os=$2
capabilities=${3:-}
script=/opt/pulse-tart-image/prepare-template.sh
[[ $expected_os =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]]
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID == 0 ]]
[[ $seconds =~ ^[0-9]+$ && $seconds -ge 60 && $seconds -le 7200 ]]
state=/var/db/pulse-tart-template
mkdir -p "$state"
chmod 700 "$state"
if [[ ${PULSE_TEMPLATE_SERVICE:-0} != 1 ]]; then
  cat > /Library/LaunchDaemons/dev.pulse-tart.template.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>Label</key><string>dev.pulse-tart.template</string>
<key>ProgramArguments</key><array><string>/bin/bash</string><string>$script</string><string>$seconds</string><string>$expected_os</string><string>$capabilities</string></array>
<key>EnvironmentVariables</key><dict><key>PULSE_TEMPLATE_SERVICE</key><string>1</string><key>HOME</key><string>/var/root</string></dict>
<key>RunAtLoad</key><true/><key>StandardOutPath</key><string>/var/log/pulse-tart-template.log</string><key>StandardErrorPath</key><string>/var/log/pulse-tart-template.log</string>
</dict></plist>
PLIST
  chmod 600 /Library/LaunchDaemons/dev.pulse-tart.template.plist
  launchctl print system/dev.pulse-tart.template >/dev/null 2>&1 || launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.template.plist
  for ((i=0; i<seconds; i++)); do
    if [[ -f $state/ready ]]; then
      printf 'PULSE_TEMPLATE_READY '
      cat "$state/ready"
      exit 0
    fi
    if [[ -f $state/failed ]]; then echo PULSE_TEMPLATE_FAILED; tail -c 6000 /var/log/pulse-tart-template.log; exit 1; fi
    sleep 1
  done
  echo PULSE_TEMPLATE_FAILED
  /sbin/shutdown -h now
  exit 1
fi
# A restored clone must never repeat template preparation.
[[ ! -f $state/ready ]] || exit 0
(sleep "$seconds"; touch "$state/failed"; /sbin/shutdown -h now) &
guard=$!
trap 'kill "$guard" 2>/dev/null || true' EXIT
trap 'touch "$state/failed"; sleep 10; /sbin/shutdown -h now' ERR
[[ $(/usr/bin/sw_vers -productVersion) == "$expected_os" ]]
/opt/pulse-tart/bin/c2j version
/opt/pulse-tart/bin/pulse-tart-guest version
for capability in $capabilities; do
  case "$capability" in
    nix) /nix/var/nix/profiles/default/bin/nix --version >/dev/null ;;
    git|node|pnpm|uv) "/opt/pulse-tart/bin/$capability" --version >/dev/null ;;
    python) /opt/pulse-tart/bin/python3 --version >/dev/null ;;
    *) exit 1 ;;
  esac
done
test ! -e /var/db/pulse-tart/intent
test ! -e /Library/LaunchDaemons/dev.pulse-tart.guest.plist
/nix/var/nix/profiles/default/bin/nix --extra-experimental-features nix-command store ping --store daemon
# All prep timers must be gone BEFORE the provider can snapshot this guest.
kill "$guard" 2>/dev/null || true
wait "$guard" 2>/dev/null || true
trap - EXIT
/opt/pulse-tart/bin/python3 - <<'PY' > "$state/ready.tmp"
import json,re,subprocess
boot=subprocess.check_output(['/usr/sbin/sysctl','-n','kern.bootsessionuuid'],text=True).strip()
network=subprocess.check_output(['/sbin/ifconfig','en0'],text=True)
mac=re.search(r'\bether\s+([0-9a-f:]{17})',network).group(1)
print(json.dumps({'boot_id':boot,'mac':mac}))
PY
sync
mv "$state/ready.tmp" "$state/ready"
