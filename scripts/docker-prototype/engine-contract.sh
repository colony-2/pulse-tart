#!/bin/bash
# Runs as the ordinary task user against an allocation-private engine.
set -euo pipefail
exec > >(tee /var/tmp/pulse-docker-contract.log) 2>&1
work=${1:-$PWD}
mkdir -p "$work"; cd "$work"
printf "WORKSPACE=%s\nTMPDIR=%s\n" "$PWD" "${TMPDIR:-unset}"
name=pulse-prototype
cleanup() {
 docker rm -f "$name" "$name-copy" >/dev/null 2>&1 || true
 docker compose -p pulse-prototype down -v >/dev/null 2>&1 || true
}
trap cleanup EXIT
# The downloaded image is recorded by digest in the report. ARM64 only.
docker pull --platform linux/arm64 alpine:3.22
mkdir -p 'path with spaces/nested'
printf 'original\n' > 'path with spaces/file'
printf '#!/bin/sh\necho executable\n' > run.sh
chmod +x run.sh
cat > Dockerfile <<'DOCKER'
FROM alpine:3.22
COPY run.sh /run.sh
CMD ["/run.sh"]
DOCKER
start=$SECONDS
docker buildx build --load -t pulse-prototype:local .
echo "BUILD_SECONDS=$((SECONDS-start))"
test "$(docker run --rm pulse-prototype:local)" = executable
docker run --rm -v "$PWD:/work" alpine:3.22 sh -ec 'test "$(cat "/work/path with spaces/file")" = original; echo container > /work/container-write; /work/run.sh'
test "$(cat container-write)" = container
docker run --rm --mount "type=bind,src=$PWD/path with spaces/file,dst=/input" alpine:3.22 cat /input | grep -qx original
mkdir -p "$TMPDIR"
docker run --rm -v "$TMPDIR:/tmp/shared" alpine:3.22 sh -c 'echo temporary >/tmp/shared/container-temp'
grep -qx temporary "$TMPDIR/container-temp"
docker run -d --name "$name" -p 127.0.0.1::8080 python:3.13-alpine sh -c 'mkdir -p /www; echo service >/www/index.html; exec python -m http.server 8080 --directory /www'
port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "8080/tcp") 0).HostPort}}' "$name")
# Retry service readiness only. Publication itself must exist before start returns.
[[ $port =~ ^[0-9]+$ ]]
for i in $(seq 1 50); do curl -fsS "http://localhost:$port" > response && break || sleep .1; done
grep -qx service response
docker exec "$name" echo exec-ok | grep -qx exec-ok
docker logs "$name" >/dev/null
docker cp run.sh "$name:/copied.sh"
docker cp "$name:/copied.sh" copied.sh
cmp run.sh copied.sh
printf 'streaming\n' | docker run --rm -i alpine:3.22 cat | grep -qx streaming
cat > compose.yaml <<'COMPOSE'
services:
  app:
    image: alpine:3.22
    command: [sh, -c, 'cat /input/file; echo compose >/input/compose-write; sleep 300']
    volumes:
      - './path with spaces:/input'
COMPOSE
start=$SECONDS
docker compose -p pulse-prototype up -d
echo "COMPOSE_START_SECONDS=$((SECONDS-start))"
for i in $(seq 1 50); do [[ -f 'path with spaces/compose-write' ]] && break; sleep .1; done
grep -qx compose 'path with spaces/compose-write'
docker compose -p pulse-prototype down -v
printf 'PULSE_DOCKER_CONTRACT_OK\n'
