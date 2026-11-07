#!/bin/bash
# Full deterministic suite plus real Docker library jobs, sequentially on Linux
# and macOS. At most one macOS primary and one Linux companion run at a time.

# Parse the complete driver before execution so edits cannot change an active run.
main() {
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
backend=${1:-tart}
platform=${2:-both}
[[ $# -le 2 && ( $backend == tart || $backend == orchard ) && ( $platform == both || $platform == linux || $platform == darwin ) ]] || { echo 'Usage: test-macos-docker-library.sh [tart|orchard] [both|linux|darwin]'  >&2; exit 2; }
if [[ $backend == tart ]]; then
 [[ $(uname -s)/$(uname -m) == Darwin/arm64 ]] || { echo 'Run native Tart tests on an Apple Silicon Mac.' >&2; exit 2; }
 export PULSE_TEST_ENGINE_WORKER_ADDRESS=${PULSE_TEST_ENGINE_WORKER_ADDRESS:-192.168.64.1}
else
 : "${PULSE_TEST_ORCHARD_URL:?Set the Orchard URL}"
 : "${PULSE_TEST_ORCHARD_WORKER:?Set the Orchard worker}"
 : "${PULSE_TEST_ENGINE_WORKER_ADDRESS:?Set the worker IP reachable from its guests}"
fi
relative="test-results/$(date -u +%Y%m%dT%H%M%SZ)-docker-library-$backend-$$"
out="$PWD/$relative"
mkdir -p "$out"
python3 scripts/test-report.py snapshot "$out"
{
 date -u
 git rev-parse HEAD
 go version
 if command -v tart >/dev/null; then tart --version; fi
 uname -sm
} > "$out/environment.log" 2>&1
guests=(linux darwin)
if [[ $platform != both ]]; then guests=("$platform"); fi
printf 'Docker library suite: %s; platforms: %s; logs: %s\n' "$backend" "${guests[*]}" "$relative"
cd "$out/source"
export GOWORK=off PULSE_TEST_DOCKER_LIBRARY="$backend" PULSE_TEST_OUTPUT_DIR="$out"
unset PULSE_TEST_BACKEND
status=0
# Include script/prototype regressions in the frozen snapshot as well.
if ! python3 -m unittest discover -s scripts -p 'test_*test.py' > "$out/python.log" 2>&1; then status=2; fi
if ! python3 -m unittest discover -s scripts/docker-prototype -p 'test*.py' >> "$out/python.log" 2>&1; then status=2; fi
for guest_os in "${guests[@]}"; do
 export PULSE_TEST_DOCKER_OS="$guest_os"
 printf 'Testing %s through the public library; fresh image preparation may take several minutes.\n' "$guest_os"
 set +e
 go test -race -json -count=1 -timeout=45m ./... 2>&1 | tee "$out/$guest_os-tests.jsonl" | python3 scripts/test-report.py stream "$out/$guest_os.log"
 codes=("${PIPESTATUS[@]}")
 set -e
 for code in "${codes[@]}"; do if [[ $code != 0 ]]; then status=2; fi; done
 if ! grep -q "DOCKER_LIBRARY_JOBDB_PASS: $guest_os;" "$out/$guest_os.log"; then status=2; fi
 # Avoid stacking new infrastructure on top of an unresolved cleanup failure.
 if [[ $status != 0 ]]; then break; fi
done
python3 - "$out" "$status" "$backend" "$platform" <<'PY'
import json,pathlib,sys
out=pathlib.Path(sys.argv[1]);(out/'report.json').write_text(json.dumps({'scope':'production-library-'+sys.argv[4],'backend':sys.argv[3],'result':'passed' if sys.argv[2]=='0' else 'incomplete','production_qualified':False,'remaining_live_gates':['native concurrent production allocator','fault injection','resource pressure']},indent=2)+'\n')
PY
tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
printf 'Report: %s/report.json Result: ' "$relative"
if [[ $status == 0 ]]; then echo passed; else echo incomplete; fi
printf 'Send: %s.tar.gz\n' "$relative"
exit "$status"
}
main "$@"
