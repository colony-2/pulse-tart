#!/bin/bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
out=${1:?output directory required}
mkdir -p "$out";out=$(cd "$out" && pwd)
version=${2:-latest}
work=$(mktemp -d /tmp/pulse-docker-build.XXXXXX)
trap 'rm -rf "$work"' EXIT
python3 - "$version" "$out/build.json" <<'PY'
import json,sys,urllib.request,re
version=sys.argv[1]
def get(path):
    return json.load(urllib.request.urlopen('https://api.github.com/repos/colony-2/c2j/'+path,timeout=30))
if version=='latest': version=get('releases/latest')['tag_name']
assert re.fullmatch('[A-Za-z0-9][A-Za-z0-9._/-]*',version)
sha=version if re.fullmatch('[a-f0-9]{40}',version) else get('commits/'+version)['sha']
json.dump(dict(c2j_selection=version,c2j_commit=sha),open(sys.argv[2],'w'),indent=2)
PY
selected=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["c2j_selection"])' "$out/build.json")
revision=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["c2j_commit"])' "$out/build.json")
curl --fail --location --retry 3 "https://api.github.com/repos/colony-2/c2j/tarball/$revision" -o "$work/source.tar.gz"
mkdir "$work/source"
tar -xzf "$work/source.tar.gz" --strip-components=1 -C "$work/source"
mkdir -p "$work/source/cmd/pulse-docker-fixture"
cp "$root/experimental/docker/jobdb-fixture.go.txt" "$work/source/cmd/pulse-docker-fixture/main.go"
(cd "$work/source" && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X github.com/colony-2/c2j/internal/buildinfo.version=$selected -X github.com/colony-2/c2j/internal/buildinfo.commit=$revision" -o "$out/c2j" ./cmd/c2j && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$out/jobdb-fixture" ./cmd/pulse-docker-fixture)
(cd "$root" && go build -o "$out/tunnel" ./experimental/docker/cmd/tunnel && go build -o "$out/guest" ./experimental/docker/cmd/guest && go build -o "$out/allocation" ./experimental/docker/cmd/allocation)

(cd "$root" && GOOS=darwin GOARCH=arm64 go build -o "$out/relay-darwin" ./experimental/docker/cmd/relay)
(cd "$root/experimental/docker/testcontainers-go" && GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o "$out/testcontainers-go" . && GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o "$out/testcontainers-darwin" .)
