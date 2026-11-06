"""Focused local Tart macOS execution with a stable, allocation-owned share.

The host root is never replaced. Each slot owns one prepared macOS template;
only one of its clones runs at once. Linux companions are always fresh cold VMs.
"""

import concurrent.futures
import json
import os
import pathlib
import plistlib
import shlex
import shutil
import time
from types import SimpleNamespace

MAC_IMAGE = "ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77"
MOUNT = "/Users/Shared/pulse"
SHARE = MOUNT + "/work"


def clear_job(path, stable, inode):
    """Remove only this allocation's child, after native teardown has succeeded."""
    if stable.is_symlink() or stable.stat().st_ino != inode:
        raise RuntimeError("stable workspace identity changed; refusing cleanup")
    if path.parent != stable / "jobs" or path.parent.is_symlink() or path.is_symlink():
        raise RuntimeError("unowned job workspace; refusing cleanup")
    shutil.rmtree(path)
    if stable.stat().st_ino != inode:
        raise RuntimeError("workspace root changed during cleanup")


def run_workspace(environment):
    h = SimpleNamespace(**environment)
    if h.a.slots not in (1, 2) or h.a.rounds < 2:
        raise RuntimeError(
            "workspace suite requires --slots 1 or 2 and --rounds at least 2"
        )
    h.report["workspace_lifecycle"] = h.a.lifecycle
    h.report["workspace_slots"] = h.a.slots
    h.report["docker_clients"] = json.loads(
        (h.TOOLS / "docker-clients.json").read_text()
    )
    h.report["workspace_design"] = (
        "Stable share inode; fresh per-job subdirectories; no directory replacement; no Linux suspension."
    )
    templates = []
    slots = []

    def command(vm, text, **kw):
        return h.guest(vm, "set -eu; " + text, **kw)

    def mount(vm, mac=False):
        if mac:
            # Same command for cold clones; a resumed mount must already be usable.
            command(
                vm,
                "if ! mount | grep -F ' on /Users/Shared/pulse ' >/dev/null; then sudo umount '/Volumes/My Shared Files' 2>/dev/null || true; sudo mkdir -p /Users/Shared/pulse; sudo mount_virtiofs com.apple.virtio-fs.automount /Users/Shared/pulse; fi",
            )
        else:
            command(
                vm,
                "sudo mkdir -p /Users/Shared/pulse; sudo mount -t virtiofs com.apple.virtio-fs.automount /Users/Shared/pulse",
            )
        assert b"virtiofs" in command(vm, "mount").lower(), "virtiofs mount missing"

    def prepare_mac(vm):
        h.wait("macOS template SSH", lambda: h.guest(vm, "true") == b"", 300)
        command(vm, "sudo mkdir -p /opt/pulse-docker")
        for local, remote in [
            ("c2j-darwin", "c2j"),
            ("jobdb-fixture-darwin", "jobdb-fixture"),
            ("docker-darwin", "docker"),
            ("docker-buildx-darwin", "docker-buildx"),
            ("docker-compose-darwin", "docker-compose"),
        ]:
            h.upload(vm, "/opt/pulse-docker/" + remote, (h.TOOLS / local).read_bytes())
        for name in (
            "prepare-macos.sh",
            "engine-contract.sh",
            "accept.py",
            "run-job.py",
            "check-workspace.py",
        ):
            h.upload(
                vm,
                "/opt/pulse-docker/" + name,
                (h.ROOT / "scripts/docker-prototype" / name).read_bytes(),
            )
        command(vm, "sudo bash /opt/pulse-docker/prepare-macos.sh")
        mount(vm, True)

    def fixture(vm):
        service = {
            "Label": "dev.pulse.docker-fixture",
            "UserName": "pulseworker",
            "ProgramArguments": ["/opt/pulse-docker/jobdb-fixture"],
            "RunAtLoad": True,
            "EnvironmentVariables": {
                "HOME": "/Users/pulseworker",
                "TMPDIR": "/var/tmp",
                "PATH": "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
            },
            "StandardOutPath": "/var/tmp/pulse-fixture.log",
            "StandardErrorPath": "/var/tmp/pulse-fixture.log",
        }
        h.upload(
            vm,
            "/Library/LaunchDaemons/dev.pulse.docker-fixture.plist",
            plistlib.dumps(service),
        )
        command(
            vm,
            "sudo chmod 644 /Library/LaunchDaemons/dev.pulse.docker-fixture.plist; sudo launchctl bootstrap system /Library/LaunchDaemons/dev.pulse.docker-fixture.plist",
        )
        h.wait(
            "macOS JobDB fixture",
            lambda: (
                b'"ready":true'
                in h.guest(vm, "curl -fsS http://127.0.0.1:18881/health")
            ),
            60,
        )

    def new_spec(name, slot, mac=False):
        s = h.spec(name, "darwin", MAC_IMAGE) if mac else h.spec(name)
        s["hostDirs"] = [{"name": "work", "path": str(slot["root"]), "ro": False}]
        return s

    def round_slot(slot, generation):
        prefix = h.owner + "-s%d-g%d" % (slot["index"], generation)
        job = slot["root"] / "jobs" / prefix
        job.mkdir(mode=0o777)
        (job / "tmp").mkdir(mode=0o777)
        job.chmod(0o777)
        (job / "tmp").chmod(0o777)
        workspace = SHARE + "/jobs/" + prefix
        linux = mac = None
        begin = time.monotonic()
        try:
            linux = h.create(new_spec(prefix + "-engine", slot))
            s = new_spec(prefix + "-mac", slot, True)
            s["suspendable"] = h.a.lifecycle == "suspended"
            mac = h.create(
                s, source=slot["template"]["spec"]["name"], resume=s["suspendable"]
            )
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                preparation = pool.submit(h.prepare, linux)
                h.wait("macOS clone SSH", lambda: h.guest(mac, "true") == b"", 180)
                primary_ssh_seconds = time.monotonic() - mac["started"]
                preparation.result()
            mount(linux)
            if not s["suspendable"]:
                mount(mac, True)
            boot = command(mac, "sysctl -n kern.bootsessionuuid").decode().strip()
            if s["suspendable"] and boot != slot["boot"]:
                raise RuntimeError("suspended clone cold-booted; boot identity changed")
            # Unique job directories and retained root identity, observed from both guests.
            assert slot["root"].stat().st_ino == slot["inode"]
            token = prefix + "-host"
            (job / "host-marker").write_text(token)
            for vm in (mac, linux):
                assert (
                    command(vm, "cat " + workspace + "/host-marker").decode() == token
                )
                if slot.get("previous"):
                    command(vm, "test ! -e " + SHARE + "/jobs/" + slot["previous"])
            command(
                mac,
                "sudo -u pulseworker sh -c "
                + shlex.quote("echo macos >" + workspace + "/mac-marker"),
            )
            assert (
                command(linux, "cat " + workspace + "/mac-marker").strip() == b"macos"
            )
            command(
                linux,
                "sudo -u pulseworker sh -c "
                + shlex.quote("echo linux >" + workspace + "/linux-marker"),
            )
            assert (
                command(mac, "cat " + workspace + "/linux-marker").strip() == b"linux"
            )
            h.report.setdefault("workspace_progress", []).append(
                {
                    "slot": slot["index"],
                    "generation": generation,
                    "lifecycle": h.a.lifecycle,
                    "primary_boot_identity": boot,
                    "template_boot_identity": slot["boot"],
                    "suspended_resume_verified": s["suspendable"]
                    and boot == slot["boot"],
                    "shared_host_primary_engine_markers_verified": True,
                }
            )
            h.save()
            command(
                linux,
                "sudo docker run -d --name identical-container -p 127.0.0.1:18080:8080 python:3.13-alpine python -m http.server 8080",
            )
            command(linux, "sudo docker pull alpine:3.22")
            fixture(mac)
            mac.update(base=slot["base"], workspace=workspace, engine=linux)
            h.persist(mac)

            def run_job(primary, engine):
                try:
                    h.run_job(primary)
                except Exception as error:
                    h.result("macos_c2j_workspace", "failed", str(error))
                    raise
                log = (h.OUT / (primary["spec"]["name"] + "-contract.log")).read_text()
                if (
                    "WORKSPACE=" + workspace + "/tmp/recipe-worktree-" not in log
                    or "TMPDIR=" + workspace + "/tmp\n" not in log
                ):
                    raise RuntimeError(
                        "real c2j checkout or TMPDIR escaped shared allocation"
                    )
                if (job / "tmp/container-temp").read_text().strip() != "temporary":
                    raise RuntimeError(
                        "host did not observe c2j Docker temp bind write"
                    )
                assert (
                    command(engine, "cat " + workspace + "/tmp/container-temp").strip()
                    == b"temporary"
                )

            h.mac_relay(linux, mac=mac, on_ready=run_job, task_user="pulseworker")
            # Detailed native/shared comparisons are diagnostic, not fake full semantics passes.
            program = (h.ROOT / "scripts/docker-prototype/filesystem.py").read_bytes()
            for osname, vm in [("macos", mac), ("linux", linux)]:
                for kind, path in [
                    ("shared", workspace + "/fs-" + osname),
                    ("native", "/var/tmp/" + prefix + "-fs"),
                ]:
                    data = h.guest(
                        vm, "sudo -u pulseworker /usr/bin/python3 - " + path, program
                    )
                    (
                        h.OUT / (prefix + "-" + osname + "-" + kind + "-fs.json")
                    ).write_bytes(data)
            for name in (
                "macos-relay.log",
                "macos-relay-network.json",
                "macos-relay-host.log",
                "macos-testcontainers-go.log",
                "engine-tunnel.log",
            ):
                path = h.OUT / name
                if path.exists():
                    shutil.copy2(path, h.OUT / (prefix + "-" + name))
            h.report["samples"].append(
                {
                    "slot": slot["index"],
                    "generation": generation,
                    "macos_shared_job_seconds": time.monotonic() - begin,
                    "primary_ssh_seconds": primary_ssh_seconds,
                    "lifecycle": h.a.lifecycle,
                    "boot_identity": boot,
                }
            )
            h.save()
        finally:
            # Never clear files while a previous job or daemon can still read them.
            for vm in (mac, linux):
                if vm is not None:
                    try:
                        if vm is mac:
                            (h.OUT / (prefix + "-fixture.log")).write_bytes(
                                h.guest(
                                    vm,
                                    "cat /var/tmp/pulse-fixture.log 2>/dev/null || true",
                                )
                            )
                    except Exception:
                        pass
                    h.cleanup(vm)
            if all(vm is None or vm.get("cleaned") for vm in (mac, linux)):
                clear_job(job, slot["root"], slot["inode"])
        slot["previous"] = prefix

    try:
        for index in range(h.a.slots):
            root = h.PRIVATE / ("slot-%d" % index)
            root.mkdir(mode=0o777)
            root.chmod(0o777)
            (root / "jobs").mkdir(mode=0o777)
            (root / "jobs").chmod(0o777)
            slot = {"index": index, "root": root, "inode": root.stat().st_ino}
            slots.append(slot)
            s = new_spec(h.owner + "-template-%d" % index, slot, True)
            s["suspendable"] = h.a.lifecycle == "suspended"
            template = h.create(s)
            templates.append(template)
            slot["template"] = template
            prepare_mac(template)
            version = command(template, "sw_vers -productVersion").decode().strip()
            slot["base"] = "macos-tahoe-" + version + "-arm64"
            slot["boot"] = (
                command(template, "sysctl -n kern.bootsessionuuid").decode().strip()
            )
            h.tart("suspend" if s["suspendable"] else "stop", s["name"])
            expected = "suspended" if s["suspendable"] else "stopped"
            h.wait(
                "prepared macOS " + expected,
                lambda: h.native(template)["State"] == expected,
                90,
            )
        for generation in range(h.a.rounds):
            print(
                "macOS shared-workspace generation",
                generation + 1,
                "of",
                h.a.rounds,
                flush=True,
            )
            # Local helper/report writes are serialized, but all pairs are not
            # claimed concurrent until the harness supports parallel callbacks.
            for slot in slots:
                round_slot(slot, generation)
        h.result(
            "macos_c2j_workspace",
            "passed",
            "Real macOS supplied leases; ordinary task user; shared checkout/TMPDIR; native Docker CLI Buildx/Compose and container writes observed by host and companion.",
        )
        h.result(
            "macos_shared_mounts",
            "passed",
            "Stable slot root, same absolute path in both guests, real c2j/container bidirectional writes.",
        )
        h.result(
            "stable_workspace_generations",
            "passed",
            "Fresh primary and engine per job; old guests deleted before child removal; next generation sees no prior job directory; root inode retained.",
            slots=h.a.slots,
            generations=h.a.rounds,
        )
        if h.a.lifecycle == "suspended":
            h.result(
                "macos_shared_resume",
                "passed",
                "Distinct slot templates; clones retain original boot identity and attached stable share; real c2j jobs after resume.",
                slots=h.a.slots,
                generations=h.a.rounds,
            )
    finally:
        for vm in templates:
            h.cleanup(vm)
