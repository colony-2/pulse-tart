#!/usr/bin/env python3
"""Retry cleanup using a private journal; never select resources by name prefix."""

import argparse
import json
import os
import pathlib
import signal
import subprocess
import time
import urllib.error
import urllib.request

p = argparse.ArgumentParser(description=__doc__)
p.add_argument("state", help="private_state directory from report.json; keep private")
a = p.parse_args()
root = pathlib.Path(a.state).resolve()
if (
    not root.is_dir()
    or root.stat().st_uid != os.getuid()
    or root.stat().st_mode & 0o077
):
    p.error("expected a private, current-user-owned journal directory")
errors = []
for path in sorted(root.glob("*.json")):
    j = json.loads(path.read_text())
    if "spec" not in j or j.get("cleaned"):
        continue
    name = j["spec"]["name"]
    try:
        if j.get("url"):
            url = j["url"].rstrip("/") + "/vms/" + name

            def call(method, body=None):
                request = urllib.request.Request(
                    url,
                    method=method,
                    data=None if body is None else json.dumps(body).encode(),
                    headers={"Content-Type": "application/json"},
                )
                with urllib.request.urlopen(request, timeout=30) as r:
                    b = r.read()
                    return json.loads(b) if b else None

            try:
                v = call("GET")
            except urllib.error.HTTPError as e:
                if e.code != 404:
                    raise
                v = None
            if v is not None:
                assert j.get("uid") and v["uid"] == j["uid"], (
                    "ownership UID differs or missing"
                )
                assert v["image"] == j["spec"]["image"], "image differs"
                assert v["startup_script"].get("script_content") == j["spec"][
                    "startup_script"
                ].get("script_content"), "startup identity differs"
                if v.get("status") not in ("failed", "stopped"):
                    v["powerState"] = "stopped"
                    call("PUT", v)
                deadline = time.monotonic() + 90
                while True:
                    v = call("GET")
                    assert v["uid"] == j["uid"], "ownership changed while stopping"
                    if any(
                        c["type"] == "running" and c["state"] == "false"
                        for c in v.get("conditions", [])
                    ):
                        break
                    if time.monotonic() > deadline:
                        raise RuntimeError("stop timed out; preserving record")
                    time.sleep(2)
                call("DELETE")
        else:
            home = pathlib.Path(
                j.get(
                    "tart_home",
                    os.environ.get(
                        "PULSE_TEST_TART_HOME", str(pathlib.Path.home() / ".tart")
                    ),
                )
            )
            directory = home / "vms" / name
            if directory.exists():
                assert j.get("inode") == directory.stat().st_ino, (
                    "native directory identity differs"
                )
                env = dict(os.environ, TART_HOME=str(home), TART_NO_AUTO_PRUNE="true")
                v = json.loads(
                    subprocess.check_output(
                        ["tart", "get", name, "--format", "json"], env=env
                    )
                )
                if v["State"] == "running":
                    subprocess.run(
                        ["tart", "stop", name], env=env, check=True, timeout=90
                    )
                subprocess.run(
                    ["tart", "delete", name], env=env, check=True, timeout=90
                )
        j["cleaned"] = True
        path.write_text(json.dumps(j))
        print("Cleaned", name, flush=True)
    except Exception as e:
        errors.append(name)
        print("Preserved", name, ":", str(e), flush=True)
# Detached native worker helpers survive a test-controller crash. Only terminate
# the recorded session leader with this exact private ready-file identity.
if not errors:
    for path in root.glob('*-endpoint.json'):
        helper = json.loads(path.read_text())
        if helper.get('helper_kind') != 'pulse-test-worker-endpoint':
            continue
        try:
            pid = int(helper['pid'])
            if pid <= 1 or helper['ready'] != str(path):
                raise RuntimeError('invalid helper identity')
            ps = subprocess.run(['ps','-p',str(pid),'-o','command='],capture_output=True,text=True)
            if ps.returncode == 0 and ps.stdout.strip():
                expected = helper['script']+' --target '+helper['target']+' --ready '+str(path)
                if expected not in ps.stdout or os.getpgid(pid) != pid:
                    raise RuntimeError('helper process identity changed')
                os.kill(pid, signal.SIGTERM)
            path.unlink()
        except ProcessLookupError:
            path.unlink(missing_ok=True)
        except Exception as e:
            errors.append(str(path))
            print('Preserved helper:', str(e), flush=True)
if errors:
    raise SystemExit(1)
print("Owned VMs removed. Private journal/workspace retained for inspection at", root)
