"""Current no-bind contract. All resources are disposable and owned by the harness."""
import hashlib
import json
import os
import plistlib
import subprocess
import sys
import shlex
import time
import urllib.parse
from types import SimpleNamespace

MAC_IMAGE = 'ghcr.io/cirruslabs/macos-tahoe-base@sha256:4aaa751fa7538902d73c767f27a066fc87e83f183069da4e4b5022863abd4c77'
REQUIRED = ['no_bind_linux_job', 'no_bind_macos_job', 'no_bind_macos_sdk',
            'no_bind_macos_java', 'no_bind_registry', 'no_bind_unsupported', 'no_bind_controller_independence',
            'macos_authenticated_api', 'macos_tcp_ports', 'macos_start_gate',
            'macos_port_conflicts', 'macos_relay_recovery']


def required_cases(platform, extras_only=False):
    if extras_only:
        return [key for key in REQUIRED if key not in ('no_bind_linux_job','no_bind_macos_job','no_bind_controller_independence')]
    if platform == 'linux':
        return ['no_bind_linux_job']
    if platform == 'macos':
        return [key for key in REQUIRED if key != 'no_bind_linux_job']
    return REQUIRED[:]


def endpoint_transport(environment, engine, primary=None):
    h = SimpleNamespace(**environment)
    # Separate sshd: never expose the source image's admin/password service.
    h.guest(engine, 'sudo ssh-keygen -q -t ed25519 -N "" -f /opt/pulse-docker/endpoint-host-key')
    config = '''Port 2222
HostKey /opt/pulse-docker/endpoint-host-key
PidFile /run/pulse-engine-sshd.pid
AuthorizedKeysFile /home/pulseengine/.ssh/authorized_keys
AllowUsers pulseengine
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
PermitRootLogin no
UsePAM yes
AllowTcpForwarding local
AllowStreamLocalForwarding local
GatewayPorts no
X11Forwarding no
PermitTTY no
ForceCommand /bin/false
'''
    h.upload(engine, '/opt/pulse-docker/endpoint-sshd.conf', config.encode())
    h.guest(engine, 'sudo /usr/sbin/sshd -t -f /opt/pulse-docker/endpoint-sshd.conf; sudo /usr/sbin/sshd -f /opt/pulse-docker/endpoint-sshd.conf')
    h.report['transport_fingerprints'] = {
        'host_key_sha256':hashlib.sha256(h.guest(engine, 'cat /opt/pulse-docker/endpoint-host-key.pub')).hexdigest(),
        'client_key_sha256':hashlib.sha256(h.guest(engine, 'cat /home/pulseengine/.ssh/authorized_keys')).hexdigest(),
    }
    if h.a.backend == 'tart':
        ip = h.guest(engine, "hostname -I | awk '{print $1}'").decode().strip()
        gateway = h.guest(primary, "route -n get default | awk '/gateway:/{print $2}'").decode().strip()
        ready = h.PRIVATE/(engine['spec']['name']+'-endpoint.json')
        log = open(h.OUT/'worker-endpoint.log', 'wb')
        process = subprocess.Popen([sys.executable, str(h.ROOT/'scripts/docker-prototype/worker-endpoint.py'),
            '--target', ip+':2222', '--ready', str(ready)], start_new_session=True, stdout=log, stderr=log)
        h.worker_helpers.append((process, log))
        ep = h.wait('native authenticated engine endpoint', lambda: json.loads(ready.read_text()) if ready.exists() else None, 30)
        h.report['engine_endpoint'] = dict(ep, worker_address=gateway, guest_port=2222, authentication='allocation-only SSH key; lab detached worker process')
        h.save()
        return gateway+':'+str(ep['port']), h.guest(engine, 'cat /opt/pulse-docker/endpoint-host-key.pub')
    vm = h.native(engine)
    vm['endpoints'] = [{'name':'engine-ssh','protocol':'tcp','target':{'vm':{'port':2222}}}]
    h.api('PUT', '/vms/'+engine['spec']['name'], vm)
    def listening():
        vm = h.native(engine)
        for ep in vm.get('observedEndpoints', []):
            if ep['name'] == 'engine-ssh' and ep['state'] == 'listening':
                return ep
    ep = h.wait('Orchard authenticated engine endpoint', listening, 90)
    host = os.environ.get('PULSE_TEST_ENGINE_WORKER_ADDRESS', urllib.parse.urlsplit(h.URL).hostname)
    h.report['engine_endpoint'] = dict(ep, worker_address=host, guest_port=2222, authentication='allocation-only SSH public key; pinned freshly generated server key')
    h.report['engine_endpoint']['host_key_sha256'] = hashlib.sha256(h.guest(engine, 'cat /opt/pulse-docker/endpoint-host-key.pub')).hexdigest()
    h.save()
    return host+':'+str(ep['workerPort']), h.guest(engine, 'cat /opt/pulse-docker/endpoint-host-key.pub')


