"""apt-cache producers must finish under pipefail even with large inventories."""
import os
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent


class EngineVersionSelection(unittest.TestCase):
    def test_large_package_inventory_does_not_sigpipe(self):
        line = next(line.strip() for line in (ROOT / 'scripts/docker-engine.sh').read_text().splitlines()
                    if line.strip().startswith('selected=$(apt-cache'))
        with TemporaryDirectory() as tmp:
            binary = Path(tmp) / 'apt-cache'
            binary.write_text('''#!/usr/bin/env python3
import sys
print('docker.io | 29.1.3-0ubuntu3 | repository')
print('docker.io | 29.1.3-older | repository')
for _ in range(20000):
    print('docker.io | 28.0.0-older | repository')
''')
            binary.chmod(0o755)
            env = dict(os.environ, PATH=tmp + os.pathsep + os.environ['PATH'])
            result = subprocess.run(['bash', '-c', 'set -euo pipefail\nversion=29.1.3\n' + line + '\nprintf "%s" "$selected"\n'],
                                    env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, '29.1.3-0ubuntu3')


if __name__ == '__main__':
    unittest.main()
