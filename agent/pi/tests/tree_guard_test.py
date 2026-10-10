import importlib.util
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "tree_guard.py"
SPEC = importlib.util.spec_from_file_location("tree_guard", SCRIPT)
assert SPEC and SPEC.loader
tree_guard = importlib.util.module_from_spec(SPEC)
sys.modules.setdefault(SPEC.name, tree_guard)
SPEC.loader.exec_module(tree_guard)


def make_tree(root: Path) -> None:
	(root / ".git").mkdir()
	(root / ".git" / "HEAD").write_text("ref: refs/heads/main\n")
	(root / ".session").mkdir()
	(root / ".session" / "log").write_text("turn 1\n")
	(root / "a.txt").write_text("a\n")
	(root / "run.sh").write_text("#!/bin/sh\n")
	os.chmod(root / "run.sh", 0o755)
	(root / "dir" / "sub").mkdir(parents=True)
	(root / "dir" / "sub" / "b.bin").write_bytes(b"\x00\x01" * 700_000)
	(root / "dir" / "empty").mkdir()
	os.symlink("a.txt", root / "link")
	os.symlink("/nonexistent/target", root / "dangling")
	(root / "sub").mkdir()
	(root / "sub" / ".git").mkdir()
	(root / "sub" / ".git" / "config").write_text("nested\n")


class RecordTests(unittest.TestCase):
	def setUp(self):
		self._tmp = tempfile.TemporaryDirectory()
		self.root = Path(self._tmp.name).resolve()
		make_tree(self.root)
		self.addCleanup(self._tmp.cleanup)

	def record(self):
		return tree_guard.record(self.root, exclude=(".git", ".session"))

	def test_records_every_path_outside_the_excluded_top_level_names(self):
		rec = self.record()
		self.assertEqual(sorted(rec.entries), sorted([
			"a.txt", "run.sh", "dir", "dir/sub", "dir/sub/b.bin", "dir/empty", "link", "dangling",
			"sub", "sub/.git", "sub/.git/config",
		]))
		self.assertEqual(rec.entries["link"], ("l", 0, "a.txt"))
		self.assertEqual(rec.entries["dangling"], ("l", 0, "/nonexistent/target"))
		self.assertEqual(rec.entries["run.sh"][:2], ("f", 0o755))
		self.assertEqual(rec.entries["dir/empty"][0], "d")
		self.assertEqual(len(rec.entries["a.txt"][2]), 64)
		self.assertEqual(rec.files, 4)
		self.assertEqual(rec.bytes, 2 + 10 + 1_400_000 + 7)

	def test_an_unchanged_tree_records_the_same(self):
		self.assertEqual(tree_guard.changed_paths(self.record(), self.record()), [])

	def test_a_change_in_an_excluded_folder_is_not_a_change(self):
		before = self.record()
		(self.root / ".session" / "log").write_text("turn 2\n")
		(self.root / ".git" / "index").write_text("x")
		self.assertEqual(tree_guard.changed_paths(before, self.record()), [])

	def test_each_kind_of_change_is_seen(self):
		def retarget(root):
			(root / "link").unlink()
			os.symlink("run.sh", root / "link")

		def link_for_dir(root):
			(root / "dir" / "empty").rmdir()
			os.symlink("sub", root / "dir" / "empty")

		def same_size_same_mtime(root):
			path = root / "a.txt"
			info = path.stat()
			path.write_text("b\n")
			os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns))

		def fifo(root):
			os.mkfifo(root / "pipe")

		cases = {
			"content": (lambda r: (r / "a.txt").write_text("changed\n"), ["a.txt"]),
			"content with size and time kept": (same_size_same_mtime, ["a.txt"]),
			"the last byte of a large file": (lambda r: (r / "dir" / "sub" / "b.bin").write_bytes(b"\x00\x01" * 699_999 + b"\x00\x02"), ["dir/sub/b.bin"]),
			"mode": (lambda r: os.chmod(r / "a.txt", 0o600), ["a.txt"]),
			"directory mode": (lambda r: os.chmod(r / "dir" / "empty", 0o700), ["dir/empty"]),
			"added file": (lambda r: (r / "dir" / "new").write_text(""), ["dir/new"]),
			"added empty directory": (lambda r: (r / "newdir").mkdir(), ["newdir"]),
			"removed file": (lambda r: (r / "run.sh").unlink(), ["run.sh"]),
			"link target": (retarget, ["link"]),
			"file replaced by a link to itself elsewhere": (lambda r: ((r / "a.txt").rename(r / "moved"), os.symlink("moved", r / "a.txt")), ["a.txt", "moved"]),
			"directory replaced by a link": (link_for_dir, ["dir/empty"]),
			"a nested .git": (lambda r: (r / "sub" / ".git" / "config").write_text("hooks\n"), ["sub/.git/config"]),
			"a fifo": (fifo, ["pipe"]),
		}
		for name, (change, want) in cases.items():
			with self.subTest(name):
				with tempfile.TemporaryDirectory() as tmp:
					root = Path(tmp).resolve()
					make_tree(root)
					before = tree_guard.record(root, exclude=(".git", ".session"))
					change(root)
					after = tree_guard.record(root, exclude=(".git", ".session"))
					self.assertEqual(tree_guard.changed_paths(before, after), sorted(want))

	def test_a_tree_over_a_bound_is_refused(self):
		for name, patch, reason in (
			("entries", {"MAX_ENTRIES": 4}, "more than 4 entries"),
			("bytes", {"MAX_BYTES": 1000}, "more than 1000 bytes of file content"),
			("seconds", {"MAX_SECONDS": -1.0}, "took longer than -1 s"),
		):
			with self.subTest(name), mock.patch.multiple(tree_guard, **patch):
				with self.assertRaises(tree_guard.TooLarge) as raised:
					self.record()
				self.assertEqual(str(raised.exception), reason)

	def test_a_link_is_never_followed(self):
		outside = tempfile.TemporaryDirectory()
		self.addCleanup(outside.cleanup)
		(Path(outside.name) / "big").write_bytes(b"x" * 4096)
		os.symlink(outside.name, self.root / "out")
		rec = self.record()
		self.assertEqual(rec.entries["out"][0], "l")
		self.assertNotIn("out/big", rec.entries)


