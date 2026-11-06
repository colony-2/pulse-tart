#!/usr/bin/env python3
"""Real-machine experiment driver. Never turns unrun coverage into success."""

import argparse, base64, concurrent.futures, hashlib, json, os, pathlib, platform, shlex, subprocess, sys, tempfile, time, urllib.error, urllib.request, uuid

ROOT = pathlib.Path(__file__).resolve().parents[2]
LINUX = "ghcr.io/cirruslabs/ubuntu@sha256:439ba447488fa19f9b1ed091602546b8cb32442abdb60bac4943a4d6c3dd2ffa"
p = argparse.ArgumentParser()
p.add_argument("backend", choices=["tart", "orchard"])
p.add_argument("--tools", required=True)
p.add_argument("--output", required=True)
p.add_argument("--display-output", help="Output directory as displayed to the user")
p.add_argument("--rounds", type=int, default=2)
p.add_argument("--adopt-engine")
p.add_argument("--relay-only", action="store_true")
p.add_argument("--no-bind", action="store_true", help="current Docker contract without shared filesystems")
p.add_argument("--no-bind-extras-only", action="store_true", help="explicitly skip already-qualified c2j job for SDK/registry iteration")
p.add_argument("--no-bind-platform", choices=["both", "linux", "macos"], default="both")
p.add_argument(
    "--filesystem-only",
    action="store_true",
    help="shared-file coherence reproduction without Docker or c2j",
)
p.add_argument(
    "--macos-workspace",
    action="store_true",
    help="focused cold/suspended shared-workspace and macOS c2j tests",
)
p.add_argument(
    "--slots",
    type=int,
    default=1,
    help="number of independent workspace templates to exercise sequentially",
)
p.add_argument("--lifecycle", choices=["cold", "suspended"], default="cold")
a = p.parse_args()
if a.filesystem_only and (a.no_bind or a.macos_workspace or a.relay_only or a.adopt_engine):
    p.error("filesystem-only cannot be combined with another focused mode")
if a.no_bind_extras_only and (not a.no_bind or a.no_bind_platform != "macos"):
    p.error("no-bind-extras-only requires --no-bind --no-bind-platform macos")
if a.no_bind and (a.macos_workspace or a.relay_only or a.adopt_engine or a.slots != 1):
    p.error("no-bind uses one slot per process; other modes must be run separately")
if a.no_bind and a.lifecycle == "suspended" and (a.backend != "tart" or a.no_bind_platform != "macos" or a.no_bind_extras_only or a.rounds < 2):
    p.error("suspended no-bind requires local Tart, --no-bind-platform macos, full jobs and --rounds at least 2")
if a.macos_workspace:
    p.error("shared-workspace execution was superseded; use --no-bind (filesystem-only remains a historical diagnostic)")
if a.rounds < 1:
    p.error("rounds must be positive")
OUT = pathlib.Path(a.output).resolve()
OUT.mkdir(parents=True, exist_ok=True)
TOOLS = pathlib.Path(a.tools).resolve()
PRIVATE = pathlib.Path(tempfile.mkdtemp(prefix="pulse-docker-private-"))
os.chmod(PRIVATE, 0o700)
owner = "pulse-docker-" + uuid.uuid4().hex[:12]
URL = os.environ.get("PULSE_TEST_ORCHARD_URL", "http://192.168.1.195:6120").rstrip("/")
if not URL.endswith("/v1"):
    URL += "/v1"
WORKER = os.environ.get("PULSE_TEST_ORCHARD_WORKER", "jnmac25.local")
TART_HOME = os.environ.get("PULSE_TEST_TART_HOME", str(pathlib.Path.home() / ".tart"))
CASES = json.loads((ROOT / "scripts/docker-prototype/experiments.json").read_text())
report = {
    "schema": 1,
    "backend": a.backend,
    "owner": owner,
    "started_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    "controller_machine": platform.platform(),
    "linux_image": LINUX,
    "build": json.loads((TOOLS / "build.json").read_text()),
    "cases": {
        k: {
            "description": v,
            "status": "not_run",
            "reason": "Not yet exercised by this prototype run.",
        }
        for k, v in CASES.items()
    },
    "samples": [],
    "resources": {
        "per_linux_allocation": {
            "vms": 1,
            "cpu": 4,
            "memory_mib": 8192,
            "worker_memory_mib": 7680,
        },
        "measured_relay_pair": {"vms": 2, "cpu": 8, "memory_mib": 16384},
        "macos_pair_proposal": {
            "vms": 2,
            "primary_cpu": 4,
            "primary_memory_mib": 8192,
            "companion_cpu": 2,
            "companion_memory_mib": 4096,
        },
    },
    "owned": [],
}


def save():
    tmp = OUT / "report.tmp"
    tmp.write_text(json.dumps(report, indent=2))
    os.replace(tmp, OUT / "report.json")


def result(case, status, reason="", **values):
    report["cases"][case].update(status=status, reason=reason, **values)
    save()
    if a.no_bind:
        print(case + ": " + status, flush=True)


def api(method, path, body=None):
    req = urllib.request.Request(
        URL + path,
        data=None if body is None else json.dumps(body).encode(),
        method=method,
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, timeout=30) as r:
        data = r.read()
        return json.loads(data) if data else None


