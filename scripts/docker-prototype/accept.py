#!/usr/bin/python3
"""One disposable VM, one immutable execution intent. Payload arrives on stdin."""

import json, os, subprocess, sys, platform, plistlib

root = "/var/lib/pulse-docker"
os.makedirs(root, mode=0o700, exist_ok=True)
payload = json.load(sys.stdin)
# Even an interrupted handoff is never executed again by a delivery retry.
fd = os.open(root + "/intent", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump({"id": payload["id"], "hash": payload["hash"]}, f)
    f.flush()
    os.fsync(f.fileno())
fd = os.open(root + "/delivery", os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(payload, f)
    f.flush()
    os.fsync(f.fileno())
directory = os.open(root, os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
if platform.system() == "Darwin":
    service = {
        "Label": "dev.pulse.docker-job",
        "ProgramArguments": ["/usr/bin/python3", "/opt/pulse-docker/run-job.py"],
        "RunAtLoad": True,
        "StandardOutPath": root + "/runner.log",
        "StandardErrorPath": root + "/runner.log",
    }
    path = root + "/job.plist"
    with open(path, "wb") as f:
        plistlib.dump(service, f)
    os.chmod(path, 0o600)
    command = ["/bin/launchctl", "bootstrap", "system", path]
else:
    command = [
        "systemd-run",
        "--unit=pulse-docker-job",
        "/usr/bin/python3",
        "/opt/pulse-docker/run-job.py",
    ]
subprocess.run(
    command, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
)
print("PULSE_DOCKER_ACCEPTED")
