#!/bin/bash
# Runs inside a real c2j job as pulseworker. Identical names/ports in both VMs.
set -euo pipefail
marker=$(cat /opt/pulse-docker/qualification-marker)
export DOCKER_HOST=unix:///var/run/docker.sock
for name in qualification-service qualification-pressure qualification-oom; do
 ! docker container inspect "$name" >/dev/null 2>&1
done
! docker volume inspect qualification-data >/dev/null 2>&1
! docker volume inspect qualification-unused >/dev/null 2>&1
# Preload before the measured overlap; no external traffic is required in faults.
docker pull python:3.13-alpine
for volume in qualification-data qualification-unused; do
 docker volume create "$volume" >/dev/null
 docker run --rm -v "$volume:/data" python:3.13-alpine sh -c 'printf "%s" "$1" > /data/marker' sh "$marker"
done
docker run -d --restart=always --name qualification-service -v qualification-data:/data -p 127.0.0.1:41081:8080 python:3.13-alpine python -m http.server 8080 --directory /data >/dev/null
printf '%s' "$marker" > /var/tmp/qualification-started
# Tolerate transient daemon/relay restarts, but fail after a permanent engine loss.
failures=0
for ((i=0;i<900;i++)); do
 if value=$(curl --max-time 3 -fsS http://127.0.0.1:41081/marker) && [[ $value == "$marker" ]]; then
  failures=0
  date +%s > /var/tmp/qualification-heartbeat
 else
  failures=$((failures+1))
  if [[ $failures -ge 30 ]]; then echo 'engine unavailable for 30 consecutive probes' >&2; exit 1; fi
 fi
 if [[ -e /var/tmp/qualification-release ]]; then
  [[ $failures == 0 ]]
  test "$(docker exec qualification-service cat /data/marker)" = "$marker"
  echo PULSE_DOCKER_QUALIFICATION_JOB_OK
  exit 0
 fi
 sleep 1
done
echo 'qualification job was not released within its deadline' >&2
exit 1