def cmd(args, **kw):
    try:
        return subprocess.run(args, check=True, capture_output=True, **kw)
    except subprocess.CalledProcessError as e:
        name = "command-failure-" + uuid.uuid4().hex[:8] + ".log"
        (OUT / name).write_bytes(
            ("Command: " + shlex.join([str(x) for x in args]) + "\n").encode()
            + (e.stdout or b"")
            + (e.stderr or b"")
        )
        raise RuntimeError("Command failed; see " + name) from e


def tart(*args):
    return cmd(
        ["tart", *args],
        env=dict(os.environ, TART_HOME=TART_HOME, TART_NO_AUTO_PRUNE="true"),
        timeout=900 if args and args[0] in ("clone", "pull") else 120,
    ).stdout


owned = []
worker_helpers = []


def persist(j):
    path = PRIVATE / (j["spec"]["name"] + ".json")
    path.write_text(json.dumps(j))
    path.chmod(0o600)
    return path


def native(j):
    if a.backend == "orchard":
        return api("GET", "/vms/" + j["spec"]["name"])
    return json.loads(tart("get", j["spec"]["name"], "--format", "json"))


def create(s, source=None, resume=False):
    j = {
        "url": URL if a.backend == "orchard" else "",
        "spec": s,
        "started": time.monotonic(),
        "tart_home": TART_HOME,
    }
    owned.append(j)
    persist(j)
    if a.backend == "orchard":
        v = api("POST", "/vms", s)
        j["uid"] = v["uid"]
    else:
        tart("clone", source or s["image"], s["name"])
        if not resume:
            tart(
                "set",
                s["name"],
                "--cpu",
                str(s["cpu"]),
                "--memory",
                str(s["memory"]),
                "--random-mac",
            )
        directory = pathlib.Path(TART_HOME) / "vms" / s["name"]
        j["inode"] = directory.stat().st_ino
        j["uid"] = "local:" + str(j["inode"])
        with open(PRIVATE / (s["name"] + ".run.log"), "wb") as log:
            args = ["tart", "run", "--no-graphics"]
            if s.get("suspendable"):
                args += ["--suspendable"]
            for share in s.get("hostDirs", []):
                args += ["--dir", share["name"] + ":" + share["path"]]
            proc = subprocess.Popen(
                [*args, s["name"]],
                stdout=log,
                stderr=log,
                start_new_session=True,
                env=dict(os.environ, TART_HOME=TART_HOME, TART_NO_AUTO_PRUNE="true"),
            )
            j["pid"] = proc.pid
    persist(j)
    report["owned"].append(
        {
            "name": s["name"],
            "uid": j.get("uid"),
            "role": s["os"],
            "worker": WORKER if a.backend == "orchard" else "local",
        }
    )
    save()
    return j


def guest(j, command, data=None, timeout=180):
    if a.backend == "tart":
        log = PRIVATE / (j["spec"]["name"] + ".run.log")
        if log.exists():
            output = log.read_text(errors="replace")
            if "Error:" in output and native(j).get("State") == "stopped":
                raise RuntimeError(output[-2000:].strip())
        j["address"] = (
            tart("ip", j["spec"]["name"], "--wait", "5").decode().strip() + ":22"
        )
        persist(j)
    return cmd(
        [
            str(TOOLS / "guest"),
            "-state",
            str(persist(j)),
            "-command",
            command,
            "-timeout",
            str(timeout),
        ],
        input=data,
        timeout=timeout + 15,
    ).stdout


def wait(label, fn, seconds=900):
    start = time.monotonic()
    next_log = 0
    last = ""
    while time.monotonic() - start < seconds:
        try:
            value = fn()
            if value:
                return value
        except (
            subprocess.CalledProcessError,
            urllib.error.URLError,
            TimeoutError,
            RuntimeError,
        ) as e:
            last = str(e)
        elapsed = time.monotonic() - start
        if elapsed >= next_log:
            print(label, round(elapsed), "seconds", flush=True)
            next_log = elapsed + 30
        time.sleep(2)
    raise RuntimeError(label + " timed out: " + last)


def upload(j, path, data):
    guest(j, "sudo tee " + shlex.quote(path) + " >/dev/null", data, 180)


def spec(name, osname="linux", image=LINUX, suspendable=False):
    return dict(
        name=name,
        os=osname,
        arch="arm64",
        runtime="tart",
        image=image,
        cpu=4,
        memory=8192,
        headless=True,
        username="admin",
        password=os.environ.get("PULSE_TEST_SSH_PASSWORD", "admin"),
        restart_policy="Never",
        resources={
            "org.cirruslabs.logical-cores": 4,
            "org.cirruslabs.memory-mib": 8192,
            "org.cirruslabs.tart-vms": 1,
        },
        labels={"org.cirruslabs.orchard.worker-name": WORKER},
        startup_script={
            "script_content": "#!/bin/sh\nwhile :; do sleep 60; done\n",
            "env": {},
        },
        suspendable=suspendable,
    )


