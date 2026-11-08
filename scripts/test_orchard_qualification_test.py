"""Orchard preflight validates capacity and archives no VM credentials."""
import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import subprocess
import sys
from tempfile import TemporaryDirectory
from threading import Thread
import unittest

ROOT = Path(__file__).resolve().parent.parent

class OrchardPreflight(unittest.TestCase):
    def test_idle_worker_capacity_auth_and_redacted_inventory(self):
        worker = {'name': 'test-mac', 'arch': 'arm64', 'runtime': 'tart', 'capabilities': ['vm-endpoints'],
                  'resources': {'org.cirruslabs.logical-cores': 16, 'org.cirruslabs.memory-mib': 65536, 'org.cirruslabs.tart-vms': 2}}
        vm = {'name': 'stopped', 'worker': 'test-mac', 'conditions': [{'type': 'running', 'state': 'false'}],
              'password': 'do-not-archive', 'startup_script': {'script_content': 'do-not-archive'}}
        self_case = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_): pass
            def do_GET(self):
                self_case.assertEqual(self.headers.get('Authorization'), 'Basic '+base64.b64encode(b'test:token').decode())
                data = {'/v1/controller/info': {'version': 'fixture'}, '/v1/workers/test-mac': worker, '/v1/vms': [vm]}[self.path]
                self.send_response(200); self.end_headers(); self.wfile.write(json.dumps(data).encode())
        server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        thread = Thread(target=server.serve_forever, daemon=True); thread.start()
        try:
            with TemporaryDirectory() as tmp:
                env = dict(os.environ, PULSE_TEST_ORCHARD_URL=f'http://127.0.0.1:{server.server_port}', PULSE_TEST_ORCHARD_WORKER='test-mac', PULSE_TEST_ORCHARD_USERNAME='test', PULSE_TEST_ORCHARD_TOKEN='token')
                def run():
                    return subprocess.run([sys.executable, str(ROOT/'scripts/docker-qualification/orchard-preflight.py'), tmp], env=env, capture_output=True, text=True, timeout=10)
                result = run()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertNotIn('do-not-archive', (Path(tmp)/'before-vms.json').read_text())
                vm['conditions'][0]['state'] = 'true'
                result = run()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('idle worker', result.stderr)
                vm['conditions'][0]['state'] = 'false'
                worker['capabilities'] = []
                self.assertNotEqual(run().returncode, 0)
                worker['capabilities'] = ['vm-endpoints']
                worker['resources']['org.cirruslabs.logical-cores'] = 8
                self.assertNotEqual(run().returncode, 0)
        finally:
            server.shutdown(); server.server_close(); thread.join()

if __name__ == '__main__': unittest.main()
