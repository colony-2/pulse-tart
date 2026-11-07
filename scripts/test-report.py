#!/usr/bin/env python3
"""Render Go test events and require complete live/JobDB coverage for success."""
import hashlib
import json
from pathlib import Path
import stat
import sys

ROOT_PACKAGE = "github.com/colony-2/pulse-tart"
MARKERS = ("JOBDB_VALID_PASS:", "JOBDB_REPLAY_PASS:", "JOBDB_EXPIRED_PASS:")


def source_files(root):
    files = list(root.glob("*.go")) + [root / p for p in ("go.mod", "go.sum", "flake.nix", "flake.lock", "bases.json")]
    for directory in ("internal", "guest", "scripts", "tests", "experimental"):
        files.extend(p for p in (root / directory).rglob("*")
                     if p.is_file() and "__pycache__" not in p.parts)
    return sorted(files)


def capture_source(root):
    return {p.relative_to(root): (p.read_bytes(), stat.S_IMODE(p.stat().st_mode))
            for p in source_files(root)}


def snapshot(root, destination):
    """Freeze test/build inputs, excluding runtime state and previous reports."""
    captured = capture_source(root)
    if captured != capture_source(root):
        raise RuntimeError("Source changed while capturing the snapshot; rerun the test script.")
    source = destination / "source"
    source.mkdir()  # Never merge a new snapshot into a previous run.
    for relative, (contents, mode) in captured.items():
        target = source / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(contents)
        target.chmod(mode)
    fingerprints = {str(p): hashlib.sha256(contents).hexdigest()
                    for p, (contents, _) in captured.items()}
    (destination / "source-sha256.json").write_text(json.dumps(fingerprints, indent=2) + "\n")


def summarize(events, backend, exit_code):
    markers = MARKERS + (("IMAGE_UPDATE_PASS:",) if backend == "tart" else ())
    live_passed = False
    checks = {marker.removesuffix(":"): False for marker in markers}
    failures, skipped, packages = [], [], {}
    for event in events:
        package, test, action = event.get("Package"), event.get("Test"), event.get("Action")
        if package == ROOT_PACKAGE and test == "TestLive":
            live_passed |= action == "pass"
            for marker in markers:
                checks[marker.removesuffix(":")] |= marker in event.get("Output", "")
        if action == "fail":
            failures.append({"package": package, "test": test})
        if action == "skip" and test:
            skipped.append({"package": package, "test": test})
        if not test and action in ("pass", "fail", "skip"):
            packages[package] = "no_test_files" if action == "skip" else action
    passed = exit_code == 0 and live_passed and all(checks.values()) and not failures
    return {"result": "passed" if passed else "failed", "backend": backend,
            "command_exit_code": exit_code, "live_test_passed": live_passed,
            "jobdb_checks": {k:v for k,v in checks.items() if k.startswith("JOBDB_")},
            "image_update_passed": checks.get("IMAGE_UPDATE_PASS"), "failures": failures, "skipped_tests": skipped,
            "packages": packages}


def main():
    mode, destination = sys.argv[1:3]
    path = Path(destination)
    if mode == "stream":
        with path.open("w") as log:
            for line in sys.stdin:
                event = json.loads(line)
                output = event.get("Output", "")
                log.write(output)
                log.flush()
                print(output, end="", flush=True)
    elif mode == "snapshot":
        snapshot(Path("."), path)
    elif mode == "summarize":
        events = [json.loads(line) for line in (path / "tests.jsonl").read_text().splitlines() if line.strip()]
        result = summarize(events, sys.argv[3], int(sys.argv[4]))
        (path / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
        print("Full suite and JobDB:", result["result"])
        if result["result"] != "passed":
            print("Required live execution, completion, renewal, secret, replay, expiry, or native image update checks did not all pass.")
        return 0 if result["result"] == "passed" else 1
    else:
        raise ValueError("unknown report mode")
    return 0


if __name__ == "__main__":
    sys.exit(main())