def prepare(j):
    wait("SSH " + j["spec"]["name"], lambda: guest(j, "true") == b"", 300)
    guest(j, "sudo mkdir -p /opt/pulse-docker")
    upload(
        j,
        "/opt/pulse-docker/prepare.sh",
        (ROOT / "scripts/docker-prototype/prepare-linux.sh").read_bytes(),
    )
    guest(
        j,
        "sudo systemd-run --unit=pulse-docker-prepare /bin/bash /opt/pulse-docker/prepare.sh",
    )
    wait(
        "Docker preparation " + j["spec"]["name"],
        lambda: (
            b"PULSE_DOCKER_PREPARED"
            in guest(j, "sudo journalctl -u pulse-docker-prepare --no-pager")
        ),
        900,
    )
    for name in ["c2j", "jobdb-fixture"]:
        upload(j, "/opt/pulse-docker/" + name, (TOOLS / name).read_bytes())
    for name in [
        "engine-contract.sh",
        "accept.py",
        "run-job.py",
        "filesystem.py",
        "sdk.py",
        "check-workspace.py",
    ]:
        upload(
            j,
            "/opt/pulse-docker/" + name,
            (ROOT / "scripts/docker-prototype" / name).read_bytes(),
        )
    guest(
        j,
        "sudo chmod 755 /opt/pulse-docker/*; sudo systemd-run --unit=pulse-docker-jobdb --property=User=pulseworker --setenv=HOME=/home/pulseworker /opt/pulse-docker/jobdb-fixture",
    )
    wait(
        "JobDB " + j["spec"]["name"],
        lambda: b'"ready":true' in guest(j, "curl -fsS http://127.0.0.1:18881/health"),
        60,
    )
    report["samples"].append(
        {
            "vm": j["spec"]["name"],
            "cold_prepare_seconds": time.monotonic() - j["started"],
        }
    )
    versions = guest(
        j,
        "uname -a; cat /etc/os-release; docker --version; docker buildx version; docker compose version; /opt/pulse-docker/c2j version; sudo docker info --format '{{json .}}'; free -b; df -B1 / /var/lib/docker",
    )
    (OUT / (j["spec"]["name"] + "-versions.log")).write_bytes(versions)
    return j


def allocation(j, action, launch=None):
    args = [
        str(TOOLS / "allocation"),
        "-state",
        str(persist(j)),
        "-journal",
        str(PRIVATE / (j["spec"]["name"] + "-allocation")),
        "-action",
        action,
    ]
    if "engine" in j:
        args += [
            "-engine-state",
            str(persist(j["engine"])),
            "-workspace",
            j["workspace"],
            "-base",
            j["base"],
        ]
    return json.loads(
        cmd(
            args,
            input=None if launch is None else json.dumps(launch).encode(),
            timeout=180,
        ).stdout
    )


def run_job(j):
    caps = allocation(j, "capabilities")
    assert caps["bases_complete"] and "docker" in caps["bases"][0]["capabilities"]
    invalid = {
        "launch_id": "unsupported",
        "base": "ubuntu-*" if "engine" in j else "macos*",
        "job": {
            "jobdb": "http://127.0.0.1:18880/test",
            "job_id": "unsupported",
            "instance_id": "test",
            "tenant_id": "test",
        },
        "lease": "not-a-lease",
    }
    assert allocation(j, "submit", invalid)[0]["status"] == "unsupported"
    launch = json.loads(guest(j, "curl -fsS 'http://127.0.0.1:18881/launch?case=valid'"))
    launch.update(
        launch_id=j["spec"]["name"] + "-job",
        base="macos-*arm64" if "engine" in j else "ubuntu-*arm64",
        capabilities=["docker", "git"],
        cpu_millis=1000,
        memory_bytes=2 << 30,
    )
    begin = time.monotonic()
    assert allocation(j, "submit", launch)[0]["status"] == "accepted"
    assert (
        allocation(j, "submit", launch)[0]["status"] == "accepted"
    )  # New controller process and same durable state.

    def complete():
        s = json.loads(guest(j, "curl -fsS 'http://127.0.0.1:18881/status?case=valid'"))
        if s["status"] not in ("COMPLETED", "FAILED", "CANCELED", "CANCELLED"):
            if s.get("claims", 0) == 1 and s.get("renewals", 0) > 0:
                observed = time.time()
                j.setdefault("job_running_first_unix", observed)
                j["job_running_last_unix"] = observed
            return None
        return s

    try:
        status = wait("c2j " + j["spec"]["name"], complete, 600)
    finally:
        try:
            (OUT / (j["spec"]["name"] + "-worker.log")).write_bytes(
                guest(
                    j,
                    "sudo cat /var/lib/pulse-docker/worker.log /var/lib/pulse-docker/runner.log 2>/dev/null || true",
                )
            )
        except Exception:
            pass
    (OUT / (j["spec"]["name"] + "-job.json")).write_text(json.dumps(status, indent=2))
    code = guest(j, "sudo cat /var/lib/pulse-docker/exit").decode().strip()
    report["samples"].append(
        {
            "vm": j["spec"]["name"],
            "job_seconds": time.monotonic() - begin,
            "exit": code,
            "jobdb": status,
        }
    )
    if not status["output_ok"] or code != "0":
        (OUT / (j["spec"]["name"] + "-worker.log")).write_bytes(
            guest(j, "sudo cat /var/lib/pulse-docker/worker.log")
        )
        (OUT / (j["spec"]["name"] + "-contract.log")).write_bytes(
            guest(j, "cat /var/tmp/pulse-docker-contract.log 2>/dev/null || true")
        )
        raise RuntimeError("real c2j Docker job failed; see worker and contract logs")
    (OUT / (j["spec"]["name"] + "-contract.log")).write_bytes(
        guest(j, "cat /var/tmp/pulse-docker-contract.log")
    )
    assert (
        status["claims"] == 1
        and status["secret_calls"] == 1
        and status["renewals"] >= 1
    )
    assert allocation(j, "list")["items"] == []
    return status


