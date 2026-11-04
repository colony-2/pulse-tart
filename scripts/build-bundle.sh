#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
out=${1:-dist/guest-bundle.tar.gz}
mkdir -p "$(dirname "$out")"
python3 - "$out" "${2:-default}" <<'PY'
import gzip,hashlib,io,tarfile,sys
from pathlib import Path
files={'bootstrap-guest.sh':Path('scripts/bootstrap-guest.sh'),'flake.nix':Path('flake.nix'),'flake.lock':Path('flake.lock'),'tests/integration/c2j-fixture.go.txt':Path('tests/integration/c2j-fixture.go.txt')}
files.update({name:Path('scripts')/name for name in ['install-guest.sh','prepare-image.sh','prepare-template.sh']})
files.update({'guest/'+p.name:p for p in Path('guest').iterdir() if p.suffix=='.go' or p.name=='go.mod.txt'})
assert sys.argv[2] in ('default','integration')
with open(sys.argv[1],'wb') as f, gzip.GzipFile(filename="",fileobj=f,mode='wb',mtime=0) as g, tarfile.open(fileobj=g,mode='w|') as t:
 # Hash all environment inputs, including the profile and installer/account setup.
 identity=hashlib.sha256()
 identity.update((sys.argv[2]+"\n").encode())
 for name,p in sorted(files.items()):
  if name in ('flake.nix','flake.lock','install-guest.sh') or name.startswith('guest/') or (sys.argv[2]=='integration' and name.startswith('tests/')):
   data=p.read_bytes();identity.update(name.encode()+b'\0'+str(len(data)).encode()+b'\0'+data)
 data=(identity.hexdigest()+"\n").encode();info=tarfile.TarInfo('environment.sha256');info.size=len(data);info.mode=0o644;t.addfile(info,io.BytesIO(data))
 info=tarfile.TarInfo("profile");data=(sys.argv[2]+"\n").encode();info.size=len(data);info.mode=0o644;t.addfile(info,io.BytesIO(data))
 for name,p in sorted(files.items()):
  b=p.read_bytes();info=tarfile.TarInfo(name);info.size=len(b);info.mode=0o644;info.mtime=0;t.addfile(info,io.BytesIO(b))
PY
