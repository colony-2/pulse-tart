#!/bin/bash
# Supported no-bind contract, executed by the real c2j task user.
set -euo pipefail
exec > >(tee /var/tmp/pulse-docker-contract.log) 2>&1
work=${1:-$PWD}
mkdir -p "$work"; cd "$work"
printf 'WORKSPACE=%s\nTMPDIR=%s\n' "$PWD" "${TMPDIR:-unset}"
name=pulse-prototype
cleanup() {
 docker rm -f "$name" "$name-copy" pulse-long-wait >/dev/null 2>&1 || true
 docker compose -p pulse-prototype down -v >/dev/null 2>&1 || true
 docker volume rm pulse-data >/dev/null 2>&1 || true
}
trap cleanup EXIT
docker pull --platform linux/arm64 alpine:3.22
mkdir -p 'context with spaces' 'transfer with spaces'
printf 'not-in-image\n' > 'context with spaces/ignored'
printf 'ignored\n' > 'context with spaces/.dockerignore'
printf '#!/bin/sh\necho executable\n' > 'context with spaces/run.sh'
chmod +x 'context with spaces/run.sh'
cat > 'context with spaces/Dockerfile' <<'DOCKER'
FROM alpine:3.22
COPY . /context
RUN test ! -e /context/ignored && test -x /context/run.sh
CMD ["/context/run.sh"]
DOCKER
start=$SECONDS
docker buildx build --load -t pulse-prototype:local 'context with spaces'
echo "BUILD_SECONDS=$((SECONDS-start))"
test "$(docker run --rm pulse-prototype:local)" = executable
# Local binary data is uploaded through archive APIs, never mounted.
python3 - <<'PY'
import pathlib
p=pathlib.Path('transfer with spaces/input.bin');p.write_bytes(bytes(range(256))*32768)
PY
docker create --name "$name-copy" alpine:3.22 sh -c 'cat /input.bin >/output.bin; test -x /run.sh'
start=$SECONDS
docker cp 'transfer with spaces/input.bin' "$name-copy:/input.bin"
docker cp 'context with spaces/run.sh' "$name-copy:/run.sh"
docker start -a "$name-copy"
test "$(docker inspect --format '{{.State.ExitCode}}' "$name-copy")" = 0
docker cp "$name-copy:/output.bin" 'transfer with spaces/output.bin'
cmp 'transfer with spaces/input.bin' 'transfer with spaces/output.bin'
echo "TRANSFER_BYTES=8388608 TRANSFER_ROUNDTRIP_SECONDS=$((SECONDS-start))"
docker volume create pulse-data
docker run --rm -v pulse-data:/data alpine:3.22 sh -c 'echo named-volume >/data/value'
test "$(docker run --rm -v pulse-data:/data alpine:3.22 cat /data/value)" = named-volume
docker run --rm --tmpfs /scratch alpine:3.22 sh -ec 'echo tmpfs >/scratch/value; test "$(cat /scratch/value)" = tmpfs'
docker run -d --name "$name" -p 127.0.0.1::8080 python:3.13-alpine sh -c 'mkdir -p /www; echo service >/www/index.html; exec python -m http.server 8080 --directory /www'
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}' "$name")
[[ $port =~ ^[0-9]+$ ]]
for i in $(seq 1 100); do curl -fsS "http://localhost:$port" > response && break || sleep .1; done
grep -qx service response
docker exec "$name" echo exec-ok | grep -qx exec-ok
docker logs "$name" >/dev/null
printf 'streaming\n' | docker run --rm -i alpine:3.22 cat | grep -qx streaming
# Exercise a stream longer than the relay's old 30-second header timeout.
docker exec "$name" sh -c 'echo first; sleep 35; echo last' > long-stream
test "$(cat long-stream)" = "$(printf 'first\nlast')"
# A Docker wait call may produce no response headers until the container exits.
docker run -d --name pulse-long-wait alpine:3.22 sleep 35
test "$(docker wait pulse-long-wait)" = 0
cat > compose.yaml <<'COMPOSE'
services:
  server:
    image: python:3.13-alpine
    command: [sh, -c, 'echo compose > /data/index.html; exec python -m http.server 8080 --directory /data']
    volumes: [compose-data:/data]
    ports: ['127.0.0.1::8080']
    healthcheck:
      test: [CMD, python, -c, 'import urllib.request; urllib.request.urlopen("http://127.0.0.1:8080")']
      interval: 1s
      timeout: 2s
      retries: 30
  client:
    build: './context with spaces'
    env_file: ./task.env
    depends_on:
      server:
        condition: service_healthy
    command: [sh, -ec, 'test "$${EXPECTED}" = client-env; wget -qO- http://server:8080 | grep -qx compose']
volumes:
  compose-data: {}
COMPOSE
printf 'EXPECTED=client-env\n' > task.env
start=$SECONDS
docker compose -p pulse-prototype up --build -d
client=$(docker compose -p pulse-prototype ps -aq client)
test "$(docker wait "$client")" = 0
server=$(docker compose -p pulse-prototype ps -q server)
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}' "$server")
curl -fsS "http://localhost:$port" | grep -qx compose
echo "COMPOSE_SECONDS=$((SECONDS-start))"
# Record unsupported runtime binds without promising an API firewall.
printf 'private-macos-marker\n' > job-only-marker
if docker run --rm --mount "type=bind,src=$PWD/job-only-marker,dst=/marker" alpine:3.22 cat /marker > bind-result 2> bind-error; then
  if [[ $(uname -s) == Darwin ]]; then echo 'ERROR: unexpected primary filesystem exposure'; exit 1; fi
  echo 'UNSUPPORTED_BIND=engine-local file accessible on Linux (not portable contract)'
else
  echo 'UNSUPPORTED_BIND=engine rejected missing source; no implicit sharing'
fi
printf 'PULSE_DOCKER_CONTRACT_OK\n'
