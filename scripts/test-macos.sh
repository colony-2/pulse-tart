#!/bin/bash
# Run every test, including real VM/JobDB execution, and retain shareable results.
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
backend=${1:-${PULSE_TEST_BACKEND:-tart}}
if [[ $# -gt 1 || ( $backend != tart && $backend != orchard ) ]]; then
  echo 'Usage: ./scripts/test-macos.sh [tart|orchard]' >&2
  exit 2
fi
for command in go python3 git; do command -v "$command" >/dev/null || { echo "Missing command: $command" >&2; exit 2; }; done
out="$PWD/test-results/$(date -u +%Y%m%dT%H%M%SZ)-$backend-$$"
mkdir -p "$out"
export PULSE_TEST_BACKEND="$backend"
export PULSE_TEST_OUTPUT_DIR="$out"
echo "Results: $out"
{
  date -u
  echo "Backend: $backend"
  if [[ $(uname -s) == Darwin ]]; then sw_vers; uname -m; sysctl hw.model hw.ncpu hw.memsize; else uname -sm; fi
  if command -v tart >/dev/null; then tart --version; fi
  go version
  git rev-parse HEAD
  git diff --stat
} 2>&1 | tee "$out/environment.log"
python3 scripts/test-report.py fingerprint "$out"
if [[ $backend == tart && ( $(uname -s) != Darwin || $(uname -m) != arm64 ) ]]; then
  echo 'Direct Tart live tests require an Apple Silicon Mac.' | tee "$out/preflight-error.log"
  exit 2
fi
if [[ $backend == orchard && ( -z ${PULSE_TEST_ORCHARD_URL:-} || -z ${PULSE_TEST_ORCHARD_WORKER:-} ) ]]; then
  echo 'Set PULSE_TEST_ORCHARD_URL and PULSE_TEST_ORCHARD_WORKER.' | tee "$out/preflight-error.log"
  exit 2
fi
# Do not add -run here: the full suite and the JobDB assertions must execute.
set +e
go test -json -race -v -count=1 -timeout=50m ./... 2> "$out/go-stderr.log" \
  | tee "$out/tests.jsonl" \
  | python3 scripts/test-report.py stream "$out/tests.log"
statuses=("${PIPESTATUS[@]}")
set -e
status=0
for code in "${statuses[@]}"; do if [[ $code != 0 ]]; then status=$code; break; fi; done
cat "$out/go-stderr.log"
set +e
python3 scripts/test-report.py summarize "$out" "$backend" "$status"
status=$?
set -e
# Only this output directory is archived. Private runtime journals stay outside it.
archive="$out.tar.gz"
tar -czf "$archive" -C "$(dirname "$out")" "$(basename "$out")"
echo "Send: $archive"
exit "$status"
