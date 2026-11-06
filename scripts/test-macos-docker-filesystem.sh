#!/bin/bash
# Small coherence reproduction: no Docker CLI/c2j builds or leases; Engine installed only if guest sharing passes.
set -euo pipefail
if [[ $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Run this script on the Apple Silicon Mac with Tart installed.' >&2
  exit 2
fi
exec "$(dirname "$0")/test-docker-prototype.sh" tart --filesystem-only "$@"