def cleanup(j):
    if j.get("cleaned"):
        return
    start = time.monotonic()
    name = j["spec"]["name"]
    try:
        v = native(j)
        if a.backend == "orchard":
            assert (
                v["uid"] == j.get("uid")
                and v["image"] == j["spec"]["image"]
                and v["startup_script"].get("script_content")
                == j["spec"]["startup_script"].get("script_content")
                and (v["startup_script"].get("env") or {})
                == (j["spec"]["startup_script"].get("env") or {})
            )
            # These are explicitly disposable test allocations, never production jobs.
            if v.get("status") not in ("failed", "stopped"):
                v["powerState"] = "stopped"
                api("PUT", "/vms/" + name, v)
            wait(
                "stop " + name,
                lambda: any(
                    c["type"] == "running" and c["state"] == "false"
                    for c in native(j).get("conditions", [])
                ),
                90,
            )
            api("DELETE", "/vms/" + name)
            try:
                api("GET", "/vms/" + name)
            except urllib.error.HTTPError as e:
                assert e.code == 404
            else:
                raise RuntimeError("owned API record still exists")
        else:
            directory = pathlib.Path(TART_HOME) / "vms" / name
            assert directory.stat().st_ino == j["inode"]
            log = PRIVATE / (name + ".run.log")
            if log.exists():
                (OUT / (name + "-native.log")).write_bytes(log.read_bytes())
            if v["State"] == "running":
                tart("stop", name)
                wait("native stop " + name, lambda: native(j)["State"] != "running", 30)
            tart("delete", name)
            if directory.exists():
                raise RuntimeError("owned native directory still exists")
        j["cleaned"] = True
        persist(j)
        report["samples"].append(
            {"vm": name, "cleanup_seconds": time.monotonic() - start}
        )
    except Exception as e:
        report.setdefault("cleanup_errors", []).append({"vm": name, "error": str(e)})
        raise


