#!/usr/bin/env python3
import http.client, io, json, os, socket, tarfile, time, urllib.parse, uuid

path = os.environ.get("DOCKER_HOST", "unix:///var/run/docker.sock").removeprefix(
    "unix://"
)


class Unix(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(120)
        self.sock.connect(path)


def request(method, url, body=None, raw=False):
    c = Unix("localhost", timeout=120)
    if body is not None and not raw:
        body = json.dumps(body).encode()
    c.request(
        method,
        url,
        body,
        {"Content-Type": "application/x-tar" if raw else "application/json"},
    )
    r = c.getresponse()
    data = r.read()
    code = r.status
    c.close()
    return code, data


out = {}
owned = []


def create(ip="127.0.0.1", port=""):
    code, b = request(
        "POST",
        "/containers/create",
        {
            "Image": "python:3.13-alpine",
            "Cmd": ["python", "-m", "http.server", "8080"],
            "ExposedPorts": {"8080/tcp": {}},
            "HostConfig": {
                "PortBindings": {"8080/tcp": [{"HostIp": ip, "HostPort": port}]}
            },
        },
    )
    assert code == 201, (code, b)
    id = json.loads(b)["Id"]
    owned.append(id)
    return id


try:
    assert request("GET", "/_ping") == (200, b"OK")
    for ip in ("127.0.0.1", "0.0.0.0"):
        id = create(ip)
        code, b = request("POST", "/containers/" + id + "/start")
        assert code == 204, (code, b)
        code, b = request("GET", "/containers/" + id + "/json")
        port = int(json.loads(b)["NetworkSettings"]["Ports"]["8080/tcp"][0]["HostPort"])
        # Listener registration must be complete; no retry or sleep for this connect.
        socket.create_connection(("127.0.0.1", port), timeout=2).close()
        start = time.monotonic()
        while True:
            try:
                c = http.client.HTTPConnection("127.0.0.1", port, timeout=2)
                c.request("GET", "/")
                r = c.getresponse()
                r.read()
                assert r.status == 200
                c.close()
                break
            except (OSError, http.client.HTTPException):
                if time.monotonic() - start > 15:
                    raise
                time.sleep(0.05)  # Only the Python HTTP application's readiness.
        out[ip] = {"port": port, "application_ready_seconds": time.monotonic() - start}
        code, b = request(
            "POST",
            "/containers/" + id + "/exec",
            {
                "Cmd": ["echo", "exec-through-relay"],
                "AttachStdout": True,
                "AttachStderr": True,
            },
        )
        assert code == 201
        code, b = request(
            "POST",
            "/exec/" + json.loads(b)["Id"] + "/start",
            {"Detach": False, "Tty": False},
        )
        assert b"exec-through-relay" in b
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w") as tar:
            body = b"copy-through-relay"
            item = tarfile.TarInfo("probe.txt")
            item.size = len(body)
            tar.addfile(item, io.BytesIO(body))
        code, b = request(
            "PUT", "/containers/" + id + "/archive?path=/tmp", archive.getvalue(), True
        )
        assert code == 200, (code, b)
        code, b = request("GET", "/containers/" + id + "/archive?path=/tmp/probe.txt")
        assert code == 200
        with tarfile.open(fileobj=io.BytesIO(b)) as tar:
            assert tar.extractfile("probe.txt").read() == b"copy-through-relay"
        code, b = request("GET", "/containers/" + id + "/logs?stdout=1&stderr=1")
        assert code == 200 and b"GET /" in b
    # Force a macOS-local conflict while Linux's corresponding port is free.
    blocker = socket.socket()
    blocker.bind(("127.0.0.1", 0))
    blocker.listen()
    port = blocker.getsockname()[1]
    id = create("127.0.0.1", str(port))
    code, b = request("POST", "/containers/" + id + "/start")
    assert code == 502, (code, b)
    code, b = request("GET", "/containers/" + id + "/json")
    assert not json.loads(b)["State"]["Running"]
    blocker.close()
    out["conflict"] = "start failed and container stopped"
    out["status"] = "passed"
    print(json.dumps(out, indent=2))
finally:
    for id in owned:
        request("DELETE", "/containers/" + id + "?force=1&v=1")
