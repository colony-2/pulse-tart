import importlib.util
import pathlib
import unittest
from unittest.mock import Mock, patch
from types import SimpleNamespace

ROOT = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('no_bind', ROOT/'no_bind.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class EndpointTest(unittest.TestCase):
    def test_endpoint_uses_dedicated_key_only_service_not_management_ssh(self):
        vm = {'spec': {'name':'owned-engine'}}
        uploaded = {}
        observed = {'observedEndpoints':[{'name':'engine-ssh','state':'listening','workerPort':45000,'protocol':'tcp'}]}
        native = Mock(side_effect=[{}, observed])
        def guest_command(_, command):
            # Native Tart connects as the image admin, not the service user.
            # Its 0700 .ssh directory must stay private during fingerprinting.
            if '/home/pulseengine/.ssh/authorized_keys' in command and not command.startswith('sudo '):
                raise PermissionError('service account key is not admin-readable')
            return b'pinned-server-key'
        guest = Mock(side_effect=guest_command)
        api = Mock()
        env = dict(a=SimpleNamespace(backend="orchard"), guest=guest, upload=lambda _,p,b: uploaded.update({p:b}),
                   native=native, api=api, wait=lambda _,f,seconds: f(),
                   URL='http://192.0.2.1:6120/v1', report={},save=lambda:None)
        with patch.dict("os.environ", {"PULSE_TEST_ENGINE_WORKER_ADDRESS": "192.0.2.1"}):
            address, hostkey = module.endpoint_transport(env, vm)
        self.assertEqual(address, '192.0.2.1:45000')
        self.assertEqual(hostkey, b'pinned-server-key')
        config = uploaded['/opt/pulse-docker/endpoint-sshd.conf'].decode()
        for setting in ['Port 2222','PasswordAuthentication no','AllowUsers pulseengine','ForceCommand /bin/false']:
            self.assertIn(setting, config)
        sent = api.call_args.args[2]
        self.assertEqual(sent['endpoints'][0]['target']['vm']['port'],2222)
        self.assertNotIn('hostDirs', sent)

class WorkerEndpointTest(unittest.TestCase):
    def test_binary_half_close(self):
        import json
        import socket
        import subprocess
        import sys
        import tempfile
        import threading
        import time
        with tempfile.TemporaryDirectory() as tmp:
            server = socket.socket()
            server.bind(('127.0.0.1', 0))
            server.listen()
            def echo():
                c, _ = server.accept()
                with c:
                    data = bytearray()
                    while True:
                        part = c.recv(65536)
                        if not part:
                            break
                        data.extend(part)
                    c.sendall(data)
            worker = threading.Thread(target=echo, daemon=True)
            worker.start()
            ready = pathlib.Path(tmp)/'ready.json'
            proc = subprocess.Popen([sys.executable,str(ROOT/'worker-endpoint.py'),'--target','127.0.0.1:'+str(server.getsockname()[1]),'--ready',str(ready)])
            try:
                deadline = time.monotonic()+5
                while not ready.exists() and time.monotonic()<deadline:
                    time.sleep(.01)
                port = json.loads(ready.read_text())['port']
                payload = bytes(range(256))*1024
                with socket.create_connection(('127.0.0.1',port),2) as c:
                    c.sendall(payload)
                    c.shutdown(socket.SHUT_WR)
                    result=bytearray()
                    while True:
                        part=c.recv(65536)
                        if not part:break
                        result.extend(part)
                    self.assertEqual(result,payload)
            finally:
                proc.terminate()
                proc.wait(timeout=5)
                server.close()
                worker.join(timeout=2)


if __name__ == '__main__':
    unittest.main()
