#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
version=latest
positional=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --c2j) [[ $# -ge 2 ]] || { echo 'Missing --c2j value.' >&2; exit 2; }; version=$2; shift 2 ;;
    --help|-h) echo 'Usage: build-bundle.sh [OUTPUT] [default|integration|docker|docker-integration] [--c2j latest|TAG|COMMIT]'; exit 0 ;;
    --*) echo "Unknown option: $1" >&2; exit 2 ;;
    *) positional+=("$1"); shift ;;
  esac
done
[[ ${#positional[@]} -le 2 ]] || { echo 'Too many arguments.' >&2; exit 2; }
out=${positional[0]:-dist/guest-bundle.tar.gz}
mkdir -p "$(dirname "$out")"
python3 - "$out" "${positional[1]:-default}" "$version" <<'PY'
import gzip,io,tarfile,sys,subprocess,tempfile
from pathlib import Path
files={'bootstrap-guest.sh':Path('scripts/bootstrap-guest.sh'),'flake.nix':Path('flake.nix'),'flake.lock':Path('flake.lock'),'tests/integration/c2j-fixture.go.txt':Path('tests/integration/c2j-fixture.go.txt'),'tests/integration/docker-fixture.go.txt':Path('tests/integration/docker-fixture.go.txt')}
files.update({name:Path('scripts')/name for name in ['install-guest.sh','prepare-image.sh','prepare-template.sh','prepare-base.sh','prepare-base-linux.sh','docker-engine.sh','docker-primary.sh','select-c2j.sh','resolve-c2j-vendor.sh']})
files.update({'guest/'+p.name:p for p in Path('guest').iterdir() if p.suffix=='.go' or p.name in ('go.mod.txt','go.sum.txt')})
assert sys.argv[2] in ('default','integration','docker','docker-integration')
with tempfile.TemporaryDirectory(prefix='pulse-bundle-') as temp:
 root=Path(temp)
 for name,p in files.items():
  target=root/name;target.parent.mkdir(parents=True,exist_ok=True);target.write_bytes(p.read_bytes())
 (root/'profile').write_text(sys.argv[2]+'\n')
 subprocess.run(['bash',str(root/'select-c2j.sh'),str(root),sys.argv[3]],check=True)
 with open(sys.argv[1],'wb') as f, gzip.GzipFile(filename="",fileobj=f,mode='wb',mtime=0) as g, tarfile.open(fileobj=g,mode='w|') as t:
  for p in sorted(root.rglob('*')):
   if p.is_file():
    b=p.read_bytes();info=tarfile.TarInfo(str(p.relative_to(root)));info.size=len(b);info.mode=0o644;info.mtime=0;t.addfile(info,io.BytesIO(b))
PY
