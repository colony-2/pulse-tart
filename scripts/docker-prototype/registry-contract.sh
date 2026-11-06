#!/bin/bash
# Disposable allocation-local registry; synthetic credentials never printed.
set -euo pipefail
tmp=$(mktemp -d "${TMPDIR:-/tmp}/registry-probe.XXXXXX")
name=pulse-registry
cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT
cd "$tmp"
password=$(python3 -c 'import secrets;print(secrets.token_hex(24))')
printf '%s\n' "$password" | docker run --rm -i httpd:2-alpine htpasswd -niB pulse > "$tmp/htpasswd"
docker create --name "$name" -p 127.0.0.1::5000 -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=probe -e REGISTRY_AUTH_HTPASSWD_PATH=/htpasswd registry:2
# Registry config is explicitly copied into its private container.
docker cp "$tmp/htpasswd" "$name:/htpasswd"
docker start "$name"
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5000/tcp") 0).HostPort}}' "$name")
for i in $(seq 1 100); do
 code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$port/v2/" || true)
 [[ $code == 401 ]] && break
 sleep .1
done
[[ $code == 401 ]]
mkdir "$tmp/client"
printf '%s\n' "$password" | docker --config "$tmp/client" login "localhost:$port" --username pulse --password-stdin
unset password
image="localhost:$port/pulse-test:probe"
docker pull alpine:3.22
docker tag alpine:3.22 "$image"
docker --config "$tmp/client" push "$image"
docker image rm "$image"
docker --config "$tmp/client" pull "$image"
test "$(docker run --rm "$image" echo registry-ok)" = registry-ok
docker image rm "$image"
docker --config "$tmp/client" logout "localhost:$port"
if docker --config "$tmp/client" pull "$image" > "$tmp/unauthorized.log" 2>&1; then
 echo 'ERROR: authenticated registry allowed pull after logout'; exit 1
fi
printf 'PULSE_REGISTRY_AUTH_OK\n'
