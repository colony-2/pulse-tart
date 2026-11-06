#!/usr/bin/python3
"""Operate only on synthetic reproduction files, returning bytes/metadata evidence."""

import hashlib
import json
import os
import pathlib
import sys
import time


def snapshot(path):
    try:
        # Read before stat: don't use a separate getattr to hide a stale first read.
        with path.open("rb") as f:
            data = f.read()
            st = os.fstat(f.fileno())
        return {
            "status": "ok",
            "length": len(data),
            "sha256": hashlib.sha256(data).hexdigest(),
            "head_hex": data[:64].hex(),
            "stat_size": st.st_size,
            "inode": st.st_ino,
            "mtime_ns": st.st_mtime_ns,
            "uid": st.st_uid,
            "gid": st.st_gid,
        }
    except OSError as e:
        return {"status": "error", "error": type(e).__name__, "errno": e.errno}


def write(path, value, method):
    data = value.encode()
    target = path.with_name(path.name + ".replacement") if method == "atomic" else path
    with target.open("wb") as f:
        f.write(data)
        if method in ("fsync", "atomic"):
            f.flush()
            os.fsync(f.fileno())
    if method == "atomic":
        os.replace(target, path)
    elif method not in ("close", "fsync"):
        raise ValueError("invalid write method")


def observe(path, expected, delay=2.0):
    digest = hashlib.sha256(expected.encode()).hexdigest()
    start = time.monotonic()
    samples = []
    for offset in (0.0, 0.1, 0.5, delay):
        if offset > time.monotonic() - start:
            time.sleep(offset - (time.monotonic() - start))
        sample = snapshot(path)
        sample["seconds"] = time.monotonic() - start
        sample["matches"] = sample.get("sha256") == digest
        samples.append(sample)
        if sample["matches"]:
            break
    return {
        "immediate_match": samples[0]["matches"],
        "expected_sha256": digest,
        "expected_length": len(expected.encode()),
        "samples": samples,
    }


def main():
    req = json.load(sys.stdin)
    path = pathlib.Path(req["path"])
    root = pathlib.Path("/Users/Shared/pulse/work/coherence")
    if path.parent != root or not path.name.startswith("probe-") or path.is_symlink():
        raise RuntimeError("invalid reproduction path")
    if req["op"] == "write":
        write(path, req["value"], req["method"])
        out = {"status": "written"}
    elif req["op"] == "read":
        out = snapshot(path)
    elif req["op"] == "observe":
        out = observe(path, req["expected"])
    else:
        raise ValueError("invalid operation")
    print(json.dumps(out))


if __name__ == "__main__":
    main()
