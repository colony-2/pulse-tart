import os
from pathlib import Path
import subprocess
from tempfile import TemporaryDirectory
import unittest

ROOT=Path(__file__).resolve().parent.parent
HASH='sha256-'+'B'*43+'='

class VendorResolution(unittest.TestCase):
    def run_resolution(self, failure=False):
        with TemporaryDirectory() as tmp:
            root=Path(tmp);binary=root/'nix';recipe=root/'flake.nix'
            recipe.write_text('workerVendorHash = "sha256-'+'A'*43+'=";\n')
            binary.write_text('#!/bin/bash\nset -eu\nif [[ ${NETWORK_FAILURE:-0} == 1 ]]; then echo network-failed >&2; exit 1; fi\nif grep -q "'+HASH+'" "$RECIPE"; then exit 0; fi\necho "error: dependency hash mismatch; got: '+HASH+'" >&2\nexit 1\n')
            binary.chmod(0o755)
            env=dict(os.environ,PATH=str(root)+os.pathsep+os.environ['PATH'],RECIPE=str(recipe),NETWORK_FAILURE=str(int(failure)))
            result=subprocess.run(['bash',str(ROOT/'scripts/resolve-c2j-vendor.sh'),str(root)],env=env,capture_output=True,text=True)
            return result,recipe.read_text()
    def test_resolves_selected_source_hash_and_verifies_second_build(self):
        result,recipe=self.run_resolution();self.assertEqual(result.returncode,0,result.stderr);self.assertIn(HASH,recipe)
    def test_transport_failure_does_not_invent_or_pin_dependencies(self):
        result,recipe=self.run_resolution(True);self.assertNotEqual(result.returncode,0);self.assertNotIn(HASH,recipe)

if __name__=='__main__':unittest.main()
