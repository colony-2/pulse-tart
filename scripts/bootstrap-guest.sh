#!/bin/bash
# Called by guest delivery as root. No lease or application environment is accepted here.
set -euo pipefail
set +x
umask 077
bundle=$1
seconds=$2
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID == 0 ]]
[[ $seconds =~ ^[0-9]+$ && $seconds -ge 60 && $seconds -le 7200 ]]
mkdir -p /var/db/pulse-tart
chmod 700 /var/db/pulse-tart
# An ingest retry must not replace a running environment or reset execution intent.
if [[ -f /var/db/pulse-tart/intent ]]; then exit 0; fi
# A transient SSH connection is not the owner of bootstrap. launchd is.
if [[ ${PULSE_BOOTSTRAP_SERVICE:-0} != 1 ]]; then
  if [[ ! -d /var/db/pulse-tart/bootstrap ]]; then
    cp -R "$bundle" /var/db/pulse-tart/bootstrap
  fi
  cat > /Library/LaunchDaemons/dev.pulse-tart.bootstrap.plist <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.pulse-tart.bootstrap</string>
<key>ProgramArguments</key><array><string>/bin/bash</string><string>/var/db/pulse-tart/bootstrap/bootstrap-guest.sh</string><string>/var/db/pulse-tart/bootstrap</string><string>$seconds</string></array>
<key>EnvironmentVariables</key><dict><key>PULSE_BOOTSTRAP_SERVICE</key><string>1</string><key>HOME</key><string>/var/root</string></dict>
<key>RunAtLoad</key><true/><key>StandardOutPath</key><string>/var/log/pulse-tart-bootstrap.log</string><key>StandardErrorPath</key><string>/var/log/pulse-tart-bootstrap.log</string>
</dict></plist>
PLIST
  chmod 600 /Library/LaunchDaemons/dev.pulse-tart.bootstrap.plist
  launchctl print system/dev.pulse-tart.bootstrap >/dev/null 2>&1 || launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.bootstrap.plist
  for ((i=0; i<seconds; i++)); do
    if [[ -f /var/db/pulse-tart/ready ]]; then echo PULSE_BOOTSTRAP_OK; exit 0; fi
    if [[ -f /var/db/pulse-tart/bootstrap-failed ]]; then echo PULSE_BOOTSTRAP_FAILED; tail -c 6000 /var/log/pulse-tart-bootstrap.log; exit 1; fi
    sleep 1
  done
  echo PULSE_BOOTSTRAP_TIMEOUT
  /sbin/shutdown -h now
  exit 1
fi
# Bound only setup, never a c2j process. This child is owned by the launchd job.
(sleep "$seconds"; touch /var/db/pulse-tart/bootstrap-failed; /sbin/shutdown -h now) &
guard=$!
fail() { touch /var/db/pulse-tart/bootstrap-failed; echo PULSE_BOOTSTRAP_FAILED; sleep 10; /sbin/shutdown -h now; }
trap fail ERR
trap 'kill "$guard" 2>/dev/null || true' EXIT
/bin/bash "$bundle/install-guest.sh" "$bundle"
cat > /Library/LaunchDaemons/dev.pulse-tart.guest.plist <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>dev.pulse-tart.guest</string>
<key>ProgramArguments</key><array><string>/opt/pulse-tart/bin/pulse-tart-guest</string><string>serve</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/>
<key>StandardOutPath</key><string>/var/log/pulse-tart-guest.log</string><key>StandardErrorPath</key><string>/var/log/pulse-tart-guest.log</string>
</dict></plist>
PLIST
chmod 600 /Library/LaunchDaemons/dev.pulse-tart.guest.plist
launchctl print system/dev.pulse-tart.guest >/dev/null 2>&1 || launchctl bootstrap system /Library/LaunchDaemons/dev.pulse-tart.guest.plist
printf 'ready\n' > /var/db/pulse-tart/ready.tmp
mv /var/db/pulse-tart/ready.tmp /var/db/pulse-tart/ready
echo PULSE_BOOTSTRAP_OK
