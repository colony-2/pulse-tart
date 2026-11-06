#!/bin/bash
# Current Docker capability contract: native workspaces, no job-path binds.
set -euo pipefail
if [[ $(uname -s)/$(uname -m) != Darwin/arm64 ]]; then
  echo 'Run this script on the Apple Silicon Mac with Tart installed.' >&2
  exit 2
fi
exec "$(dirname "$0")/test-docker-prototype.sh" tart --no-bind "$@"
