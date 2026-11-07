"""A broken native endpoint must stop the driver before any VM suite runs."""
import json
import os
from pathlib import Path
import shutil
import subprocess
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent

class DockerDriver(unittest.TestCase):
    def test_failed_or_skipped_preflight_never_starts_vm_suite(self):
        for code in (0, 1):
            with self.subTest(preflight_exit=code), TemporaryDirectory() as tmp:
                root = Path(tmp)
                scripts = root / 'scripts'
                scripts.mkdir()
                (scripts / 'docker-prototype').mkdir()
                # Python 3.12+ exits unsuccessfully when discovery finds no
                # tests. Exercise both snapshot suites before the mock Go run.
                fixture = '''import unittest
from pathlib import Path
class SnapshotFixture(unittest.TestCase):
    def test_source_available(self):
        self.assertTrue(Path('go.mod').is_file())
'''
                (scripts / 'test_fixture_test.py').write_text(fixture)
                (scripts / 'docker-prototype' / 'test_fixture.py').write_text(fixture)
                for name in ('test-macos-docker-library.sh', 'test-report.py'):
                    shutil.copy2(ROOT / 'scripts' / name, scripts / name)
                for name in ('go.mod', 'go.sum', 'flake.nix', 'flake.lock', 'bases.json'):
                    shutil.copy2(ROOT / name, root / name)
                binary = root / 'bin'
                binary.mkdir()
                mocks = {
                    'uname': '#!/bin/bash\ncase "$1" in -s) echo Darwin;; -m) echo arm64;; *) echo "Darwin arm64";; esac\n',
                    'git': '#!/bin/bash\necho fixture-commit\n',
                    'tart': '#!/bin/bash\necho fixture-tart\n',
                    'go': '''#!/bin/bash
if [[ $1 == version ]]; then echo 'go version fixture'; exit 0; fi
printf '%s\n' "$*" >> "$CALLS_FILE"
printf '%s\n' '{"Action":"output","Output":"endpoint preflight did not pass\\n"}'
exit "$PREFLIGHT_EXIT"
''',
                }
                for name, content in mocks.items():
                    p = binary / name
                    p.write_text(content)
                    p.chmod(0o755)
                calls = root / 'calls'
                env = dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'],
                           CALLS_FILE=str(calls), PREFLIGHT_EXIT=str(code))
                result = subprocess.run(['bash', str(scripts / 'test-macos-docker-library.sh'), 'tart', 'darwin'],
                                        env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 2, result.stdout + result.stderr)
                detail = result.stdout + result.stderr + '\n'.join(
                    p.read_text() for p in (root / 'test-results').glob('*/*.log'))
                self.assertTrue(calls.exists(), detail)
                commands = calls.read_text().splitlines()
                self.assertEqual(len(commands), 1, commands)
                self.assertIn('^TestLiveDockerEndpoint$', commands[0])
                reports = list((root / 'test-results').glob('*/report.json'))
                self.assertEqual(len(reports), 1)
                self.assertEqual(json.loads(reports[0].read_text())['result'], 'incomplete')
                self.assertTrue(Path(str(reports[0].parent) + '.tar.gz').exists())
                self.assertIn('Send: test-results/', result.stdout)

if __name__ == '__main__':
    unittest.main()
