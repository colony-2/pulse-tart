"""Read-only native checks; every invocation has its own scratch and evidence."""
import glob
import json
import os
from pathlib import Path
import platform
import selectors
import signal
import subprocess
import sys
import tempfile
import time

CHECKS = {
    'formatting': [['gofmt', '-l', *sorted(glob.glob('*.go')), 'internal', 'guest']],
    'race': [['go', 'test', '-race', './...']],
    'vet-shell': [['go', 'vet', './...'], *[['bash', '-n', p] for p in sorted(glob.glob('scripts/*.sh'))]],
    'build': [['go', 'build', './...']],
}
LIMIT = 256 * 1024
TIMEOUT = 900
active = None


def stop(signum, _frame):
    if active is not None:
        try:
            os.killpg(active.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    raise SystemExit(128 + signum)


for sig in (signal.SIGTERM, signal.SIGINT):
    signal.signal(sig, stop)


def run(command, env, log):
    global active
    log.write(('\n$ ' + json.dumps(command) + '\n').encode())
    size = 0
    had_output = False
    deadline = time.monotonic() + TIMEOUT
    try:
        active = subprocess.Popen(command, env=env, stdout=subprocess.PIPE,
                                  stderr=subprocess.STDOUT, start_new_session=True)
        with selectors.DefaultSelector() as selector:
            selector.register(active.stdout, selectors.EVENT_READ)
            while selector.get_map():
                if time.monotonic() >= deadline:
                    os.killpg(active.pid, signal.SIGKILL)
                    active.wait()
                    log.write(b'\nTIMEOUT\n')
                    return {'command': command, 'status': 'failed', 'reason': 'timeout'}
                for key, _ in selector.select(0.1):
                    data = os.read(key.fileobj.fileno(), 8192)
                    if not data:
                        selector.unregister(key.fileobj)
                        continue
                    had_output = True
                    remaining = max(0, LIMIT - size)
                    log.write(data[:remaining])
                    size += len(data)
            code = active.wait(timeout=max(0.1, deadline - time.monotonic()))
        if size > LIMIT:
            log.write(b'\n[output truncated]\n')
        return {'command': command, 'exit_code': code,
                'status': 'passed' if code == 0 and not (command[0] == 'gofmt' and had_output) else 'failed'}
    except (OSError, subprocess.SubprocessError) as exc:
        if active is not None and active.poll() is None:
            os.killpg(active.pid, signal.SIGKILL)
            active.wait()
        log.write(('\nLAUNCH ERROR: ' + str(exc) + '\n').encode())
        return {'command': command, 'status': 'failed', 'reason': str(exc)}
    finally:
        active = None


def main():
    mode = sys.argv[1]
    outbox = Path(os.environ['OUTBOX'])
    if mode == 'aggregate':
        inbox = Path(os.environ['INBOX'])
        results = []
        for name in CHECKS:
            result = json.loads((inbox / (name + '.json')).read_text())
            assert result['check'] == name
            assert result['status'] in ('passed', 'failed')
            assert result['commands'] and all(c['status'] in ('passed', 'failed') for c in result['commands'])
            assert result['status'] == ('passed' if all(c['status'] == 'passed' for c in result['commands']) else 'failed')
            assert (inbox / (name + '.log')).stat().st_size > 0
            results.append(result)
        passed = all(r['status'] == 'passed' for r in results)
        (outbox / 'verification.json').write_text(json.dumps(results, indent=2))
        (outbox / 'verification.md').write_text('# Native verification\n\n' + '\n'.join(
            '- ' + r['check'] + ': ' + r['status'] for r in results) + '\n\nLive infrastructure, alternate platforms, releases and deployment are held.\n')
        print('passed' if passed else 'failed', end='')
        return
    with tempfile.TemporaryDirectory(prefix='pulse-native-') as scratch:
        env = os.environ.copy()
        for gate in ('PULSE_TEST_BACKEND', 'PULSE_TEST_DOCKER_LIBRARY', 'PULSE_TEST_DOCKER_ENDPOINT',
                     'PULSE_TEST_DOCKER_QUALIFICATION', 'PULSE_TEST_QUALIFICATION_BACKEND',
                     'PULSE_HELPER', 'PULSE_BYTES_HELPER'):
            env[gate] = ''
        for key in ('GOCACHE', 'GOMODCACHE', 'GOTMPDIR', 'TMPDIR'):
            env[key] = str(Path(scratch) / key)
            Path(env[key]).mkdir()
        env.update(GOTOOLCHAIN='local', GOENV='off', GOWORK='off', PYTHONDONTWRITEBYTECODE='1', GOPRIVATE='github.com/colony-2/*',
                   GOOS='linux', GOARCH='arm64', CGO_ENABLED='1', GOFLAGS='')
        with (outbox / (mode + '.log')).open('wb') as log:
            # No metadata is inserted in shell source. Scoped Git credentials remain inherited.
            tools = [run(c, env, log) for c in (['go', 'version'], ['gcc', '--version'],
                     ['bash', '--version'], ['python3', '--version'])]
            if platform.system() != 'Linux' or platform.machine() not in ('aarch64', 'arm64'):
                raise RuntimeError('native Linux arm64 required')
            if all(t['status'] == 'passed' for t in tools):
                commands = [run(c, env, log) for c in CHECKS[mode]]
            else:
                commands = [{'command': c, 'status': 'failed', 'reason': 'not run: required tool unavailable'} for c in CHECKS[mode]]
            result = {'check': mode, 'status': 'passed' if all(t['status'] == 'passed' for t in tools + commands) else 'failed',
                      'commands': commands, 'tools': tools, 'platform': platform.platform(),
                      'candidate': os.environ.get('CANDIDATE'), 'base': os.environ.get('BASE'),
                      'exclusions': ['live infrastructure', 'cross-builds', 'macOS', 'cloud', 'releases', 'signing', 'publication', 'deployment']}
        (outbox / (mode + '.json')).write_text(json.dumps(result, indent=2))
        print(result['status'], end='')


if __name__ == '__main__':
    main()