def mac_relay(engine, mac=None, on_ready=None, task_user="admin"):
    owns_mac = mac is None
    macimage = "ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77"
    if owns_mac:
        mac = create(spec(owner + "-relay-mac", "darwin", macimage))
    tunnel = None
    tunnel_log = None
    try:
        wait("macOS relay SSH", lambda: guest(mac, "true") == b"", 300)
        key = PRIVATE / (mac["spec"]["name"] + "-engine-key")
        cmd(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(key)])
        guest(
            engine,
            "sudo useradd -m -s /bin/sh pulseengine; sudo usermod -aG docker pulseengine; sudo install -d -m 700 -o pulseengine /home/pulseengine/.ssh",
        )
        upload(
            engine,
            "/home/pulseengine/.ssh/authorized_keys",
            key.with_suffix(".pub").read_bytes(),
        )
        upload(
            engine,
            "/etc/ssh/sshd_config.d/pulse-engine.conf",
            b"Match User pulseengine\n PasswordAuthentication no\n PubkeyAuthentication yes\n AllowTcpForwarding local\n AllowStreamLocalForwarding local\n",
        )
        guest(
            engine,
            "sudo chmod 600 /home/pulseengine/.ssh/authorized_keys; sudo chown -R pulseengine:pulseengine /home/pulseengine/.ssh; sudo sshd -t; sudo systemctl reload ssh",
        )
        if a.no_bind:
            from no_bind import endpoint_transport
            address, hostkey = endpoint_transport(globals(), engine, mac)
        else:
            address = None
            hostkey = guest(engine, "cat /etc/ssh/ssh_host_ed25519_key.pub")
        ip = guest(engine, "hostname -I | awk '{print $1}'").decode().strip()
        guest(
            mac,
            "sudo mkdir -p /opt/pulse-docker; sudo chmod 700 /opt/pulse-docker; sudo chown admin:staff /opt/pulse-docker",
        )
        for name, data in [
            ("relay", (TOOLS / "relay-darwin").read_bytes()),
            ("testcontainers-go", (TOOLS / "testcontainers-darwin").read_bytes()),
            ("engine-key", key.read_bytes()),
            ("engine-host-key", hostkey),
            (
                "relay-probe.py",
                (ROOT / "scripts/docker-prototype/relay-probe.py").read_bytes(),
            ),
        ]:
            upload(mac, "/opt/pulse-docker/" + name, data)
        # Measure direct routing, then use an explicitly reported controller
        # tunnel when the guest network isolates siblings.
        direct = (
            guest(
                mac,
                "nc -z -G 3 "
                + shlex.quote(ip)
                + " 22 >/dev/null 2>&1 && echo reachable || echo isolated",
            ).strip()
            == b"reachable"
        )
        report["direct_guest_route"] = {"address": ip, "reachable": direct}
        if address is not None:
            report["engine_data_path"] = "Worker endpoint to allocation-only SSH service; independent of one-shot provider processes."
        elif not direct:
            tunnel_log = open(OUT / "engine-tunnel.log", "wb")

            def start_tunnel():
                process = subprocess.Popen(
                    [
                        str(TOOLS / "tunnel"),
                        "-primary",
                        str(persist(mac)),
                        "-engine",
                        str(persist(engine)),
                    ],
                    stdout=subprocess.PIPE,
                    stderr=tunnel_log,
                )
                import select

                if not select.select([process.stdout], [], [], 45)[0]:
                    process.terminate()
                    process.wait(timeout=15)
                    raise RuntimeError("engine tunnel startup timed out")
                if process.stdout.readline().strip() != b"PULSE_ENGINE_TUNNEL_READY":
                    process.terminate()
                    process.wait(timeout=15)
                    raise RuntimeError("engine tunnel did not start")
                return process

            tunnel = start_tunnel()
            address = "127.0.0.1:22443"
            report["engine_data_path"] = (
                "Per-allocation end-to-end SSH over an external-controller tunnel; no controller credentials in guests."
            )
        else:
            address = ip + ":22"
            report["engine_data_path"] = "Direct per-allocation guest-to-guest SSH."
        config = {
            "Address": address,
            "User": "pulseengine",
            "KeyFile": "/opt/pulse-docker/engine-key",
            "HostKeyFile": "/opt/pulse-docker/engine-host-key",
            "Socket": "/opt/pulse-docker/docker.sock",
        }
        upload(mac, "/opt/pulse-docker/config.json", json.dumps(config).encode())
        guest(
            mac,
            "sudo chown "
            + task_user
            + " /opt/pulse-docker /opt/pulse-docker/engine-key /opt/pulse-docker/config.json; sudo chmod 600 /opt/pulse-docker/engine-key /opt/pulse-docker/config.json; sudo chmod 755 /opt/pulse-docker/relay /opt/pulse-docker/testcontainers-go; sudo ln -sf /opt/pulse-docker/docker.sock /var/run/docker.sock",
        )
        plist = b"""<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>Label</key><string>dev.pulse.docker-relay</string><key>UserName</key><string>admin</string><key>ProgramArguments</key><array><string>/opt/pulse-docker/relay</string><string>--config</string><string>/opt/pulse-docker/config.json</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardOutPath</key><string>/var/tmp/pulse-docker-relay.log</string><key>StandardErrorPath</key><string>/var/tmp/pulse-docker-relay.log</string></dict></plist>"""
        plist = plist.replace(
            b"<string>admin</string>", ("<string>" + task_user + "</string>").encode()
        )
        upload(mac, "/Library/LaunchDaemons/dev.pulse.docker-relay.plist", plist)
        guest(
            mac,
            "sudo chmod 644 /Library/LaunchDaemons/dev.pulse.docker-relay.plist; sudo launchctl bootstrap system /Library/LaunchDaemons/dev.pulse.docker-relay.plist",
        )
        wait(
            "authenticated Docker relay",
            lambda: (
                guest(
                    mac,
                    "sudo -u "
                    + task_user
                    + " curl -fsS --unix-socket /var/run/docker.sock http://localhost/_ping",
                ).strip()
                == b"OK"
            ),
            90,
        )
        (OUT / "macos-relay-host.log").write_bytes(
            guest(mac, "sw_vers; uname -m; id; mount; /usr/bin/python3 --version")
        )
        result(
            "macos_authenticated_api",
            "passed",
            "Per-allocation SSH key, pinned engine host key, real macOS Unix socket including conventional /var/run/docker.sock. Public admin image credentials remain a lab limitation.",
        )
        data = guest(
            mac,
            "sudo -u "
            + task_user
            + " /usr/bin/python3 /opt/pulse-docker/relay-probe.py",
            timeout=180,
        )
        (OUT / "macos-relay-network.json").write_bytes(data)
        for case in ("macos_tcp_ports", "macos_start_gate", "macos_port_conflicts"):
            result(
                case,
                "passed",
                "Real macOS/Linux IPv4 TCP gateway; immediate local connect, dynamic loopback/wildcard mappings and local bind conflict. See macos-relay-network.json.",
            )
        result(
            "macos_docker_api",
            "not_run",
            "Real ping/inspect/start/exec/logs/copy passed through relay. CLI plugins, build and attach coverage remain incomplete.",
        )
        (OUT / "macos-testcontainers-go.log").write_bytes(
            guest(
                mac,
                "sudo -u "
                + task_user
                + " env TMPDIR=/var/tmp DOCKER_HOST=unix:///var/run/docker.sock TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE=/var/run/docker.sock /opt/pulse-docker/testcontainers-go",
                timeout=300,
            )
        )
        result(
            "macos_testcontainers",
            "not_run",
            "Go Testcontainers with Ryuk passed; Java on macOS has not been run.",
        )
        if on_ready is not None:
            on_ready(mac, engine)
        # A force-killed relay must recover existing published services and stale socket.
        guest(mac, "sudo launchctl kickstart -k system/dev.pulse.docker-relay")
        wait(
            "relay restart",
            lambda: (
                guest(
                    mac,
                    "sudo -u "
                    + task_user
                    + " curl -fsS --unix-socket /var/run/docker.sock http://localhost/_ping",
                ).strip()
                == b"OK"
            ),
            30,
        )
        assert guest(mac, "curl -fsS http://localhost:18080")
        result(
            "macos_relay_recovery",
            "passed",
            "Force-killed relay reconnected and rediscovered preexisting Linux publication at localhost:18080.",
        )
        guest(engine, "sudo systemctl stop docker.socket docker.service")
        code = guest(
            mac,
            "sudo -u "
            + task_user
            + " curl -s --unix-socket /var/run/docker.sock -o /dev/null -w '%{http_code}' http://localhost/_ping",
        ).decode()
        assert code == "502"
        guest(engine, "sudo systemctl start docker.service")
        wait(
            "engine restart",
            lambda: (
                guest(
                    mac,
                    "sudo -u "
                    + task_user
                    + " curl -fsS --unix-socket /var/run/docker.sock http://localhost/_ping",
                ).strip()
                == b"OK"
            ),
            30,
        )
        result(
            "fault_engine_controller_relay",
            "not_run",
            "Engine stop returned 502; engine and force-killed relay recovered. Controller independence covered for Linux, but full paired-allocation fault matrix remains untested.",
        )
    finally:
        if tunnel is not None:
            tunnel.terminate()
            try:
                tunnel.wait(timeout=15)
            except subprocess.TimeoutExpired:
                tunnel.kill()
                tunnel.wait()
        if tunnel_log is not None:
            tunnel_log.close()
        try:
            (OUT / "macos-relay.log").write_bytes(
                guest(mac, "cat /var/tmp/pulse-docker-relay.log 2>/dev/null || true")
            )
        except Exception:
            pass
        if owns_mac:
            cleanup(mac)


