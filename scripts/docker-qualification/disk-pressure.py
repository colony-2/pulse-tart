#!/usr/bin/env python3
"""Fill only this disposable engine's verified 8 GiB loop filesystem."""
import errno
import json
import os
from pathlib import Path
import shutil
import subprocess

root = Path('/var/lib/docker')
mount = json.loads(subprocess.check_output(['findmnt', '--json', '--target', str(root)]))['filesystems'][0]
assert mount['target'] == str(root) and mount['fstype'] == 'ext4'
assert mount['source'].startswith('/dev/loop')
size = shutil.disk_usage(root).total
assert 7 * 2**30 < size <= 8 * 2**30, size
assert shutil.disk_usage('/').free > 10 * 2**30, 'not enough free space on engine root'
path = root / 'pulse-qualification-fill'
written = 0
created = False
try:
    # Exclusive creation: never overwrite another test's file.
    with path.open('xb', buffering=0) as f:
        created = True
        block = b'x' * 2**20
        try:
            for _ in range(8193):
                written += f.write(block)
            os.fsync(f.fileno())
            raise AssertionError('private filesystem did not reach ENOSPC')
        except OSError as error:
            if error.errno != errno.ENOSPC:
                raise
            print(json.dumps({'enospc': True, 'written_bytes': written, 'filesystem_bytes': size}), flush=True)
        # Allow the host test to observe the peer while this filesystem is full.
        import time
        time.sleep(10)
finally:
    if created:
        path.unlink()
