#!/usr/bin/python3
import base64, json, os, pwd, subprocess

root = "/var/lib/pulse-docker"
with open(root + "/delivery") as f:
    payload = json.load(f)
os.unlink(root + "/delivery")
worker = pwd.getpwnam("pulseworker")
workspace = payload.get("workspace", "/Users/pulseworker/work")


def user():
    os.initgroups(worker.pw_name, worker.pw_gid)
    os.setgid(worker.pw_gid)
    os.setuid(worker.pw_uid)


env = dict(
    os.environ,
    HOME=worker.pw_dir,
    TMPDIR=workspace + "/tmp",
    TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE="/var/run/docker.sock",
    DOCKER_HOST="unix:///var/run/docker.sock",
    PATH="/opt/pulse-docker:/usr/local/bin:/usr/bin:/bin",
)
with open(root + "/worker.log", "wb") as log:
    try:
        result = subprocess.run(
            payload["args"],
            input=base64.b64decode(payload["lease"]),
            stdout=log,
            stderr=log,
            env=env,
            cwd=workspace,
            preexec_fn=user,
        )
        code = result.returncode
    except Exception:
        code = 125
with open(root + "/exit.tmp", "w") as f:
    f.write(str(code) + "\n")
    f.flush()
    os.fsync(f.fileno())
os.rename(root + "/exit.tmp", root + "/exit")