def native_shared():
    # Host-side experiment is explicit and confined to this run's private root.
    workspace = PRIVATE / "shared-allocation" / "workspace"
    workspace.mkdir(parents=True)
    workspace.chmod(0o777)
    (workspace / "host-write").write_text("host")
    macimage = "ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77"
    primary = spec(owner + "-shared-mac", "darwin", macimage)
    engine = spec(owner + "-shared-linux")
    for config in (primary, engine):
        config["hostDirs"] = [{"name": "work", "path": str(workspace), "ro": False}]
    mac, linux = create(primary), create(engine)
    try:
        wait("shared macOS SSH", lambda: guest(mac, "true") == b"", 300)
        prepare(linux)
        guest(
            mac,
            "sudo umount '/Volumes/My Shared Files' 2>/dev/null || true; sudo mkdir -p /Users/Shared/pulse; sudo mount_virtiofs com.apple.virtio-fs.automount /Users/Shared/pulse",
        )
        guest(
            linux,
            "sudo mkdir -p /Users/Shared/pulse; sudo mount -t virtiofs com.apple.virtio-fs.automount /Users/Shared/pulse",
        )
        guest(
            mac,
            'test "$(cat /Users/Shared/pulse/work/host-write)" = host; echo macos >/Users/Shared/pulse/work/mac-write',
        )
        guest(
            linux,
            'test "$(cat /Users/Shared/pulse/work/mac-write)" = macos; echo linux >/Users/Shared/pulse/work/linux-write',
        )
        assert (workspace / "linux-write").read_text().strip() == "linux"
        guest(
            linux,
            "sudo -u pulseworker docker run --rm -v /Users/Shared/pulse/work:/work alpine:3.22 sh -c 'cat /work/mac-write; echo container >/work/container-write'",
        )
        assert (
            guest(mac, "cat /Users/Shared/pulse/work/container-write").strip()
            == b"container"
        )
        result(
            "macos_shared_mounts",
            "passed",
            "Host/macOS/Linux/container bidirectional visibility at /Users/Shared/pulse/work. No c2j claim is implied.",
        )
        for label, j in [("macos", mac), ("linux", linux)]:
            program = (ROOT / "scripts/docker-prototype/filesystem.py").read_bytes()
            data = guest(j, "python3 - /Users/Shared/pulse/work/" + label, program)
            (OUT / ("shared-" + label + "-fs.json")).write_bytes(data)
        result(
            "shared_fs_semantics",
            "not_run",
            "Per-guest primitives measured; cross-guest locking, notifications, concurrent writers and escape tests remain unverified.",
        )
        # Explicit stale-handle experiment, with both guests still holding mounts.
        os.rename(workspace, workspace.with_name("old-workspace"))
        workspace.mkdir()
        workspace.chmod(0o777)
        (workspace / "generation").write_text("replacement")
        observed = {}
        for label, j in [("macos", mac), ("linux", linux)]:
            observed[label] = (
                guest(
                    j,
                    "cat /Users/Shared/pulse/work/generation 2>/dev/null || echo stale-handle",
                )
                .decode()
                .strip()
            )
        result(
            "share_directory_replacement",
            "passed"
            if all(v == "replacement" for v in observed.values())
            else "failed",
            "Replacement at same host pathname while mounted.",
            observed=observed,
        )
    finally:
        cleanup(mac)
        cleanup(linux)
    # Exact --suspendable + --dir configuration; capture boot/save failure verbatim.
    s = spec(owner + "-shared-suspend", "darwin", macimage, True)
    s["hostDirs"] = primary["hostDirs"]
    j = create(s)
    try:
        wait("suspendable shared macOS", lambda: guest(j, "true") == b"", 90)
        tart("suspend", s["name"])
        result(
            "share_suspend",
            "not_run",
            "macOS suspension succeeded; clone restore and Linux equivalent still need verification.",
        )
    except Exception as e:
        log = (PRIVATE / (s["name"] + ".run.log")).read_text(errors="replace")
        (OUT / "macos-share-suspend.log").write_text(log)
        result("share_suspend", "failed", log[-6000:] or str(e))
    finally:
        cleanup(j)


