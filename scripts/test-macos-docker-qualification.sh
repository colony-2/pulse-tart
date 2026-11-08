#!/bin/bash
# One invocation: production allocator concurrency, failures, pressure, and reuse.
main() {
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
[[ $# == 0 ]] || { echo 'Usage: scripts/test-macos-docker-qualification.sh (no arguments)' >&2; exit 2; }
[[ $(uname -s)/$(uname -m) == Darwin/arm64 ]] || { echo 'Run on an Apple Silicon Mac.' >&2; exit 2; }
export PULSE_TEST_ENGINE_WORKER_ADDRESS=${PULSE_TEST_ENGINE_WORKER_ADDRESS:-192.168.64.1}
export TART_HOME=${PULSE_TEST_TART_HOME:-$HOME/.tart} TART_NO_AUTO_PRUNE=true
relative="test-results/$(date -u +%Y%m%dT%H%M%SZ)-docker-qualification-$$"
out="$PWD/$relative"
mkdir -p "$out"
python3 scripts/test-report.py snapshot "$out"
{ date -u; git rev-parse HEAD; go version; tart --version; uname -sm; } > "$out/environment.log" 2>&1
printf 'Docker qualification: at most two macOS VMs plus two Linux VMs / 16 vCPUs / 32 GiB.\nAllow 30–60 minutes plus downloads (100-minute test limit). Do not run another VM suite concurrently.\nLogs: %s\n' "$relative"
cd "$out/source"
export GOWORK=off PULSE_TEST_OUTPUT_DIR="$out"
unset PULSE_TEST_BACKEND PULSE_TEST_DOCKER_LIBRARY PULSE_TEST_DOCKER_QUALIFICATION PULSE_TEST_DOCKER_ENDPOINT PULSE_QUALIFICATION_CONTROLLER
status=0
# Keep evidence/archives even when a host precondition fails. Do not stop others' VMs.
if ! tart list --format json > "$out/before-vms.json" 2> "$out/host-preflight.log"; then status=2; fi
if ! python3 - "$out/before-vms.json" >> "$out/host-preflight.log" 2>&1 <<'PY'
import json,os,pathlib,shutil,sys
vms=json.loads(pathlib.Path(sys.argv[1]).read_text())
assert not any(v.get('State',v.get('state'))=='running' or v.get('Running',v.get('running')) for v in vms), 'Stop existing running VMs before this isolated qualification run.'
assert shutil.disk_usage(os.environ['TART_HOME']).free >= 40*2**30, 'Need at least 40 GiB free in TART_HOME.'
PY
then status=2; fi
if [[ $status != 0 ]]; then cat "$out/host-preflight.log" >&2; fi
if ! python3 -m unittest discover -s scripts -p 'test_*test.py' > "$out/python.log" 2>&1; then status=2; fi
if ! python3 -m unittest discover -s scripts/docker-prototype -p 'test*.py' >> "$out/python.log" 2>&1; then status=2; fi
if [[ $status != 0 ]]; then tail -n 80 "$out/python.log" >&2; fi
if [[ $status == 0 ]]; then
 printf 'Checking launchd before provisioning any VMs.\n'
 set +e
 PULSE_TEST_DOCKER_ENDPOINT=1 go test -race -json -count=1 -run '^TestLiveDockerEndpoint$' -timeout=90s ./internal/tart 2>&1 | tee "$out/endpoint-tests.jsonl" | python3 scripts/test-report.py stream "$out/endpoint.log"
 codes=("${PIPESTATUS[@]}")
 set -e
 for code in "${codes[@]}"; do if [[ $code != 0 ]]; then status=2; fi; done
 if ! grep -q 'DOCKER_ENDPOINT_SERVICE_PASS:' "$out/endpoint.log"; then status=2; fi
fi
if [[ $status == 0 ]]; then
 set +e
 PULSE_TEST_DOCKER_QUALIFICATION=1 go test -race -json -count=1 -timeout=100m ./... 2>&1 | tee "$out/tests.jsonl" | python3 scripts/test-report.py stream "$out/tests.log"
 codes=("${PIPESTATUS[@]}")
 set -e
 for code in "${codes[@]}"; do if [[ $code != 0 ]]; then status=2; fi; done
fi
if ! python3 scripts/docker-qualification/report.py "$out" "$status"; then status=2; fi
tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
printf 'Report: %s/report.json Result: ' "$relative"
if [[ $status == 0 ]]; then echo passed; else echo incomplete; fi
printf 'Send: %s.tar.gz\n' "$relative"
exit "$status"
}
main "$@"
