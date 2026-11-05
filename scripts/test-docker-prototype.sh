#!/bin/bash
# Explicitly experimental. Exit 2 means failed/incomplete requested live coverage.
set -euo pipefail
cd "$(dirname "$0")/.."
backend=${1:-tart}
[[ $backend == tart || $backend == orchard ]] || { echo 'Usage: test-docker-prototype.sh [tart|orchard]' >&2; exit 2; }
if (($#)); then shift; fi
out="$PWD/test-results/$(date -u +%Y%m%dT%H%M%SZ)-docker-$backend-$$"
mkdir -p "$out"
python3 - "$out" <<'PYTHON'
import hashlib,json,pathlib,shutil,sys
root=pathlib.Path.cwd();out=pathlib.Path(sys.argv[1]);source=out/'source';source.mkdir()
for name in ('go.mod','go.sum'): shutil.copy2(root/name,source/name)
for name in ('experimental/docker','scripts/docker-prototype'):
    shutil.copytree(root/name,source/name,ignore=shutil.ignore_patterns('__pycache__','target'))
files={str(p.relative_to(source)):hashlib.sha256(p.read_bytes()).hexdigest() for p in source.rglob('*') if p.is_file()}
(out/'source-sha256.json').write_text(json.dumps(files,indent=2)+'\n')
PYTHON
cd "$out/source"
tools=$(mktemp -d /tmp/pulse-docker-tools.XXXXXX)
./scripts/docker-prototype/build-tools.sh "$tools" "${PULSE_TEST_C2J_VERSION:-latest}" > "$out/build.log" 2>&1 || {
  python3 - "$out" <<'PYTHON'
import json,pathlib,sys
out=pathlib.Path(sys.argv[1]);cases=json.loads(pathlib.Path("scripts/docker-prototype/experiments.json").read_text())
(out/"report.json").write_text(json.dumps({"result":"incomplete","build_error":"See build.log","cases":{k:{"status":"not_run","reason":"Tool build failed"} for k in cases}},indent=2))
PYTHON
  tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
  echo "Build failed. Send: $out.tar.gz"
  exit 2
}
set +e
python3 scripts/docker-prototype/run.py "$backend" --tools "$tools" --output "$out" "$@" 2>&1 | tee "$out/run.log"
status=${PIPESTATUS[0]}
set -e
tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
echo "Send: $out.tar.gz"
exit "$status"