save()
try:
    if a.backend == "orchard":
        report["controller"] = api("GET", "/controller/info")
        report["workers"] = api("GET", "/workers")
        report["cluster_settings"] = api("GET", "/cluster-settings")
        if not a.no_bind:
            share = spec(owner + "-share-policy")
            share["hostDirs"] = [
                {"name": "workspace", "path": "/tmp/" + owner + "/workspace", "ro": False}
            ]
            try:
                j = create(share)
                result(
                    "orchard_workspace_provision",
                    "not_run",
                    "HostDir creation accepted, but no scoped remote directory-create/delete API is implemented; no security settings were changed.",
                )
                cleanup(j)
            except urllib.error.HTTPError as e:
                body = e.read().decode()
                result("orchard_workspace_provision", "failed", body, http_status=e.code)
                if owned and owned[-1].get("uid") is None:
                    owned[-1]["cleaned"] = True
    else:
        if platform.system() != "Darwin" or platform.machine() != "arm64":
            raise RuntimeError("local Tart requires an Apple Silicon Mac")
        report["tart_version"] = tart("--version").decode()
        report["host"] = "\n".join(
            line
            for line in cmd(["system_profiler", "SPHardwareDataType"])
            .stdout.decode()
            .splitlines()
            if not any(
                key in line
                for key in ("Serial Number", "Hardware UUID", "Provisioning UDID")
            )
        )
        report["host_os"] = cmd(["sw_vers"]).stdout.decode()
    if a.filesystem_only:
        if a.backend != "tart":
            raise RuntimeError("filesystem reproduction requires the Mac host")
        from coherence import run_coherence

        run_coherence(globals())
    elif a.no_bind:
        from no_bind import run_no_bind

        run_no_bind(globals())
    elif a.macos_workspace:
        if a.backend != "tart":
            raise RuntimeError(
                "shared-workspace suite requires the Mac host; Orchard scoped workspace provisioning is unavailable"
            )
        from workspace import run_workspace

        run_workspace(globals())
    elif a.relay_only:
        engine = create(spec(owner + "-relay-engine"))
        prepare(engine)
        guest(
            engine,
            "sudo docker run -d --name identical-container -p 127.0.0.1:18080:8080 python:3.13-alpine python -m http.server 8080",
        )
        mac_relay(engine)
    elif a.adopt_engine:
        engine = json.loads(pathlib.Path(a.adopt_engine).read_text())
        owned.append(engine)
        persist(engine)
        mac_relay(engine)
    else:
        for generation in range(a.rounds):
            print("Generation", generation + 1, "of", a.rounds, flush=True)
            pair = [create(spec(owner + f"-g{generation}-s{i}")) for i in range(2)]
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                list(pool.map(prepare, pair))
            result(
                "linux_cold_boot",
                "passed",
                samples=sum("cold_prepare_seconds" in s for s in report["samples"]),
            )
            with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
                list(pool.map(run_job, pair))
            for case in [
                "linux_jobdb",
                "linux_api_discovery",
                "linux_replay",
                "linux_cli",
            ]:
                result(
                    case,
                    "passed",
                    "Executed through supplied JobDB lease and shared Provider API; see per-VM job reports.",
                )
            # Same container/volume names and same fixed port on both private engines.
            for j in pair:
                guest(
                    j,
                    "sudo -u pulseworker docker volume create identical-volume; sudo -u pulseworker docker run -d --name identical-container -p 127.0.0.1:18080:8080 -v identical-volume:/data python:3.13-alpine python -m http.server 8080",
                )
            ids = [
                guest(j, "sudo docker info --format '{{.ID}}'").decode().strip()
                for j in pair
            ]
            assert ids[0] != ids[1]
            for j in pair:
                wait(
                    "isolated fixed publication",
                    lambda: guest(j, "curl -fsS http://localhost:18080") != b"",
                    30,
                )
            result(
                "linux_two_slots",
                "passed",
                "Two engines with distinct IDs, identical names/volumes/ports, concurrent jobs.",
                engine_ids=ids,
            )
            # Prune only the first allocation's private daemon; the second remains live.
            guest(
                pair[0],
                "sudo -u pulseworker docker rm -f identical-container; sudo -u pulseworker docker system prune --all --volumes --force",
                timeout=90,
            )
            assert guest(pair[1], "curl -fsS http://localhost:18080")
            result(
                "private_prune",
                "passed",
                "Pruned first private VM; second VM container stayed reachable.",
            )
            for j in pair:
                data = guest(
                    j,
                    "sudo -u pulseworker python3 /opt/pulse-docker/filesystem.py /Users/pulseworker/work/native-baseline",
                )
                (OUT / (j["spec"]["name"] + "-fs.json")).write_bytes(data)
            result(
                "linux_native_fs",
                "passed",
                "Native-disk primitives and timings recorded; this is not shared-filesystem evidence.",
            )
            if generation == 0:
                j = pair[1]
                guest(
                    j,
                    "sudo apt-get update >/dev/null && sudo env DEBIAN_FRONTEND=noninteractive apt-get install -y python3-venv maven openjdk-21-jdk-headless >/dev/null",
                    timeout=600,
                )
                guest(
                    j,
                    "sudo -u pulseworker python3 -m venv /home/pulseworker/sdk; sudo -u pulseworker /home/pulseworker/sdk/bin/pip install docker==7.1.0",
                    timeout=180,
                )
                (OUT / "linux-sdk.json").write_bytes(
                    guest(
                        j,
                        "sudo -u pulseworker /home/pulseworker/sdk/bin/python /opt/pulse-docker/sdk.py",
                    )
                )
                result(
                    "linux_sdk",
                    "passed",
                    "Official Python Docker SDK, concurrent ping and container run.",
                )
                upload(
                    j,
                    "/opt/pulse-docker/testcontainers-go",
                    (TOOLS / "testcontainers-go").read_bytes(),
                )
                guest(j, "sudo chmod 755 /opt/pulse-docker/testcontainers-go")
                (OUT / "linux-testcontainers-go.log").write_bytes(
                    guest(
                        j,
                        "sudo -u pulseworker env HOME=/home/pulseworker /opt/pulse-docker/testcontainers-go",
                        timeout=300,
                    )
                )
                result(
                    "linux_testcontainers_go",
                    "passed",
                    "Resource reaper enabled; HTTP service reached through mapped port.",
                )
                guest(
                    j,
                    "sudo -u pulseworker mkdir -p /home/pulseworker/java/src/main/java",
                )
                for local, remote in [
                    ("pom.xml", "pom.xml"),
                    ("src/main/java/Probe.java", "src/main/java/Probe.java"),
                ]:
                    upload(
                        j,
                        "/home/pulseworker/java/" + remote,
                        (
                            ROOT / "experimental/docker/testcontainers-java" / local
                        ).read_bytes(),
                    )
                guest(j, "sudo chown -R pulseworker:pulseworker /home/pulseworker/java")
                (OUT / "linux-testcontainers-java.log").write_bytes(
                    guest(
                        j,
                        "sudo -u pulseworker env HOME=/home/pulseworker bash -c 'cd /home/pulseworker/java && mvn -q compile dependency:build-classpath -Dmdep.outputFile=classpath && java -cp \"target/classes:$(cat classpath)\" Probe'",
                        timeout=600,
                    )
                )
                result(
                    "linux_testcontainers_java",
                    "passed",
                    "Resource reaper enabled; HTTP service reached through mapped port.",
                )
            if generation == 0:
                cleanup(pair[0])
                try:
                    mac_relay(pair[1])
                except Exception as e:
                    report.setdefault("component_errors", []).append(
                        "macOS relay: " + str(e)
                    )
                    save()
            for j in pair:
                cleanup(j)
        if a.rounds >= 2:
            result("linux_two_generations", "passed", generations=a.rounds, slots=2)
        if a.backend == "tart":
            native_shared()
        # New Linux source run explicitly requests suspension; no assumption that the
        # deployed Tart has the source-tree feature or a suitable machine identifier.
        j = create(spec(owner + "-linux-suspend", suspendable=True))
        try:
            wait("suspendable Linux boot", lambda: guest(j, "true") == b"", 90)
            before = guest(j, "cat /proc/sys/kernel/random/boot_id").decode().strip()
            if a.backend == "orchard":
                v = native(j)
                v["powerState"] = "suspended"
                api("PUT", "/vms/" + j["spec"]["name"], v)
            else:
                tart("suspend", j["spec"]["name"])
            wait(
                "Linux suspension",
                lambda: (
                    any(
                        c["type"] == "running" and c["state"] == "false"
                        for c in native(j).get("conditions", [])
                    )
                    if a.backend == "orchard"
                    else native(j)["State"] == "suspended"
                ),
                90,
            )
            result(
                "linux_suspend",
                "not_run",
                "Suspended successfully; cloned restore/machine identity not yet verified.",
                boot_id=before,
            )
        except Exception as e:
            v = native(j)
            detail = v.get("status_message", str(e))
            result("linux_suspend", "failed", detail)
            if a.backend == "orchard":
                (OUT / "linux-suspend-events.json").write_text(
                    json.dumps(
                        api("GET", "/vms/" + j["spec"]["name"] + "/events"), indent=2
                    )
                )
            else:
                (OUT / "linux-suspend-run.log").write_bytes(
                    (PRIVATE / (j["spec"]["name"] + ".run.log")).read_bytes()
                )
        cleanup(j)

