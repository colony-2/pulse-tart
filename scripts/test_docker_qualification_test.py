"""Qualification cannot pass on markers, missing stages, or incomplete JobDB evidence."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location('qualification_report', ROOT / 'scripts/docker-qualification/report.py')
report = importlib.util.module_from_spec(spec)
spec.loader.exec_module(report)


class QualificationReport(unittest.TestCase):
    def setUp(self):
        self.tmp = TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.evidence = {'checks': dict.fromkeys(report.CHECKS, True), 'peak_running_macos': 2,
                         'identities': [{key: f'{key}-{i}' for key in
                             ('primary_uid', 'engine_id', 'client_key_sha256', 'host_key_sha256')} for i in range(6)]}
        self.events = [{'Action': 'pass', 'Package': 'github.com/colony-2/pulse-tart', 'Test': 'TestLiveDockerQualification'}]
        for i, identity in enumerate(self.evidence['identities']):
            identity['mac'] = '00:11:22:33:44:0' + str(i % 2)
        self.jobs = [{'status': 'COMPLETED', 'output_ok': True, 'claims': 1, 'renewals': 3, 'secret_calls': 1} for _ in range(4)]

    def result(self, exit_code=0, backend="tart"):
        (self.root / 'qualification.json').write_text(json.dumps(self.evidence))
        (self.root / 'tests.jsonl').write_text('\n'.join(map(json.dumps, self.events)))
        for p in self.root.glob('jobdb-*.json'):
            p.unlink()
        for i, j in enumerate(self.jobs):
            (self.root / f'jobdb-{i}.json').write_text(json.dumps(j))
        return report.summarize(self.root, exit_code, backend)['result']

    def test_orchard_requires_its_own_fault_evidence(self):
        self.assertEqual(self.result(backend='orchard'), 'incomplete')
        self.evidence['backend'] = 'orchard'
        self.evidence['checks']['orchard_api_outage'] = True
        self.evidence['checks']['orchard_endpoint_recreation'] = True
        del self.evidence['checks']['worker_endpoint_sigkill']
        self.assertEqual(self.result(backend='orchard'), 'passed')
        self.assertEqual(self.result(backend='tart'), 'incomplete')
        for key in ('orchard_api_outage', 'orchard_endpoint_recreation'):
            self.evidence['checks'][key] = False
            self.assertEqual(self.result(backend='orchard'), 'incomplete')
            self.evidence['checks'][key] = True

    def test_complete_evidence(self):
        self.assertEqual(self.result(), 'passed')
        self.assertEqual(self.result(1), 'incomplete')

    def test_every_fault_pressure_and_isolation_stage_is_required(self):
        for key in report.CHECKS:
            with self.subTest(stage=key):
                self.evidence['checks'][key] = False
                self.assertEqual(self.result(), 'incomplete')
                self.evidence['checks'][key] = True

    def test_skip_cleanup_failure_and_marker_only_cannot_pass(self):
        for event in ({'Action': 'skip', 'Test': 'TestLiveDockerQualification'},
                      {'Action': 'output', 'Output': 'DOCKER_QUALIFICATION_PASS:'}):
            self.events = [event]
            self.assertEqual(self.result(), 'incomplete')
        self.events = [{'Action': 'pass', 'Package': 'github.com/colony-2/pulse-tart', 'Test': 'TestLiveDockerQualification'},
                       {'Action': 'fail', 'Test': 'cleanup'}]
        self.assertEqual(self.result(), 'incomplete')

    def test_duplicate_keys_or_missing_generations_cannot_pass(self):
        original = copy.deepcopy(self.evidence['identities'])
        for key in ('primary_uid', 'engine_id', 'client_key_sha256', 'host_key_sha256'):
            self.evidence['identities'] = copy.deepcopy(original)
            self.evidence['identities'][5][key] = original[0][key]
            self.assertEqual(self.result(), 'incomplete')
        self.evidence['identities'] = original[:4]
        self.assertEqual(self.result(), 'incomplete')

    def test_concurrent_mac_collision_cannot_pass(self):
        self.evidence['identities'][1]['mac'] = self.evidence['identities'][0]['mac']
        self.assertEqual(self.result(), 'incomplete')

    def test_jobdb_and_vm_limit_are_required(self):
        for key, value in (('claims', 2), ('renewals', 0), ('secret_calls', 0), ('output_ok', False), ('status', 'FAILED')):
            old = self.jobs[0][key]
            self.jobs[0][key] = value
            self.assertEqual(self.result(), 'incomplete')
            self.jobs[0][key] = old
        for peak in (0, 3):
            self.evidence['peak_running_macos'] = peak
            self.assertEqual(self.result(), 'incomplete')
        self.evidence['peak_running_macos'] = 2
        self.jobs.pop()
        self.assertEqual(self.result(), 'incomplete')


class QualificationDriver(unittest.TestCase):
    def test_failed_preconditions_and_missing_live_evidence_are_archived(self):
        for mode in ('host', 'endpoint', 'evidence'):
            with self.subTest(mode=mode):
                self.check_driver(mode)

    def check_driver(self, mode):
        with TemporaryDirectory() as tmp:
            root = Path(tmp)
            scripts = root / 'scripts'
            scripts.mkdir()
            for name in ('test-macos-docker-qualification.sh', 'test-report.py'):
                shutil.copy2(ROOT / 'scripts' / name, scripts / name)
            (scripts / 'docker-qualification').mkdir()
            shutil.copy2(ROOT / 'scripts/docker-qualification/report.py', scripts / 'docker-qualification/report.py')
            (scripts / 'docker-prototype').mkdir()
            fixture = 'import unittest\nclass Fixture(unittest.TestCase):\n def test_ok(self): self.assertTrue(True)\n'
            (scripts / 'test_fixture_test.py').write_text(fixture)
            (scripts / 'docker-prototype/test_fixture.py').write_text(fixture)
            for name in ('go.mod', 'go.sum', 'flake.nix', 'flake.lock', 'bases.json'):
                shutil.copy2(ROOT / name, root / name)
            binary = root / 'bin'
            binary.mkdir()
            mocks = {
                'uname': '#!/bin/bash\ncase "$1" in -s) echo Darwin;; -m) echo arm64;; *) echo "Darwin arm64";; esac\n',
                'git': '#!/bin/bash\necho fixture\n',
                'tart': "#!/bin/bash\nif [[ $1 != list ]]; then echo fixture; elif [[ $FAIL_STAGE == host ]]; then echo '[{\"State\":\"running\"}]'; else echo '[]'; fi\n",
                'go': """#!/bin/bash
