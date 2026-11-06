#!/bin/bash
# Explicitly experimental. Exit 2 means failed/incomplete requested live coverage.
set -euo pipefail
cd "$(dirname "$0")/.."
backend=${1:-tart}
[[ $backend == tart || $backend == orchard ]] || { echo 'Usage: test-docker-prototype.sh [tart|orchard]' >&2; exit 2; }
if (($#)); then shift; fi
display_out="test-results/$(date -u +%Y%m%dT%H%M%SZ)-docker-$backend-$$"
out="$PWD/$display_out"
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
filesystem_only=false
for arg in "$@"; do
  if [[ $arg == --filesystem-only ]]; then filesystem_only=true; fi
done
build_tools() {
  if "$filesystem_only"; then
    go build -o "$tools/guest" ./experimental/docker/cmd/guest || return $?
    printf '%s\n' '{"tool_profile":"filesystem-only","c2j":"not built","docker":"not built"}' > "$tools/build.json"
  else
    ./scripts/docker-prototype/build-tools.sh "$tools" "${PULSE_TEST_C2J_VERSION:-latest}"
  fi
}
echo "Building test tools; log: $display_out/build.log"
build_tools > "$out/build.log" 2>&1 || {
  python3 - "$out" <<'PYTHON'
import json,pathlib,sys
out=pathlib.Path(sys.argv[1]);cases=json.loads(pathlib.Path("scripts/docker-prototype/experiments.json").read_text())
(out/"report.json").write_text(json.dumps({"result":"incomplete","build_error":"See build.log","cases":{k:{"status":"not_run","reason":"Tool build failed"} for k in cases}},indent=2))
PYTHON
  tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
  echo "Build failed. Send: $display_out.tar.gz"
  exit 2
}
set +e
python3 scripts/docker-prototype/run.py "$backend" --tools "$tools" --output "$out" --display-output "$display_out" "$@" 2>&1 | tee "$out/run.log"
status=${PIPESTATUS[0]}
set -e
tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
echo "Send: $display_out.tar.gz"
exit "$status"
