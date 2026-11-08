#!/usr/bin/env python3
"""Read-only placement/capacity preflight; never archive VM credentials."""
import base64
import json
import os
from pathlib import Path
import sys
import urllib.parse
import urllib.request

out = Path(sys.argv[1])
base = os.environ['PULSE_TEST_ORCHARD_URL'].rstrip('/')
if not base.endswith('/v1'):
    base += '/v1'
worker = os.environ['PULSE_TEST_ORCHARD_WORKER']

def get(path):
    req = urllib.request.Request(base + path)
    if os.environ.get('PULSE_TEST_ORCHARD_TOKEN'):
        auth = os.environ.get('PULSE_TEST_ORCHARD_USERNAME', '') + ':' + os.environ['PULSE_TEST_ORCHARD_TOKEN']
        req.add_header('Authorization', 'Basic ' + base64.b64encode(auth.encode()).decode())
    with urllib.request.urlopen(req, timeout=10) as response:
        return json.load(response)

info = get('/controller/info')
w = get('/workers/' + urllib.parse.quote(worker, safe=''))
vms = get('/vms')
(out / 'orchard-environment.json').write_text(json.dumps({'controller': info, 'worker': w}, indent=2)+'\n')
assert 'vm-endpoints' in w.get('capabilities', []), 'Worker must support VM endpoints'
assert w.get('arch') == 'arm64' and w.get('runtime') == 'tart'
resources = w['resources']
assert resources.get('org.cirruslabs.logical-cores', 0) >= 16, 'Worker needs 16 vCPUs'
assert resources.get('org.cirruslabs.memory-mib', 0) >= 32768, 'Worker needs 32 GiB plus host overhead'
assert resources.get('org.cirruslabs.tart-vms', 0) >= 2, 'Worker needs two macOS slots'
placed = [v for v in vms if v.get('worker') == worker or v.get('labels', {}).get('org.cirruslabs.orchard.worker-name') == worker]
(out / 'before-vms.json').write_text(json.dumps([{k: v.get(k) for k in ('name', 'uid', 'os', 'status', 'powerState', 'conditions')} for v in placed], indent=2)+'\n')
assert all(any(c.get('type') == 'running' and c.get('state') == 'false' for c in v.get('conditions', [])) for v in placed), 'Run qualification on an idle worker; existing VMs were not stopped'
print('Orchard worker is idle and has capacity for two macOS/Linux pairs.')