except Exception as e:
    report["run_error"] = str(e)
    print("Experiment stopped:", str(e), flush=True)
finally:
    for j in owned:
        if not j.get("cleaned"):
            try:
                cleanup(j)
            except Exception as e:
                print(
                    "Cleanup needs inspection:", j["spec"]["name"], str(e), flush=True
                )
    for process, log in worker_helpers:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait()
        log.close()
    for key, value in report["cases"].items():
        if (
            value["status"] == "not_run"
            and value["reason"] == "Not yet exercised by this prototype run."
        ):
            value["reason"] = (
                "Coverage remains unimplemented or unavailable; see findings. This is not a passing result."
            )
    report["scope"] = (
        "filesystem-coherence"
        if a.filesystem_only
        else ("no-bind-" + a.no_bind_platform if a.no_bind else ("macos-workspace" if a.macos_workspace else "full-contract"))
    )
    report["complete"] = all(
        c["status"] == "passed" for k, c in report["cases"].items() if k != "amd64"
    )
    required = list(report["cases"])
    if a.filesystem_only:
        required = ["shared_file_coherence", "shared_container_coherence"]
    elif a.no_bind:
        from no_bind import required_cases
        required = required_cases(a.no_bind_platform, a.no_bind_extras_only)
        if a.lifecycle == "suspended":
            required += ["no_bind_snapshot_reuse"]
    elif a.macos_workspace:
        required = [
            "macos_c2j_workspace",
            "macos_shared_mounts",
            "stable_workspace_generations",
            "macos_authenticated_api",
            "macos_tcp_ports",
            "macos_start_gate",
            "macos_port_conflicts",
            "macos_relay_recovery",
        ]
        if a.lifecycle == "suspended":
            required += ["macos_shared_resume"]
    report["required_cases"] = required
    report["result"] = (
        "passed"
        if all(
            report["cases"][k]["status"] == "passed" for k in required if k != "amd64"
        )
        and not report.get("run_error")
        and not report.get("cleanup_errors")
        else "incomplete"
    )
    report["finished_unix"] = time.time()
    report["private_state"] = str(PRIVATE)
    save()
    display_out = pathlib.Path(a.display_output) if a.display_output else OUT
    print(
        "Report:", display_out / "report.json", "Result:", report["result"], flush=True
    )
sys.exit(0 if report["result"] == "passed" else 2)