def record_command(h, key, vm, command, filename, reason, timeout=300):
    try:
        data = h.guest(vm, command, timeout=timeout)
    except Exception:
        h.result(key, 'failed', 'See command-failure log; requested probe failed.')
        raise
    (h.OUT/filename).write_bytes(data)
    h.result(key, 'passed', reason)


def prepare_mac(h, vm, start_fixture=True):
    h.wait('macOS no-bind SSH', lambda: h.guest(vm, 'true') == b'', 300)
    boot_seconds = time.monotonic() - vm['started']
    h.guest(vm, 'sudo mkdir -p /opt/pulse-docker')
    for local, remote in [('c2j-darwin','c2j'),('jobdb-fixture-darwin','jobdb-fixture'),
                          ('docker-darwin','docker'),('docker-buildx-darwin','docker-buildx'),
                          ('docker-compose-darwin','docker-compose')]:
        h.upload(vm, '/opt/pulse-docker/'+remote, (h.TOOLS/local).read_bytes())
    for name in ['prepare-macos.sh','engine-contract.sh','accept.py','run-job.py','sdk.py']:
        h.upload(vm, '/opt/pulse-docker/'+name, (h.ROOT/'scripts/docker-prototype'/name).read_bytes())
    h.guest(vm, 'set -eu; sudo bash /opt/pulse-docker/prepare-macos.sh; sudo mkdir -p /Users/pulseworker/work/tmp; sudo chown -R pulseworker:pulseworker /Users/pulseworker/work')
    if start_fixture:
        fixture_mac(h, vm)
    h.report['samples'].append({'vm': vm['spec']['name'], 'cold_ssh_seconds': boot_seconds, 'cold_prepare_seconds': time.monotonic()-vm['started']})
    h.save()
    (h.OUT/(vm['spec']['name']+'-versions.log')).write_bytes(h.guest(vm,'sw_vers; uname -a; /opt/pulse-docker/c2j version; /usr/local/bin/docker --version; mount'))


def fixture_mac(h, vm):
    service = {'Label':'dev.pulse.docker-fixture','UserName':'pulseworker',
        'ProgramArguments':['/opt/pulse-docker/jobdb-fixture'],'RunAtLoad':True,
        'EnvironmentVariables':{'HOME':'/Users/pulseworker','TMPDIR':'/var/tmp','PATH':'/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin'},
        'StandardOutPath':'/var/tmp/pulse-fixture.log','StandardErrorPath':'/var/tmp/pulse-fixture.log'}
    h.upload(vm, '/Library/LaunchDaemons/dev.pulse.docker-fixture.plist', plistlib.dumps(service))
    h.guest(vm, 'sudo chmod 644 /Library/LaunchDaemons/dev.pulse.docker-fixture.plist; sudo launchctl bootstrap system /Library/LaunchDaemons/dev.pulse.docker-fixture.plist')
    h.wait('macOS JobDB fixture', lambda: b'"ready":true' in h.guest(vm,'curl -fsS http://127.0.0.1:18881/health'),60)


