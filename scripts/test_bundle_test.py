import json
import os
from pathlib import Path
import subprocess
import tarfile
from tempfile import TemporaryDirectory
import unittest

ROOT = Path(__file__).resolve().parent.parent
REVISION = 'a' * 40


class BundleSelection(unittest.TestCase):
    def setUp(self):
        self.temp = TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        # Mock only GitHub transport; run real packaging, selection and hashing.
        curl = self.bin / 'curl'
        curl.write_text('''#!/bin/bash
set -eu
url= destination=
while [[ $# -gt 0 ]]; do
  case "$1" in
    https://*) url=$1 ;;
    -o) shift; destination=$1 ;;
  esac
  shift
done
printf '%s\\n' "$url" >> "$REQUESTS"
case "$url" in
  */releases/latest) printf '{"tag_name":"v1.2.3"}' > "$destination" ;;
  */commits/v1.2.3) printf '{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}' > "$destination" ;;
  *) exit 22 ;;
esac
''')
        curl.chmod(0o755)
        self.requests = self.root / 'requests'
        self.env = dict(os.environ, PATH=str(self.bin)+os.pathsep+os.environ['PATH'], REQUESTS=str(self.requests))

    def build(self, *args, success=True):
        archive = self.root / 'bundle.tar.gz'
        result = subprocess.run(['bash', str(ROOT/'scripts/build-bundle.sh'), str(archive), *args], env=self.env, capture_output=True, text=True)
        if not success:
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(archive.exists())
            return
        self.assertEqual(result.returncode, 0, result.stderr)
        with tarfile.open(archive) as bundle:
            return {m.name: bundle.extractfile(m).read() for m in bundle.getmembers()}

    def test_latest_default_resolves_release_and_records_commit(self):
        files = self.build()
        self.assertIn(REVISION.encode(), files['flake.nix'])
        self.assertEqual(self.requests.read_text().splitlines(), [
            'https://api.github.com/repos/colony-2/c2j/releases/latest',
            'https://api.github.com/repos/colony-2/c2j/commits/v1.2.3'])
        self.assertEqual(json.loads(files['flake.lock'])['nodes']['nixpkgs'], json.loads((ROOT/'flake.lock').read_text())['nodes']['nixpkgs'])

    def test_specific_release_and_integration_profile(self):
        files = self.build('integration', '--c2j', 'v1.2.3')
        self.assertNotIn('releases/latest', self.requests.read_text())

    def test_commit_requires_no_resolution_and_changes_recipe(self):
        bundled = self.build('--c2j', 'b'*40)
        selected = self.build('--c2j', REVISION)
        self.assertFalse(self.requests.exists())
        self.assertNotEqual(bundled['flake.nix'], selected['flake.nix'])

    def test_prepare_override_uses_same_selection(self):
        files = self.build('--c2j', 'b'*40)
        directory = self.root / 'unpacked'
        for name, data in files.items():
            target = directory/name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
        subprocess.run(['bash', str(directory/'select-c2j.sh'), str(directory), 'latest'], env=self.env, check=True, capture_output=True)
        expected = self.build('--c2j', 'v1.2.3')
        self.assertEqual((directory/'flake.nix').read_bytes(), expected['flake.nix'])

    def test_missing_release_does_not_fall_back(self):
        self.build('--c2j', 'missing', success=False)

    def test_invalid_selector_fails_before_network(self):
        self.build('--c2j', 'bad;value', success=False)
        self.assertFalse(self.requests.exists())


if __name__ == '__main__':
    unittest.main()
