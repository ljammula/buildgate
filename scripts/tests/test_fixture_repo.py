"""Offline tests for scripts/fixture-repo.sh: a fixture folder tracked inside
this repo becomes a fresh one-commit repo, and an operator's own git repo is
cloned. Run: python3 -m unittest discover -s scripts/tests -p 'test_fixture_repo.py'
"""

import os
import subprocess
import tempfile
import unittest

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
SCRIPT = os.path.join(REPO_ROOT, "scripts", "fixture-repo.sh")


def git(repo, *args):
    return subprocess.run(["git", "-C", repo, *args], check=True, capture_output=True, text=True).stdout.strip()


class FixtureRepoTest(unittest.TestCase):
    def test_tracked_fixture_becomes_its_own_one_commit_repo(self):
        # testdata/fixtures/math_ops sits inside this repo's work tree; it
        # must be copied and committed, not cloned as this whole repo.
        src = os.path.join(REPO_ROOT, "testdata", "fixtures", "math_ops")
        with tempfile.TemporaryDirectory() as tmp:
            shas = []
            for name in ("a", "b"):
                dest = os.path.join(tmp, name)
                subprocess.run([SCRIPT, src, dest], check=True)
                self.assertEqual(git(dest, "rev-parse", "--show-toplevel"), os.path.realpath(dest))
                self.assertEqual(sorted(git(dest, "ls-files").splitlines()), [".gitignore", "add.py"])
                self.assertEqual(git(dest, "rev-list", "--count", "HEAD"), "1")
                self.assertEqual(git(dest, "status", "--porcelain"), "")
                self.assertEqual(
                    git(dest, "log", "-1", "--format=%an %ae %aI %cI %s"),
                    "buildgate-fixture fixture@example.invalid 2026-01-01T00:00:00Z 2026-01-01T00:00:00Z fixture: math_ops",
                )
                shas.append(git(dest, "rev-parse", "HEAD"))
            self.assertEqual(shas[0], shas[1], "same files must give the same commit SHA")

    def test_git_repo_is_cloned_with_its_history(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = os.path.join(tmp, "src")
            os.makedirs(src)
            subprocess.run(["git", "init", "-q", src], check=True)
            git(src, "config", "user.email", "t@example.com")
            git(src, "config", "user.name", "t")
            for i in range(2):
                with open(os.path.join(src, "f.txt"), "w") as f:
                    f.write(str(i))
                git(src, "add", ".")
                git(src, "commit", "-q", "-m", "c%d" % i)
            dest = os.path.join(tmp, "dest")
            subprocess.run([SCRIPT, src, dest], check=True)
            self.assertEqual(git(dest, "rev-parse", "HEAD"), git(src, "rev-parse", "HEAD"))
            self.assertEqual(git(dest, "rev-list", "--count", "HEAD"), "2")

    def test_existing_dest_is_refused(self):
        src = os.path.join(REPO_ROOT, "testdata", "fixtures", "math_ops")
        with tempfile.TemporaryDirectory() as tmp:
            r = subprocess.run([SCRIPT, src, tmp], capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("already exists", r.stderr)

    def test_missing_source_fails(self):
        with tempfile.TemporaryDirectory() as tmp:
            r = subprocess.run([SCRIPT, os.path.join(tmp, "nope"), os.path.join(tmp, "dest")], capture_output=True, text=True)
            self.assertNotEqual(r.returncode, 0)
            self.assertIn("not a directory", r.stderr)


if __name__ == "__main__":
    unittest.main()
