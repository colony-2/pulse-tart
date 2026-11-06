import importlib.util
import pathlib
import tempfile
import unittest
from unittest import mock


def load(name, file):
    spec = importlib.util.spec_from_file_location(
        name, pathlib.Path(__file__).with_name(file)
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


probe = load("coherence_probe", "coherence-probe.py")
admission = load("workspace_admission", "check-workspace.py")


class CoherenceTest(unittest.TestCase):
    def test_local_roundtrip_and_atomic_replacement(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "probe"
            for method in ("close", "fsync", "atomic"):
                probe.write(path, "original", "fsync")
                self.assertTrue(probe.observe(path, "original")["immediate_match"])
                before = path.stat().st_ino
                probe.write(path, "replacement value", method)
                self.assertTrue(
                    probe.observe(path, "replacement value")["immediate_match"]
                )
                if method == "atomic":
                    self.assertNotEqual(before, path.stat().st_ino)

    def test_eventual_visibility_does_not_turn_initial_failure_into_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "probe"
            probe.write(path, "old", "fsync")
            old = probe.snapshot(path)
            probe.write(path, "new", "fsync")
            new = probe.snapshot(path)
            with (
                mock.patch.object(probe, "snapshot", side_effect=[old, new]),
                mock.patch.object(probe.time, "sleep"),
            ):
                result = probe.observe(path, "new")
            self.assertFalse(result["immediate_match"])
            self.assertTrue(result["samples"][-1]["matches"])

    def test_admission_content_errors_identify_the_failed_direction(self):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "probe"
            path.write_text("stale")
            for code in (
                "EngineCannotReadPrimaryWrite",
                "PrimaryCannotReadEngineOverwrite",
                "PrimaryCannotReadContainerWrite",
            ):
                with self.assertRaises(admission.WorkspaceCheckError) as error:
                    admission.require_contents(path, "expected", code)
                self.assertEqual(error.exception.code, code)
            path.write_text("container\n")
            admission.require_contents(
                path, "container", "PrimaryCannotReadContainerWrite", strip=True
            )


if __name__ == "__main__":
    unittest.main()
