import importlib.util
import pathlib
import tempfile
import unittest

spec = importlib.util.spec_from_file_location(
    "workspace", pathlib.Path(__file__).with_name("workspace.py")
)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class CleanupTest(unittest.TestCase):
    def test_child_removal_retains_root_and_other_allocations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            (root / "jobs").mkdir()
            job = root / "jobs" / "ours"
            job.mkdir()
            (job / "file").write_text("secret")
            other = root / "jobs" / "other"
            other.mkdir()
            (other / "file").write_text("preserve")
            inode = root.stat().st_ino
            module.clear_job(job, root, inode)
            self.assertEqual(inode, root.stat().st_ino)
            self.assertFalse(job.exists())
            self.assertEqual((other / "file").read_text(), "preserve")

    def test_symlinked_jobs_directory_cannot_remove_external_data(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            root.mkdir()
            outside = pathlib.Path(directory) / "outside"
            outside.mkdir()
            job = outside / "victim"
            job.mkdir()
            (job / "file").write_text("preserve")
            (root / "jobs").symlink_to(outside, target_is_directory=True)
            with self.assertRaises(RuntimeError):
                module.clear_job(root / "jobs" / "victim", root, root.stat().st_ino)
            self.assertEqual((job / "file").read_text(), "preserve")

    def test_changed_root_identity_refuses_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory) / "root"
            root.mkdir()
            (root / "jobs").mkdir()
            inode = root.stat().st_ino
            root.rename(root.with_name("old"))
            root.mkdir()
            (root / "jobs").mkdir()
            job = root / "jobs" / "ours"
            job.mkdir()
            with self.assertRaises(RuntimeError):
                module.clear_job(job, root, inode)
            self.assertTrue(job.exists())


if __name__ == "__main__":
    unittest.main()