if [[ $1 == version ]]; then echo fixture; exit 0; fi
printf '%s\n' "$*" >> "$CALLS_FILE"
if [[ $FAIL_STAGE == evidence && $* == *TestLiveDockerEndpoint* ]]; then
 echo '{"Action":"output","Output":"DOCKER_ENDPOINT_SERVICE_PASS: fixture"}'
else
 echo '{"Action":"skip","Test":"TestLiveDockerQualification"}'
fi
""",
                # Exercise the real host rejection; bypass disk size only in
                # the remaining fixture cases so tests work on small CI disks.
                'python3': """#!/bin/bash
if [[ $1 == - && $2 == */before-vms.json && $FAIL_STAGE != host ]]; then cat >/dev/null; exit 0; fi
exec "$REAL_PYTHON" "$@"
""",

            }
            for name, content in mocks.items():
                path = binary / name
                path.write_text(content)
                path.chmod(0o755)
            calls = root / 'calls'
            env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'], CALLS_FILE=str(calls), PULSE_TEST_TART_HOME=str(root), FAIL_STAGE=mode, REAL_PYTHON=sys.executable)
            result = subprocess.run(['bash', str(scripts / 'test-macos-docker-qualification.sh')], env=env, capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
            if mode == 'host':
                self.assertIn('Stop existing running VMs', result.stderr)
            commands = calls.read_text().splitlines() if calls.exists() else []
            self.assertEqual(len(commands), {'host': 0, 'endpoint': 1, 'evidence': 2}[mode], commands)
            if commands:
                self.assertIn('TestLiveDockerEndpoint', commands[0])
            reports = list((root / 'test-results').glob('*/report.json'))
            self.assertEqual(len(reports), 1)
            self.assertEqual(json.loads(reports[0].read_text())['result'], 'incomplete')
            self.assertTrue(Path(str(reports[0].parent) + '.tar.gz').exists())
            self.assertIn('Send: test-results/', result.stdout)

class PrivateDiskGuard(unittest.TestCase):
    def run_probe(self, *, mount=None, write_error=None, existing=False):
        import errno
        import runpy
        from types import SimpleNamespace
        from unittest.mock import MagicMock, patch
        root = MagicMock()
        root.__str__.return_value = '/var/lib/docker'
        target = root.__truediv__.return_value
        if existing:
            target.open.side_effect = FileExistsError('already present')
        writer = target.open.return_value.__enter__.return_value
        writer.write.side_effect = [2**20, write_error or OSError(errno.ENOSPC, 'full')]
        fs = {'target': '/var/lib/docker', 'fstype': 'ext4', 'source': '/dev/loop0'}
        fs.update(mount or {})
        try:
            with patch('pathlib.Path', return_value=root), \
                 patch('subprocess.check_output', return_value=json.dumps({'filesystems': [fs]}).encode()), \
                 patch('shutil.disk_usage', return_value=SimpleNamespace(total=8*2**30, free=20*2**30)), \
                 patch('time.sleep'), patch('builtins.print'):
                runpy.run_path(str(ROOT / 'scripts/docker-qualification/disk-pressure.py'), run_name='__main__')
        except (OSError, AssertionError) as error:
            return root, target, error
        return root, target, None

    def test_enospc_removes_only_its_created_file(self):
        _, target, error = self.run_probe()
        self.assertIsNone(error)
        target.open.assert_called_once_with('xb', buffering=0)
        target.unlink.assert_called_once_with()

    def test_wrong_mount_never_writes(self):
        for mount in ({'target': '/'}, {'fstype': 'apfs'}, {'source': '/dev/vda1'}):
            with self.subTest(mount=mount):
                _, target, error = self.run_probe(mount=mount)
                self.assertIsInstance(error, AssertionError)
                target.open.assert_not_called()
                target.unlink.assert_not_called()

    def test_existing_file_is_preserved_and_other_io_errors_fail(self):
        import errno
        _, target, error = self.run_probe(existing=True)
        self.assertIsInstance(error, FileExistsError)
        target.unlink.assert_not_called()
        _, target, error = self.run_probe(write_error=OSError(errno.EIO, 'I/O failed'))
        self.assertIsInstance(error, OSError)
        self.assertEqual(error.errno, errno.EIO)
        target.unlink.assert_called_once_with()


if __name__ == '__main__':
    unittest.main()
