#!/bin/bash
# One invocation: production allocator concurrency, failures, pressure, and reuse.
main() {
set -euo pipefail
umask 077
cd "$(dirname "$0")/.."
[[ $# == 0 ]] || { echo 'Usage: scripts/test-orchard-docker-qualification.sh (no arguments)' >&2; exit 2; }
: "${PULSE_TEST_ORCHARD_URL:?Set the Orchard URL}"
: "${PULSE_TEST_ORCHARD_WORKER:?Set the worker name}"
: "${PULSE_TEST_ENGINE_WORKER_ADDRESS:?Set the worker address reachable from guests}"
relative="test-results/$(date -u +%Y%m%dT%H%M%SZ)-orchard-docker-qualification-$$"
out="$PWD/$relative"
mkdir -p "$out"
python3 scripts/test-report.py snapshot "$out"
{ date -u; git rev-parse HEAD; go version; uname -sm; } > "$out/environment.log" 2>&1
printf 'Docker qualification: at most two macOS VMs plus two Linux VMs / 16 vCPUs / 32 GiB.\nAllow 30–60 minutes plus downloads (100-minute test limit). Do not run another VM suite concurrently.\nLogs: %s\n' "$relative"
cd "$out/source"
export GOWORK=off PULSE_TEST_OUTPUT_DIR="$out" PULSE_TEST_QUALIFICATION_BACKEND=orchard
unset PULSE_TEST_BACKEND PULSE_TEST_DOCKER_LIBRARY PULSE_TEST_DOCKER_QUALIFICATION PULSE_TEST_DOCKER_ENDPOINT PULSE_QUALIFICATION_CONTROLLER
status=0
# Keep evidence/archives even when a host precondition fails. Do not stop others' VMs.
if ! python3 scripts/docker-qualification/orchard-preflight.py "$out" > "$out/host-preflight.log" 2>&1; then
 status=2
 cat "$out/host-preflight.log" >&2
fi
if ! python3 -m unittest discover -s scripts -p 'test_*test.py' > "$out/python.log" 2>&1; then status=2; fi
if ! python3 -m unittest discover -s scripts/docker-prototype -p 'test*.py' >> "$out/python.log" 2>&1; then status=2; fi
if [[ $status != 0 ]]; then tail -n 80 "$out/python.log" >&2; fi
if [[ $status == 0 ]]; then
 set +e
 PULSE_TEST_DOCKER_QUALIFICATION=1 go test -race -json -count=1 -timeout=100m ./... 2>&1 | tee "$out/tests.jsonl" | python3 scripts/test-report.py stream "$out/tests.log"
 codes=("${PIPESTATUS[@]}")
 set -e
 for code in "${codes[@]}"; do if [[ $code != 0 ]]; then status=2; fi; done
fi
if ! python3 scripts/docker-qualification/report.py "$out" "$status" orchard; then status=2; fi
tar -czf "$out.tar.gz" -C "$(dirname "$out")" "$(basename "$out")"
printf 'Report: %s/report.json Result: ' "$relative"
if [[ $status == 0 ]]; then echo passed; else echo incomplete; fi
printf 'Send: %s.tar.gz\n' "$relative"
exit "$status"
}
main "$@"
