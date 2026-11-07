#!/bin/bash
# Start the installed launcher. Job startup never installs or resolves tools.
set -euo pipefail
set +x
umask 077
[[ $(uname -s) == Darwin && $(uname -m) == arm64 && $EUID == 0 ]]
mkdir -p /var/db/pulse-tart
chmod 700 /var/db/pulse-tart
[[ ! -f /var/db/pulse-tart/intent ]] || exit 0
[[ -x /opt/pulse-tart/bin/pulse-tart-guest && -x /opt/pulse-tart/bin/c2j ]]
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
echo PULSE_BOOTSTRAP_OK