def run_no_bind(environment):
    h = SimpleNamespace(**environment)
    for key in REQUIRED:
        h.report['cases'].setdefault(key, {'description':key,'status':'not_run','reason':'Not yet exercised by this prototype run.'})
    h.report['release_gates'] = {
        'linux_guest_readiness': {'status':'not_run','reason':'See scoped real-job cases; prepared cold boot timings and production bootstrap integration remain.'},
        'api_relay': {'status':'not_run','reason':'See scoped stream/build/archive cases; production endpoint service supervision remains.'},
        'published_ports': {'status':'not_run','reason':'See scoped TCP/conflict/restart cases; port reuse and autorestart stress remain.'},
        'testcontainers': {'status':'not_run','reason':'See Go/Java fixture results; arbitrary modules excluded.'},
        'compose_subset': {'status':'not_run','reason':'See actual c2j contract log.'},
        'snapshot_reuse': {'status':'not_run','reason':'This focused suite uses cold clones; two suspended generations and concurrent distinct slots still required.'},
        'orchard_pair': {'status':'not_run','reason':'Worker endpoint and same-worker pair are exercised; durable paired reservation/recovery is production work.'},
        'failure_resource_pressure': {'status':'not_run','reason':'Relay/engine restart probes do not establish full handoff/reaper/CPU/memory/disk-pressure behavior.'},
    }
    h.report['selection'] = {'platform':h.a.no_bind_platform, 'extras_only':h.a.no_bind_extras_only, 'rounds':h.a.rounds, 'lifecycle':h.a.lifecycle, 'concurrent_pairs':False}
    h.report['contract'] = 'No job-path binds or shared filesystems; native workspace, transferred context/archive, engine-owned volumes.'
    h.report['docker_clients'] = json.loads((h.TOOLS/'docker-clients.json').read_text())
    h.save()
    template = None
    template_boot = None
    template_mac = None
    seen_engines = set()
    if h.a.lifecycle == 'suspended':
        h.report['cases']['no_bind_snapshot_reuse'] = {'description':'Two resumed allocations from a distinct credential-free template','status':'not_run','reason':'Not yet run.'}
        template = h.create(h.spec(h.owner+'-template', 'darwin', MAC_IMAGE, suspendable=True))
        prepare_mac(h, template, start_fixture=False)
        template_boot = h.guest(template, 'sysctl -n kern.bootsessionuuid').decode().strip()
        template_mac = h.guest(template, "ifconfig en0 | awk '/ether/{print $2}'").decode().strip()
        h.tart('suspend', template['spec']['name'])
        h.wait('macOS no-bind suspended template', lambda: h.native(template)['State'] == 'suspended', 90)
        h.report['template'] = {'name':template['spec']['name'], 'boot_id':template_boot, 'mac':template_mac}
        h.save()
    for generation in range(h.a.rounds):
        if h.a.no_bind_platform != 'macos':
            linux = h.create(h.spec(h.owner+'-g'+str(generation)+'-linux'))
            try:
                h.prepare(linux)
                h.run_job(linux)
                h.result('no_bind_linux_job','passed','Real lease/base/capability checks, build context, .dockerignore, 8 MiB binary archive roundtrip, executable bit, named volume, tmpfs, streaming, Compose, unsupported bind observation.', generations=generation+1)
            except Exception:
                h.result('no_bind_linux_job','failed','See worker/contract/command logs.')
                raise
            finally:
                h.cleanup(linux)
        if h.a.no_bind_platform == 'linux':
            continue
        engine = h.create(h.spec(h.owner+'-g'+str(generation)+'-engine'))
        mac = None
        try:
            h.prepare(engine)
            h.guest(engine, "sudo docker run -d --name identical-container -p 127.0.0.1:18080:8080 python:3.13-alpine python -m http.server 8080", timeout=180)
            mac_spec = h.spec(h.owner+'-g'+str(generation)+'-mac','darwin',MAC_IMAGE, suspendable=template is not None)
            if template is None:
                mac = h.create(mac_spec)
                prepare_mac(h, mac)
            else:
                mac = h.create(mac_spec, source=template['spec']['name'], resume=True)
                h.wait('resumed macOS SSH', lambda: h.guest(mac,'true') == b'', 300)
                assert h.guest(mac,'sysctl -n kern.bootsessionuuid').decode().strip() == template_boot
                assert h.guest(mac,"ifconfig en0 | awk '/ether/{print $2}'").decode().strip() == template_mac
                h.guest(mac,'set -eu; sudo test ! -e /var/lib/pulse-docker/intent; sudo test ! -e /opt/pulse-docker/engine-key; sudo test ! -e /Users/pulseworker/prior-allocation; sudo touch /Users/pulseworker/prior-allocation')
                h.report['samples'].append({'vm':mac['spec']['name'],'resume_ssh_seconds':time.monotonic()-mac['started'],'boot_and_mac_preserved':True,'previous_allocation_files_absent':True})
                fixture_mac(h, mac)
            mac.update(engine=engine, workspace='/Users/pulseworker/work', base='macos-tahoe-26.6.2-arm64')
            def ready(primary, companion):
                engine_id = h.guest(companion, "sudo docker info --format '{{.ID}}'").decode().strip()
                assert engine_id not in seen_engines, 'Engine identity reused across allocations'
                seen_engines.add(engine_id)
                observation = {'generation':generation, 'primary':primary['spec']['name'], 'engine':companion['spec']['name'], 'engine_id':engine_id,
                    'endpoint':h.report.get('engine_endpoint'), 'keys':dict(h.report.get('transport_fingerprints',{})), 'ready_unix':time.time()}
                h.report.setdefault('pairs',[]).append(observation)
                h.save()
                if not h.a.no_bind_extras_only:
                    try:
                        h.run_job(primary)
                    except Exception:
                        h.result('no_bind_macos_job','failed','Requested generation failed; see worker/contract/command logs.', generation=generation)
                        raise
                    observation['job_finished_unix'] = time.time()
                    for key in ('job_running_first_unix', 'job_running_last_unix'):
                        observation[key] = primary.get(key)
                    h.result('no_bind_macos_job','passed','Real macOS c2j lease with native private checkout; supported no-bind contract and replay passed.', generations=generation+1)
                    if h.a.backend == 'orchard':
                        h.result('no_bind_controller_independence','passed','Worker endpoint carries guest-to-engine SSH. No controller tunnel process was created; one-shot provider processes closed and reopened while job continued. Worker/controller service crash remains separate.')
                    else:
                        h.result('no_bind_controller_independence','passed','Detached worker-local TCP bridge; one-shot provider processes close/reopen without terminating guest SSH. Production service supervision remains implementation work.')
                h.upload(primary, '/opt/pulse-docker/registry-contract.sh', (h.ROOT/'scripts/docker-prototype/registry-contract.sh').read_bytes())
                worker = 'cd /var/tmp && sudo -u pulseworker env HOME=/Users/pulseworker PATH=/opt/pulse-docker:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin DOCKER_HOST=unix:///var/run/docker.sock '
                record_command(h, 'no_bind_registry', primary, worker+'bash /opt/pulse-docker/registry-contract.sh', 'macos-registry.log',
                    'Synthetic task credentials: login, push, delete local tag, authenticated pull, run, logout and denied pull; private registry.')
                h.upload(primary, '/opt/pulse-docker/unsupported-compose.py', (h.ROOT/'scripts/docker-prototype/unsupported-compose.py').read_bytes())
                record_command(h, 'no_bind_unsupported', primary, worker+'/usr/bin/python3 /opt/pulse-docker/unsupported-compose.py', 'unsupported-compose.json',
                    'Host volumes, file-backed secrets/configs and disguised driver binds do not expose primary files. No universal rejection claim.', timeout=400)
                # Additional SDK/Java qualification follows the end-to-end worker.
                from no_bind_sdk import qualify
                try:
                    qualify(h, primary)
                except Exception:
                    case = 'no_bind_macos_java' if h.report['cases']['no_bind_macos_sdk']['status'] == 'passed' else 'no_bind_macos_sdk'
                    h.result(case, 'failed', 'See command-failure log; requested SDK qualification failed.')
                    raise
            h.mac_relay(engine, mac, ready, task_user='pulseworker')
        except Exception:
            if not h.a.no_bind_extras_only and h.report['cases']['no_bind_macos_job']['status'] != 'passed':
                h.result('no_bind_macos_job','failed','See worker/contract/command logs.')
            raise
        finally:
            if mac is not None:
                h.cleanup(mac)
            h.cleanup(engine)
    if template is not None:
        h.result('no_bind_snapshot_reuse','passed','Two or more resumed clones preserved template boot/MAC; prior job/key files absent and distinct fresh engine IDs.', generations=h.a.rounds)
