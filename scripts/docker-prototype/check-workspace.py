#!/usr/bin/python3
"""Admission round-trip; runs before handing any lease to the primary."""

import os, pathlib, subprocess, sys


class WorkspaceCheckError(RuntimeError):
    def __init__(self, code):
        self.code = code
        super().__init__(code)


def require_contents(path, expected, code, strip=False):
    observed = path.read_text()
    if strip:
        observed = observed.strip()
    if observed != expected:
        raise WorkspaceCheckError(code)


def main():
    mode, raw, nonce = sys.argv[1:]
    root = pathlib.Path("/Users/Shared/pulse/work").resolve()
    work = pathlib.Path(raw).resolve()
    if root not in work.parents or work.is_symlink():
        raise RuntimeError("workspace escaped allocation root")
    mounts = subprocess.check_output(["mount"], text=True)
    if not any(
        " on /Users/Shared/pulse " in line and "virtiofs" in line.lower()
        for line in mounts.splitlines()
    ):
        raise RuntimeError("allocation virtiofs mount is missing")
    p = work / (".admission-" + nonce)
    if mode == "write":
        with p.open("x") as f:
            f.write(nonce)
            f.flush()
            os.fsync(f.fileno())
    elif mode == "engine":
        require_contents(p, nonce, "EngineCannotReadPrimaryWrite")
        p.write_text("engine-" + nonce)
    elif mode == "finish":
        require_contents(p, "engine-" + nonce, "PrimaryCannotReadEngineOverwrite")
        subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "-v",
                raw + ":/work",
                "alpine:3.22",
                "sh",
                "-ec",
                'test "$(cat /work/.admission-'
                + nonce
                + ')" = engine-'
                + nonce
                + "; echo container >/work/.container-"
                + nonce,
            ],
            check=True,
        )
        marker = work / (".container-" + nonce)
        require_contents(
            marker, "container", "PrimaryCannotReadContainerWrite", strip=True
        )
        marker.unlink()
        p.unlink()
    else:
        raise RuntimeError("invalid admission operation")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Do not include file contents, argv or lease data in admission errors.
        code = (
            error.code
            if isinstance(error, WorkspaceCheckError)
            else type(error).__name__
        )
        number = getattr(error, "errno", None)
        if isinstance(number, int) and number >= 0:
            code += " errno=" + str(number)
        print("PULSE_WORKSPACE_FAILED=" + code, file=sys.stderr)
        sys.exit(1)
