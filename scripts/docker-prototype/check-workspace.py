#!/usr/bin/python3
"""Admission round-trip; runs before handing any lease to the primary."""
import os, pathlib, subprocess, sys
mode, raw, nonce = sys.argv[1:]
root = pathlib.Path('/Users/Shared/pulse/work').resolve()
work = pathlib.Path(raw).resolve()
if root not in work.parents or work.is_symlink():
    raise RuntimeError('workspace escaped allocation root')
mounts = subprocess.check_output(['mount'], text=True)
if not any(' on /Users/Shared/pulse ' in line and 'virtiofs' in line.lower() for line in mounts.splitlines()):
    raise RuntimeError('allocation virtiofs mount is missing')
p = work / ('.admission-' + nonce)
if mode == 'write':
    with p.open('x') as f:
        f.write(nonce)
        f.flush()
        os.fsync(f.fileno())
elif mode == 'engine':
    assert p.read_text() == nonce, 'engine cannot read primary workspace'
    p.write_text('engine-' + nonce)
elif mode == 'finish':
    assert p.read_text() == 'engine-' + nonce, 'primary cannot read engine workspace'
    subprocess.run(['docker','run','--rm','-v',str(work)+':/work','alpine:3.22',
                    'sh','-ec','test "$(cat /work/.admission-'+nonce+')" = engine-'+nonce+'; echo container >/work/.container-'+nonce],check=True)
    marker = work / ('.container-' + nonce)
    assert marker.read_text().strip() == 'container', 'primary cannot read container workspace'
    marker.unlink()
    p.unlink()
else:
    raise RuntimeError('invalid admission operation')
