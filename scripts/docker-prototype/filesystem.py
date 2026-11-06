#!/usr/bin/env python3
"""Measured filesystem primitives; cross-guest visibility is a separate test."""

import fcntl, json, os, pathlib, platform, subprocess, sys, tempfile, time

root = pathlib.Path(sys.argv[1])
root.mkdir(parents=True, exist_ok=True)
out = {
    "root": str(root),
    "platform": platform.platform(),
    "uid": os.getuid(),
    "gid": os.getgid(),
    "checks": {},
    "timings": {},
}


def check(name, fn):
    start = time.monotonic()
    try:
        value = fn()
        out["checks"][name] = {"status": "passed", "value": value}
    except Exception as e:
        out["checks"][name] = {
            "status": "failed",
            "reason": type(e).__name__ + ": " + str(e),
        }
    out["checks"][name]["seconds"] = time.monotonic() - start


def files():
    d = root / "space directory"
    d.mkdir(exist_ok=True)
    f = d / "file"
    f.write_text("old")
    f.chmod(0o755)
    os.rename(f, d / "renamed")
    os.symlink("renamed", d / "link")
    assert (d / "link").read_text() == "old"
    return oct((d / "renamed").stat().st_mode & 0o777)


check("directories_spaces_rename_symlink_chmod", files)


def case():
    upper, lower = root / "Case", root / "case"
    upper.write_text("upper")
    # Existence/identity avoids mistaking the guest page cache for case sensitivity.
    existed = lower.exists()
    lower.write_text("lower")
    same = os.path.samefile(upper, lower)
    return {
        "case_sensitive": not same,
        "lower_existed_before_create": existed,
        "same_inode": same,
        "upper_read": upper.read_text(),
        "lower_read": lower.read_text(),
    }


check("case_sensitive", case)


def lock():
    child = """import errno,fcntl,sys
with open(sys.argv[1], 'a') as f:
    try:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as e:
        if e.errno in (errno.EAGAIN, errno.EACCES):
            sys.exit(73)
        raise
"""
    with open(root / "lock", "a") as f:
        fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        p = subprocess.run(
            [sys.executable, "-c", child, str(root / "lock")],
            capture_output=True,
            timeout=10,
        )
        if p.returncode != 73:
            raise RuntimeError(
                "contender exit=%d (73=blocked, 0=acquired): %s"
                % (p.returncode, p.stderr.decode(errors="replace"))
            )
    p = subprocess.run(
        [sys.executable, "-c", child, str(root / "lock")],
        capture_output=True,
        timeout=10,
    )
    if p.returncode != 0:
        raise RuntimeError(
            "lock was not acquirable after release: "
            + p.stderr.decode(errors="replace")
        )
    return "contender blocked while locked, acquired after release"


check("same_guest_flock", lock)


def xattr():
    f = root / "case"
    if platform.system() == "Darwin":
        subprocess.run(
            ["/usr/bin/xattr", "-w", "dev.pulse.prototype", "value", str(f)],
            check=True,
            capture_output=True,
        )
        value = subprocess.check_output(
            ["/usr/bin/xattr", "-p", "dev.pulse.prototype", str(f)]
        ).strip()
        assert value == b"value", "xattr did not round-trip"
        return "dev.pulse.prototype"
    os.setxattr(f, "user.pulse", b"value")
    assert os.getxattr(f, "user.pulse") == b"value", "xattr did not round-trip"
    return "user.pulse"


check("user_xattr", xattr)


def chown():
    f = root / "case"
    os.chown(f, os.getuid(), os.getgid())
    return [f.stat().st_uid, f.stat().st_gid]


check("same_user_chown", chown)
start = time.monotonic()
tree = root / "tree"
tree.mkdir(exist_ok=True)
for i in range(1000):
    (tree / str(i)).write_bytes(b"x" * 4096)
out["timings"]["write_1000_4k_files_seconds"] = time.monotonic() - start
start = time.monotonic()
assert sum(len(p.read_bytes()) for p in tree.iterdir()) == 4096000
out["timings"]["read_1000_4k_files_seconds"] = time.monotonic() - start
repo = root / "repo"
repo.mkdir(exist_ok=True)
start = time.monotonic()
for args in [
    ["init"],
    ["config", "user.email", "test@example.invalid"],
    ["config", "user.name", "Prototype"],
]:
    subprocess.run(["git", *args], cwd=repo, check=True, capture_output=True)
(repo / "source.py").write_text('print("built")\n')
subprocess.run(["git", "add", "."], cwd=repo, check=True)
subprocess.run(
    ["git", "-c", "commit.gpgsign=false", "commit", "-m", "fixture"],
    cwd=repo,
    check=True,
    capture_output=True,
)
subprocess.run(
    ["git", "clone", str(repo), str(root / "checkout")], check=True, capture_output=True
)
subprocess.run(
    [sys.executable, "-m", "compileall", "-q", str(root / "checkout")], check=True
)
out["timings"]["git_checkout_python_build_seconds"] = time.monotonic() - start
out["mounts"] = subprocess.run(["mount"], capture_output=True, text=True).stdout
print(json.dumps(out, indent=2))
