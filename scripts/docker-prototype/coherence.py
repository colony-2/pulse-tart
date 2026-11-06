"""Small read-after-write reproduction, independent of Docker/c2j setup."""

import hashlib
import importlib.util
import json
import pathlib
import shlex
import time
from types import SimpleNamespace

MAC_IMAGE = "ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77"
ROOT = "/Users/Shared/pulse/work/coherence"


def run_coherence(environment):
    h = SimpleNamespace(**environment)
    private = h.PRIVATE / "coherence-share"
    private.mkdir(mode=0o777)
    private.chmod(0o777)
    directory = private / "coherence"
    directory.mkdir(mode=0o777)
    directory.chmod(0o777)
    vms = {}
    evidence = {
        "schema": 1,
        "scope": "synthetic guest/container shared-file consistency; no leases or c2j",
        "container_observations": [],
        "observations": [],
        "remount_diagnostics": [],
        "mounts": {},
        "guest_versions": {},
    }
    module_spec = importlib.util.spec_from_file_location(
        "coherence_probe", h.ROOT / "scripts/docker-prototype/coherence-probe.py"
    )
    module = importlib.util.module_from_spec(module_spec)
    module_spec.loader.exec_module(module)

    def save():
        (h.OUT / "filesystem-coherence.json").write_text(
            json.dumps(evidence, indent=2) + "\n"
        )

    def op(role, request):
        return json.loads(
            h.guest(
                vms[role],
                "sudo -u pulseworker /usr/bin/python3 /opt/pulse-docker/coherence-probe.py",
                json.dumps(request).encode(),
                timeout=30,
            )
        )

    try:
        for role in ("macos", "linux"):
            s = (
                h.spec(h.owner + "-fs-" + role, "darwin", MAC_IMAGE)
                if role == "macos"
                else h.spec(h.owner + "-fs-linux")
            )
            s["hostDirs"] = [{"name": "work", "path": str(private), "ro": False}]
            vms[role] = h.create(s)
        for role, vm in vms.items():
            h.wait(role + " filesystem SSH", lambda: h.guest(vm, "true") == b"", 180)
            h.guest(vm, "sudo mkdir -p /opt/pulse-docker")
            h.upload(
                vm,
                "/opt/pulse-docker/coherence-probe.py",
                (h.ROOT / "scripts/docker-prototype/coherence-probe.py").read_bytes(),
            )
            if role == "macos":
                h.upload(
                    vm,
                    "/opt/pulse-docker/prepare-macos.sh",
                    (h.ROOT / "scripts/docker-prototype/prepare-macos.sh").read_bytes(),
                )
                h.guest(
                    vm,
                    "set -eu; sudo bash /opt/pulse-docker/prepare-macos.sh --user-only; sudo umount '/Volumes/My Shared Files' 2>/dev/null || true; sudo mkdir -p /Users/Shared/pulse; sudo mount_virtiofs com.apple.virtio-fs.automount /Users/Shared/pulse",
                )
                versions = "sw_vers; uname -a; /usr/bin/python3 --version"
            else:
                h.guest(
                    vm,
                    "set -eu; id pulseworker >/dev/null 2>&1 || sudo useradd --uid 55050 --create-home --shell /bin/bash pulseworker; sudo mkdir -p /Users/Shared/pulse; sudo mount -t virtiofs com.apple.virtio-fs.automount /Users/Shared/pulse",
                )
                versions = "uname -a; cat /etc/os-release; /usr/bin/python3 --version"
            evidence["mounts"][role] = h.guest(vm, "mount").decode()
            evidence["guest_versions"][role] = h.guest(vm, versions).decode()
        # Reader-origin recreates the admission failure: macOS writes/fsyncs,
        # Linux reads and overwrites, then macOS reopens the same file.
        for reader, writer in [("macos", "linux"), ("linux", "macos")]:
            for origin in (reader, writer):
                for variant, method, value in [
                    ("same_size_close", "close", "B" * 32),
                    ("grow_close", "close", "B" * 64),
                    ("grow_fsync", "fsync", "B" * 64),
                    ("atomic_replace", "atomic", "B" * 64),
                ]:
                    leaf = "probe-%02d" % len(evidence["observations"])
                    path = ROOT + "/" + leaf
                    row = {
                        "reader": reader,
                        "writer": writer,
                        "origin": origin,
                        "variant": variant,
                        "file": leaf,
                    }
                    print(
                        "Filesystem:",
                        reader,
                        "reads",
                        writer,
                        "writes;",
                        variant,
                        "origin=" + origin,
                        flush=True,
                    )
                    op(
                        origin,
                        {
                            "op": "write",
                            "path": path,
                            "value": "A" * 32,
                            "method": "fsync",
                        },
                    )
                    row["reader_before"] = op(reader, {"op": "read", "path": path})
                    row["writer_before"] = op(writer, {"op": "read", "path": path})
                    initial = hashlib.sha256(b"A" * 32).hexdigest()
                    row["baseline_valid"] = all(
                        row[k].get("sha256") == initial
                        for k in ("reader_before", "writer_before")
                    )
                    started = time.monotonic()
                    op(
                        writer,
                        {"op": "write", "path": path, "value": value, "method": method},
                    )
                    row["host_at_writer_return"] = module.snapshot(directory / leaf)
                    row["host_read_seconds"] = time.monotonic() - started
                    row["reader_after"] = op(
                        reader, {"op": "observe", "path": path, "expected": value}
                    )
                    row["reader_check_completed_seconds"] = time.monotonic() - started
                    row["host_after"] = module.snapshot(directory / leaf)
                    row["writer_after"] = op(writer, {"op": "read", "path": path})
                    expected = hashlib.sha256(value.encode()).hexdigest()
                    row["status"] = (
                        "passed"
                        if row["baseline_valid"]
                        and row["reader_after"]["immediate_match"]
                        and row["host_at_writer_return"].get("sha256") == expected
                        and row["host_after"].get("sha256") == expected
                        and row["writer_after"].get("sha256") == expected
                        else "failed"
                    )
                    evidence["observations"].append(row)
                    save()
        # Remount is a diagnostic only. Never change a failed initial observation
        # to passed because retrying or remounting eventually exposes the bytes.
        for role, vm in vms.items():
            failures = [
                r
                for r in evidence["observations"]
                if r["reader"] == role and r["status"] == "failed"
            ]
            if not failures:
                continue
            row = failures[-1]
            command = "set -eu; sudo umount /Users/Shared/pulse; "
            command += (
                "sudo mount_virtiofs" if role == "macos" else "sudo mount -t virtiofs"
            ) + " com.apple.virtio-fs.automount /Users/Shared/pulse"
            diagnostic = {
                "reader": role,
                "file": row["file"],
                "failed_variant": row["variant"],
            }
            try:
                h.guest(vm, command)
                diagnostic["after_remount"] = op(
                    role, {"op": "read", "path": ROOT + "/" + row["file"]}
                )
            except Exception as error:
                diagnostic["error"] = str(error)
            evidence["remount_diagnostics"].append(diagnostic)
            save()
        passed = all(row["status"] == "passed" for row in evidence["observations"])
        h.result(
            "shared_file_coherence",
            "passed" if passed else "failed",
            "See filesystem-coherence.json. First reads decide success; delayed reads/remounts are diagnostic only.",
            samples=len(evidence["observations"]),
        )
        if not passed:
            h.result(
                "shared_container_coherence",
                "not_run",
                "Plain guest read-after-write already failed; Docker setup omitted. This is not a passing result.",
            )
        else:
            # Only install Engine if plain guest sharing works. No macOS Docker
            # clients, relay, c2j or JobDB fixture are built for this reproduction.
            print(
                "Plain guest checks passed; checking writes from a private Linux container",
                flush=True,
            )
            linux = vms["linux"]
            h.upload(
                linux,
                "/opt/pulse-docker/prepare-linux.sh",
                (h.ROOT / "scripts/docker-prototype/prepare-linux.sh").read_bytes(),
            )
            h.guest(
                linux,
                "sudo systemd-run --unit=pulse-coherence-docker /bin/bash /opt/pulse-docker/prepare-linux.sh",
            )
            h.wait(
                "filesystem Docker preparation",
                lambda: (
                    b"PULSE_DOCKER_PREPARED"
                    in h.guest(
                        linux, "sudo journalctl -u pulse-coherence-docker --no-pager"
                    )
                ),
                900,
            )
            h.guest(linux, "sudo docker pull alpine:3.22", timeout=180)
            for mode in ("new_file", "overwrite"):
                leaf = "probe-container-" + mode
                path = ROOT + "/" + leaf
                if mode == "overwrite":
                    op(
                        "macos",
                        {
                            "op": "write",
                            "path": path,
                            "value": "A" * 32,
                            "method": "fsync",
                        },
                    )
                    op("macos", {"op": "read", "path": path})
                script = "printf '%s' 'container' > /work/" + leaf
                if mode == "overwrite":
                    script = (
                        'test "$(cat /work/' + leaf + ')" = ' + "A" * 32 + "; " + script
                    )
                h.guest(
                    linux,
                    "sudo docker run --rm -v "
                    + ROOT
                    + ":/work alpine:3.22 sh -ec "
                    + shlex.quote(script),
                    timeout=60,
                )
                row = {
                    "variant": mode,
                    "file": leaf,
                    "host_at_writer_return": module.snapshot(directory / leaf),
                }
                row["reader_after"] = op(
                    "macos", {"op": "observe", "path": path, "expected": "container"}
                )
                row["status"] = (
                    "passed"
                    if row["reader_after"]["immediate_match"]
                    and row["host_at_writer_return"].get("sha256")
                    == hashlib.sha256(b"container").hexdigest()
                    else "failed"
                )
                evidence["container_observations"].append(row)
                save()
            h.result(
                "shared_container_coherence",
                "passed"
                if all(
                    r["status"] == "passed" for r in evidence["container_observations"]
                )
                else "failed",
                "Real private Linux container creation/overwrite, ordinary bind mount, immediate macOS reads. See filesystem-coherence.json.",
            )

    finally:
        save()
        for vm in vms.values():
            h.cleanup(vm)
