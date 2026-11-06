#!/bin/bash
# Native macOS tools, compiled from upstream source; no Docker Desktop required.
set -euo pipefail
out=${1:?output directory required}
mkdir -p "$out"; out=$(cd "$out" && pwd)
work=$(mktemp -d /tmp/pulse-docker-clients.XXXXXX)
trap 'rm -rf "$work"' EXIT
build() {
 local repo=$1 version=$2 package=$3 binary=$4 flags=$5
 local dir="$work/$repo"
 mkdir -p "$dir"
 curl --fail --location --retry 3 --max-time 300 "https://api.github.com/repos/docker/$repo/tarball/$version" -o "$work/$repo.tgz"
 tar -xzf "$work/$repo.tgz" --strip-components=1 -C "$dir"
 if [[ $repo == cli ]]; then
   cp "$dir/vendor.mod" "$dir/go.mod"
   cp "$dir/vendor.sum" "$dir/go.sum"
 fi
 (cd "$dir" && GOWORK=off CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags "$flags" -o "$out/$binary" "$package")
}
build cli v28.5.1 ./cmd/docker docker-darwin '-X github.com/docker/cli/cli/version.Version=28.5.1'
build buildx v0.30.1 ./cmd/buildx docker-buildx-darwin '-X github.com/docker/buildx/version.Version=v0.30.1'
# Compose's Darwin filesystem watcher requires cgo. Use the upstream release
# and verify its published checksum rather than cross-compiling with cgo off.
curl --fail --location --retry 3 --max-time 300 https://github.com/docker/compose/releases/download/v2.40.3/docker-compose-darwin-aarch64 -o "$out/docker-compose-darwin"
curl --fail --location --retry 3 --max-time 60 https://github.com/docker/compose/releases/download/v2.40.3/docker-compose-darwin-aarch64.sha256 -o "$work/compose.sha256"
python3 - "$out/docker-compose-darwin" "$work/compose.sha256" <<'PYVERIFY'
import hashlib,pathlib,sys
actual=hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest()
expected=pathlib.Path(sys.argv[2]).read_text().split()[0]
if actual != expected: raise SystemExit('Compose release checksum mismatch')
PYVERIFY
chmod 755 "$out/docker-compose-darwin"
python3 - "$out" <<'PY'
import hashlib,json,pathlib,sys
out=pathlib.Path(sys.argv[1])
r={name:{'version':version,'sha256':hashlib.sha256((out/name).read_bytes()).hexdigest()} for name,version in [('docker-darwin','28.5.1'),('docker-buildx-darwin','0.30.1'),('docker-compose-darwin','2.40.3')]}
(out/'docker-clients.json').write_text(json.dumps(r,indent=2)+'\n')
PY