class RestoreTests(unittest.TestCase):
	def setUp(self):
		self._tmp = tempfile.TemporaryDirectory()
		self._backup = tempfile.TemporaryDirectory()
		self.root = Path(self._tmp.name).resolve()
		make_tree(self.root)
		self.addCleanup(self._tmp.cleanup)
		self.addCleanup(self._backup.cleanup)

	def record(self):
		return tree_guard.record(self.root, exclude=(".git", ".session"))

	def put_back(self, change, *, copy=("a.txt", "run.sh", "dir/sub/b.bin", "sub/.git/config"), max_bytes=1 << 30):
		before = self.record()
		copies = tree_guard.backup(self.root, before, copy, Path(self._backup.name), max_bytes=max_bytes)
		change(self.root)
		after = self.record()
		self.attempted = tree_guard.restore(self.root, before, after, copies)
		self.after_turn = after
		return tree_guard.changed_paths(before, self.record())

	def assert_left_as_the_turn_left_it(self):
		self.assertIs(self.attempted, False)
		self.assertEqual(tree_guard.changed_paths(self.after_turn, self.record()), [])

	def test_changes_are_put_back_exactly(self):
		def many(root):
			(root / "a.txt").write_text("changed\n")
			os.chmod(root / "run.sh", 0o600)
			(root / "dir" / "sub" / "b.bin").unlink()
			(root / "dir" / "sub").rmdir()
			os.symlink("/etc", root / "dir" / "sub")
			(root / "link").unlink()
			(root / "link").mkdir()
			(root / "link" / "inner").write_text("x")
			(root / "added" / "deep").mkdir(parents=True)
			(root / "added" / "deep" / "f").write_text("x")
			(root / "dir" / "empty").rmdir()
			(root / "dangling").unlink()
			(root / "dangling").write_text("now a file")

		self.assertEqual(self.put_back(many), [])
		self.assertTrue(os.path.isdir("/etc"), "a link out of the tree was removed, not followed")
		self.assertEqual((self.root / "dir" / "sub" / "b.bin").stat().st_size, 1_400_000)
		self.assertEqual(stat.S_IMODE((self.root / "run.sh").stat().st_mode), 0o755)

	def test_a_file_that_was_not_copied_stays_changed(self):
		left = self.put_back(lambda r: ((r / "a.txt").write_text("changed\n"), (r / "run.sh").write_text("x")), copy=("run.sh",))
		# All or nothing: one file cannot be put back, so none is touched.
		self.assertEqual(left, ["a.txt", "run.sh"])
		self.assertEqual((self.root / "a.txt").read_text(), "changed\n")
		self.assert_left_as_the_turn_left_it()

	def test_a_renamed_file_with_no_copy_keeps_its_content_under_the_new_name(self):
		left = self.put_back(lambda r: (r / "dir" / "sub" / "b.bin").rename(r / "b2.bin"), copy=("a.txt",))
		self.assertEqual(left, ["b2.bin", "dir/sub/b.bin"])
		self.assertEqual((self.root / "b2.bin").stat().st_size, 1_400_000)
		self.assert_left_as_the_turn_left_it()

	def test_a_renamed_directory_with_no_copies_is_left_whole_under_the_new_name(self):
		left = self.put_back(lambda r: (r / "dir").rename(r / "moved"), copy=("a.txt",))
		self.assertEqual((self.root / "moved" / "sub" / "b.bin").stat().st_size, 1_400_000)
		self.assertFalse((self.root / "dir").exists(), "no empty directories are made under the old name")
		self.assertEqual(len(left), 8)
		self.assert_left_as_the_turn_left_it()

	def test_a_restore_that_can_undo_everything_says_so(self):
		left = self.put_back(lambda r: ((r / "a.txt").write_text("changed\n"), (r / "added").write_text("x"), (r / "dir" / "empty").rmdir()))
		self.assertEqual(left, [])
		self.assertIs(self.attempted, True)

	def test_a_file_past_the_copy_bound_is_left_as_changed_not_deleted(self):
		def change(root):
			with open(root / "dir" / "sub" / "b.bin", "ab") as handle:
				handle.write(b"!")

		left = self.put_back(change, max_bytes=100)
		self.assertEqual(left, ["dir/sub/b.bin"])
		self.assert_left_as_the_turn_left_it()
		self.assertEqual((self.root / "dir" / "sub" / "b.bin").stat().st_size, 1_400_001)

	def test_a_file_with_no_copy_that_became_something_else_is_left_whole(self):
		def change(root):
			(root / "a.txt").unlink()
			(root / "a.txt").mkdir()
			(root / "a.txt" / "inner").write_text("kept\n")
			(root / "run.sh").unlink()
			os.symlink("a.txt", root / "run.sh")

		left = self.put_back(change, copy=())
		self.assertEqual(left, ["a.txt", "a.txt/inner", "run.sh"])
		self.assertEqual((self.root / "a.txt" / "inner").read_text(), "kept\n")
		self.assertTrue(os.path.islink(self.root / "run.sh"))

	def test_a_deleted_file_with_no_copy_stops_the_whole_restore(self):
		left = self.put_back(lambda r: ((r / "a.txt").unlink(), (r / "added").write_text("x")), copy=())
		self.assertEqual(left, ["a.txt", "added"])
		self.assert_left_as_the_turn_left_it()

	def test_the_copy_stops_at_its_byte_bound(self):
		before = self.record()
		copies = tree_guard.backup(self.root, before, ("a.txt", "dir/sub/b.bin", "run.sh"), Path(self._backup.name), max_bytes=100)
		self.assertEqual(sorted(copies), ["a.txt", "run.sh"])

	def test_only_regular_files_of_the_record_are_copied(self):
		before = self.record()
		copies = tree_guard.backup(self.root, before, ("link", "dir", "missing", "../outside", "a.txt"), Path(self._backup.name), max_bytes=1 << 20)
		self.assertEqual(sorted(copies), ["a.txt"])

	def test_a_copy_altered_before_the_restore_does_not_pass_as_exact(self):
		before = self.record()
		copies = tree_guard.backup(self.root, before, ("a.txt",), Path(self._backup.name), max_bytes=1 << 20)
		(self.root / "a.txt").write_text("changed\n")
		Path(copies["a.txt"]).write_text("planted\n")
		tree_guard.restore(self.root, before, self.record(), copies)
		self.assertEqual(tree_guard.changed_paths(before, self.record()), ["a.txt"])
		# A copy that is no longer the recorded content is not used at all.
		self.assertEqual((self.root / "a.txt").read_text(), "changed\n")

	def test_a_restore_that_cannot_finish_raises_nothing(self):
		before = self.record()
		(self.root / "dir" / "sub" / "b.bin").unlink()
		os.chmod(self.root / "dir" / "sub", 0o500)
		self.addCleanup(os.chmod, self.root / "dir" / "sub", 0o755)
		copies = {}
		after = self.record()
		self.assertIs(tree_guard.restore(self.root, before, after, copies), False)
		self.assertEqual(tree_guard.changed_paths(after, self.record()), [])
		os.chmod(self.root / "dir" / "sub", 0o755)

	def test_an_attempted_restore_that_fails_part_way_raises_nothing(self):
		before = self.record()
		(self.root / "dir" / "sub" / "added").write_text("x")
		os.chmod(self.root / "dir" / "sub", 0o500)
		self.addCleanup(os.chmod, self.root / "dir" / "sub", 0o755)
		after = self.record()
		with mock.patch.object(tree_guard.os, "chmod", side_effect=PermissionError("x")):
			self.assertIs(tree_guard.restore(self.root, before, after, {}), True)
		self.assertNotEqual(tree_guard.changed_paths(before, self.record()), [])


if __name__ == "__main__":
	unittest.main()
