#!/usr/bin/python3
# Official Docker Python SDK; no CLI subprocess or wrapper.
import docker, json, os, concurrent.futures

c = docker.from_env()
assert c.ping()
with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
    assert all(pool.map(lambda _: c.ping(), range(8)))
result = c.containers.run("alpine:3.22", ["sh", "-c", "printf sdk-exec"], remove=True)
assert result == b"sdk-exec"
print(
    json.dumps(
        {
            "status": "passed",
            "sdk_version": docker.__version__,
            "server": c.version(),
            "concurrent_clients": 8,
        }
    )
)
