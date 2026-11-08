#!/bin/bash
# Bounded container cgroups, never host-wide memory/CPU exhaustion.
set -euo pipefail
export DOCKER_HOST=unix:///var/run/docker.sock
cleanup() { docker rm -f qualification-pressure qualification-oom >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker run -d --name qualification-pressure --cpus=3.5 --memory=6g --memory-swap=6g python:3.13-alpine python -c '
import multiprocessing
def stress():
    data=bytearray(1024*1024*1024)
    while True:
        data[0]=(data[0]+1)%256
workers=[multiprocessing.Process(target=stress) for _ in range(4)]
for worker in workers: worker.start()
for worker in workers: worker.join()
' >/dev/null
sleep 15
test "$(docker inspect -f '{{.HostConfig.NanoCpus}}' qualification-pressure)" = 3500000000
docker exec qualification-pressure python -c 'from pathlib import Path; s=dict(x.split() for x in Path("/sys/fs/cgroup/cpu.stat").read_text().splitlines()); assert int(s["nr_throttled"])>0; memory=int(Path("/sys/fs/cgroup/memory.current").read_text()); assert memory>3*1024**3; print("CPU_THROTTLED="+s["nr_throttled"],"MEMORY_BYTES="+str(memory))'
docker rm -f qualification-pressure >/dev/null
docker run -d --name qualification-oom --memory=64m --memory-swap=64m python:3.13-alpine python -c 'x=bytearray(256*1024*1024); print(len(x))' >/dev/null
test "$(docker wait qualification-oom)" = 137
test "$(docker inspect -f '{{.State.OOMKilled}}' qualification-oom)" = true
echo CONTAINER_OOM_CONFIRMED
