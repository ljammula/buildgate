"""Offline tests for scripts/install-finish.sh, `make install`'s last step.

Each test runs the script with PATH holding only a temp directory with the
utilities it needs and stand-in `gh` and `factoryd`; HOME is a temp directory
too, so the shell profile it edits is the test's own.
"""

import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "install-finish.sh"
UTILITIES = ["sh", "dirname", "grep", "mkdir", "ln"]


class InstallFinishTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.bin = self.tmp / "bin"
        self.home = self.tmp / "home"
        self.gobin = self.tmp / "go" / "bin"
        self.userbin = self.home / ".local" / "bin"
        for d in (self.bin, self.home, self.gobin):
            d.mkdir(parents=True)
        for name in UTILITIES:
            os.symlink(shutil.which(name), self.bin / name)
        self.log = self.tmp / "calls.log"
        self.installed = self.gobin / "factoryd"
        self.stub(self.installed, 'echo "factoryd $*" >> "$LOG"; [ "$1" = setup ] && exit ${SETUP_EXIT:-0}; exit ${DOCTOR_EXIT:-0}')
        self.stub(self.bin / "gh", 'echo "gh $*" >> "$LOG"; [ "$1 $2" = "auth status" ] && exit ${GH_STATUS:-0}; exit 0')

    def stub(self, path, body):
        path.write_text("#!/bin/sh\n" + body + "\n")
        path.chmod(path.stat().st_mode | stat.S_IXUSR)

    def run_script(self, path=None, **extra):
        env = {"PATH": path or str(self.bin), "HOME": str(self.home), "SHELL": "/bin/zsh", "LOG": str(self.log), **extra}
        return subprocess.run(
            [str(self.bin / "sh"), str(SCRIPT), str(self.installed)],
            env=env, capture_output=True, text=True, stdin=subprocess.DEVNULL,
        )

    def calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def test_adds_the_bin_directory_to_the_shell_profile_once(self):
        line = f'export PATH="$PATH:{self.gobin}"'
        first = self.run_script()
        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertIn(line, (self.home / ".zshrc").read_text())
        self.assertIn("Open a new terminal", first.stdout.split("Left for you:")[1])
        self.run_script()
        self.assertEqual((self.home / ".zshrc").read_text().count(line), 1)

    def test_keeps_what_the_profile_already_holds(self):
        (self.home / ".zshrc").write_text("alias k=kubectl\n")
        self.run_script()
        self.assertTrue((self.home / ".zshrc").read_text().startswith("alias k=kubectl\n"))

    @unittest.skipUnless(os.access("/bin/zsh", os.X_OK), "needs zsh")
    def test_a_profile_that_spells_the_directory_another_way_is_left_alone(self):
        (self.home / "go").symlink_to(self.tmp / "go")
        profile = 'export PATH="$PATH:$HOME/go/bin"\n'
        (self.home / ".zshrc").write_text(profile)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.home / ".zshrc").read_text(), profile)

    def test_bash_uses_bash_profile(self):
        self.run_script(SHELL="/bin/bash")
        self.assertIn(str(self.gobin), (self.home / ".bash_profile").read_text())
        self.assertFalse((self.home / ".zshrc").exists())

    def test_leaves_the_profile_alone_when_factoryd_is_on_path(self):
        result = self.run_script(path=f"{self.bin}:{self.gobin}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.home / ".zshrc").exists())

    def user_bin_on_path(self):
        """PATH with ~/.local/bin on it, as a machine whose tools live there has."""
        return f"{self.bin}:{self.userbin}"

    def test_links_into_the_user_bin_and_leaves_the_profile_alone(self):
        result = self.run_script(path=self.user_bin_on_path())
        self.assertEqual(result.returncode, 0, result.stderr)
        link = self.userbin / "factoryd"
        self.assertTrue(link.is_symlink())
        self.assertEqual(link.resolve(), self.installed.resolve())
        self.assertFalse((self.home / ".zshrc").exists())
        self.assertIn("Nothing is left to do", result.stdout)
        again = self.run_script(path=self.user_bin_on_path())
        self.assertEqual(again.returncode, 0, again.stderr)
        self.assertNotIn("Linked", again.stdout)

    def test_links_even_when_factoryd_already_resolves(self):
        self.run_script(path=f"{self.bin}:{self.gobin}")
        self.assertTrue((self.userbin / "factoryd").is_symlink())

    def test_a_user_bin_not_on_path_still_gets_the_profile_line(self):
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.userbin / "factoryd").is_symlink())
        self.assertIn(str(self.gobin), (self.home / ".zshrc").read_text())

    def test_another_factoryd_in_the_user_bin_is_left_alone(self):
        self.userbin.mkdir(parents=True)
        self.stub(self.userbin / "factoryd", "exit 0")
        before = (self.userbin / "factoryd").read_text()
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.userbin / "factoryd").is_symlink())
        self.assertEqual((self.userbin / "factoryd").read_text(), before)
        self.assertIn(str(self.gobin), (self.home / ".zshrc").read_text())

    def test_a_dangling_link_is_replaced(self):
        self.userbin.mkdir(parents=True)
        os.symlink(self.tmp / "gone" / "factoryd", self.userbin / "factoryd")
        self.run_script(path=self.user_bin_on_path())
        self.assertEqual((self.userbin / "factoryd").resolve(), self.installed.resolve())

    def test_a_user_bin_that_cannot_be_written_falls_back_to_the_profile(self):
        self.userbin.mkdir(parents=True)
        self.userbin.chmod(0o555)
        self.addCleanup(self.userbin.chmod, 0o755)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.userbin / "factoryd").exists())
        self.assertIn(str(self.gobin), (self.home / ".zshrc").read_text())

    def test_an_unknown_shell_gets_the_line_printed(self):
        result = self.run_script(SHELL="/usr/bin/fish")
        self.assertIn(f'export PATH="$PATH:{self.gobin}"', result.stdout.split("Left for you:")[1])
        self.assertEqual([p.name for p in self.home.iterdir()], [".local"])

    def test_runs_doctor_fix_and_a_failing_doctor_does_not_fail_the_install(self):
        result = self.run_script(DOCTOR_EXIT="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("factoryd doctor -fix", self.calls())
        self.assertIn("marks FAIL", result.stdout.split("Left for you:")[1])

    def test_asks_for_the_model_before_the_check(self):
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = [c for c in self.calls() if c.startswith("factoryd")]
        self.assertEqual(calls, ["factoryd setup", "factoryd doctor -fix"])

    def test_a_setup_that_could_not_pick_a_model_is_named_and_not_fatal(self):
        result = self.run_script(SETUP_EXIT="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Choose a model: factoryd setup", result.stdout.split("Left for you:")[1])
        self.assertIn("factoryd doctor -fix", self.calls())

    def test_refuses_a_path_that_is_not_the_installed_binary(self):
        self.installed = self.gobin / "missing"
        result = self.run_script()
        self.assertEqual(result.returncode, 2)
        self.assertEqual(self.calls(), [])
        self.assertFalse((self.home / ".zshrc").exists())

    def test_logged_in_gh_is_not_asked_again(self):
        self.run_script()
        self.assertEqual([c for c in self.calls() if c.startswith("gh")], ["gh auth status"])

    def test_without_a_terminal_a_logged_out_gh_gets_a_note_not_a_prompt(self):
        result = self.run_script(GH_STATUS="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("gh auth login", self.calls())
        self.assertIn("Log in to GitHub: gh auth login", result.stdout.split("Left for you:")[1])

    def test_the_last_lines_are_a_numbered_list_of_what_is_left(self):
        result = self.run_script(GH_STATUS="1", SETUP_EXIT="1", DOCTOR_EXIT="1")
        self.assertEqual(result.returncode, 0, result.stderr)
        tail = result.stdout.split("buildgate is installed.\n")[1].splitlines()
        self.assertEqual(tail[0], "Left for you:")
        self.assertEqual([line[:5] for line in tail[1:5]], ["  1. ", "  2. ", "  3. ", "  4. "])
        self.assertTrue(tail[1].startswith("  1. Open a new terminal"))
        self.assertTrue(tail[5].startswith("Then run your first request: factoryd quickstart"))
        self.assertEqual(len(tail), 6)

    def test_with_nothing_left_it_says_so(self):
        result = self.run_script(path=f"{self.bin}:{self.gobin}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("Left for you:", result.stdout)
        self.assertTrue(result.stdout.rstrip().endswith('Nothing is left to do. Run your first request: factoryd quickstart <repo> "<request>"'))


if __name__ == "__main__":
    unittest.main()
