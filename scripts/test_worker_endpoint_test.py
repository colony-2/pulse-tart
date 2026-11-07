"""Exercise the production worker bridge's stream and fixed-port restart contract."""
import json
from pathlib import Path
import socket
import socketserver
import subprocess
import sys
from tempfile import TemporaryDirectory
import threading
import time
import unittest

SCRIPT = Path(__file__).resolve().parent.parent / 'internal/tart/endpoint/bridge.py'

class EchoAfterEOF(socketserver.BaseRequestHandler):
    def handle(self):
        body = bytearray()
        while chunk := self.request.recv(65536):
            body.extend(chunk)
        self.request.sendall(bytes(body)[::-1])

class WorkerEndpoint(unittest.TestCase):
    def test_binary_half_close_and_same_port_restart(self):
        with socketserver.ThreadingTCPServer(('127.0.0.1', 0), EchoAfterEOF) as target, TemporaryDirectory() as tmp:
            thread = threading.Thread(target=target.serve_forever, daemon=True)
            thread.start()
            self.addCleanup(target.shutdown)
            with socket.socket() as reserve:
                reserve.bind(('127.0.0.1', 0))
                port = reserve.getsockname()[1]
            ready = Path(tmp) / 'ready.json'
            payload = bytes(range(256)) * 1024
            for generation in range(2):
                ready.unlink(missing_ok=True)
                proc = subprocess.Popen([sys.executable, str(SCRIPT), '--target', f'127.0.0.1:{target.server_address[1]}', '--port', str(port), '--ready', str(ready)], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
                try:
                    deadline = time.monotonic() + 5
                    while not ready.exists() and proc.poll() is None and time.monotonic() < deadline:
                        time.sleep(.01)
                    self.assertTrue(ready.exists(), f'bridge generation {generation} failed to start')
                    self.assertEqual(json.loads(ready.read_text())['port'], port)
                    with socket.create_connection(('127.0.0.1', port), timeout=5) as client:
                        client.sendall(payload)
                        client.shutdown(socket.SHUT_WR)
                        received = bytearray()
                        while chunk := client.recv(65536):
                            received.extend(chunk)
                        self.assertEqual(bytes(received), payload[::-1])
                finally:
                    proc.terminate()
                    proc.communicate(timeout=5)

if __name__ == '__main__':
    unittest.main()
