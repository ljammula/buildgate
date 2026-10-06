"""Offline tests for scripts/with-spinner.sh: exit-status passthrough, the
plain (non-terminal) path, and the terminal path's animation, marks and
failure tail. The terminal path runs under a pseudo-terminal on stderr."""

import os
import pty
import subprocess
import unittest

SCRIPT = os.path.join(os.path.dirname(__file__), "..", "with-spinner.sh")


def run_plain(*cmd):
    return subprocess.run([SCRIPT, *cmd], capture_output=True, text=True)


def run_tty(*cmd):
    """Run with stderr on a pty; return (exit status, stdout, terminal text)."""
    master, slave = pty.openpty()
    proc = subprocess.Popen([SCRIPT, *cmd], stdout=subprocess.PIPE, stderr=slave, text=True)
    os.close(slave)
    chunks = []
    while True:
        try:
            data = os.read(master, 4096)
        except OSError:  # EIO once the slave side closes on Linux
            break
        if not data:
            break
        chunks.append(data)
    os.close(master)
    out, _ = proc.communicate()
    return proc.returncode, out, b"".join(chunks).decode()


class WithSpinnerPlain(unittest.TestCase):
    def test_runs_command_untouched_and_passes_status(self):
        r = run_plain("busy", "sh", "-c", "echo out; echo err >&2; exit 3")
        self.assertEqual(r.returncode, 3)
        self.assertEqual(r.stdout, "out\n")
        self.assertEqual(r.stderr, "err\n")

    def test_success_prints_nothing_extra(self):
        r = run_plain("busy", "true")
        self.assertEqual((r.returncode, r.stdout, r.stderr), (0, "", ""))

    def test_usage_error(self):
        r = run_plain("only-words")
        self.assertEqual(r.returncode, 2)
        self.assertIn("usage", r.stderr)


class WithSpinnerTerminal(unittest.TestCase):
    def test_success_animates_then_marks_check(self):
        code, out, term = run_tty("building the thing", "sh", "-c", "echo noisy; sleep 0.5")
        self.assertEqual(code, 0)
        self.assertEqual(out, "")
        self.assertIn("building the thing", term)
        self.assertRegex(term, "[⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏] building the thing · ")
        self.assertIn("✓ building the thing", term)
        self.assertNotIn("noisy", term)  # captured in the log, dropped on success

    def test_failure_passes_status_and_prints_log_tail(self):
        script = "for i in $(seq 1 60); do echo line$i; done; exit 7"
        code, _, term = run_tty("compiling", "sh", "-c", script)
        self.assertEqual(code, 7)
        self.assertIn("✗ compiling", term)
        self.assertIn("(exit 7)", term)
        self.assertIn("line60", term)
        self.assertIn("line21", term)
        self.assertNotIn("line20\r", term)  # only the last 40 lines


if __name__ == "__main__":
    unittest.main()
