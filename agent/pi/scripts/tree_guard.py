"""Records a directory tree so that two records can be compared, and puts a
changed tree back where it can.

build_app.py uses it around the one model turn it runs after a build's checks
have passed (the notes turn): the tree is recorded, the turn runs, the tree is
recorded again. Equal records mean the tree the checks passed on is, byte for
byte, the tree the build hands back, whatever the turn was able to do. The
comparison is made by this process from what is on disk; it does not depend
on the coding agent obeying its prompt or on a harness flag.

A record holds, for every path under the root that is not below an excluded
top-level name: its kind, its permission bits, and the SHA-256 of a regular
file's content or a symbolic link's target. Tracked, untracked and ignored
files are all in it (git is not asked), as are empty directories. A link is
never followed. Times, owners and extended attributes are not recorded.

`record` refuses (TooLarge) a tree past MAX_ENTRIES, MAX_BYTES of file content
or MAX_SECONDS, so the caller can skip the turn instead of running it
unguarded. `backup` copies chosen regular files aside, up to a byte bound, and
`restore` uses those copies. Neither is trusted: the caller records the tree
again after a restore and compares.
"""
from __future__ import annotations

import hashlib
import os
import stat
import time
from pathlib import Path

MAX_ENTRIES = 250_000
MAX_BYTES = 4 << 30
MAX_SECONDS = 20.0
_CHUNK = 1 << 20


class TooLarge(Exception):
	"""The tree is past a bound; str() is the reason, with no path in it."""


class Record:
	"""entries maps a path relative to the root (POSIX separators) to
	(kind, permission bits, detail): kind "f" with the content's SHA-256,
	"l" with the link text (bits 0: a link's own are not portable), "d" with
	"", or "o" (a fifo, socket or device) with its file type and device."""

	def __init__(self) -> None:
		self.entries: dict[str, tuple] = {}
		self.files = 0
		self.bytes = 0
		self.seconds = 0.0


def _hash_file(path: str) -> tuple[str, int]:
	"""SHA-256 and length of the regular file at path, opened without
	following a link and without blocking on something that is no longer a
	regular file."""
	fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
	try:
		if not stat.S_ISREG(os.fstat(fd).st_mode):
			raise OSError("not a regular file")
		digest = hashlib.sha256()
		size = 0
		while True:
			chunk = os.read(fd, _CHUNK)
			if not chunk:
				break
			digest.update(chunk)
			size += len(chunk)
		return digest.hexdigest(), size
	finally:
		os.close(fd)


def record(root: Path, *, exclude: tuple[str, ...] = ()) -> Record:
	"""Records every path under root except the top-level names in exclude.
	Raises TooLarge past a bound and OSError when a path cannot be read."""
	started = time.monotonic()
	rec = Record()
	base = os.fspath(root)
	pending = [""]
	while pending:
		rel_dir = pending.pop()
		with os.scandir(os.path.join(base, rel_dir) if rel_dir else base) as listing:
			children = list(listing)
		for child in children:
			if not rel_dir and child.name in exclude:
				continue
			rel = f"{rel_dir}/{child.name}" if rel_dir else child.name
			info = child.stat(follow_symlinks=False)
			bits = stat.S_IMODE(info.st_mode)
			if stat.S_ISLNK(info.st_mode):
				rec.entries[rel] = ("l", 0, os.readlink(child.path))
			elif stat.S_ISDIR(info.st_mode):
				rec.entries[rel] = ("d", bits, "")
				pending.append(rel)
			elif stat.S_ISREG(info.st_mode):
				digest, size = _hash_file(child.path)
				rec.entries[rel] = ("f", bits, digest)
				rec.files += 1
				rec.bytes += size
			else:
				rec.entries[rel] = ("o", bits, f"{stat.S_IFMT(info.st_mode)}:{info.st_rdev}")
			if len(rec.entries) > MAX_ENTRIES:
				raise TooLarge(f"more than {MAX_ENTRIES} entries")
			if rec.bytes > MAX_BYTES:
				raise TooLarge(f"more than {MAX_BYTES} bytes of file content")
			if time.monotonic() - started > MAX_SECONDS:
				raise TooLarge(f"took longer than {MAX_SECONDS:g} s")
	rec.seconds = time.monotonic() - started
	return rec


def changed_paths(before: Record, after: Record) -> list[str]:
	"""The paths that are in one record and not the other, or differ."""
	names = set(before.entries) | set(after.entries)
	return sorted(name for name in names if before.entries.get(name) != after.entries.get(name))


def backup(root: Path, rec: Record, paths, dest: Path, *, max_bytes: int) -> dict[str, str]:
	"""Copies the regular files of rec named in paths into dest, in order,
	while their total stays within max_bytes. Returns path -> copy. A file
	that cannot be copied is left out."""
	copies: dict[str, str] = {}
	total = 0
	base = os.fspath(root)
	for number, rel in enumerate(paths):
		entry = rec.entries.get(rel)
		if entry is None or entry[0] != "f":
			continue
		source = os.path.join(base, rel)
		try:
			size = os.lstat(source).st_size
			if total + size > max_bytes:
				continue
			target = os.path.join(os.fspath(dest), str(number))
			_copy(source, target, 0o600)
			total += size
			copies[rel] = target
		except OSError:
			continue
	return copies


def _copy(source: str, target: str, bits: int) -> None:
	"""Writes source's bytes to a new file at target (which must not exist)
	with the given permission bits, following no link at either end."""
	src = os.open(source, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
	try:
		if not stat.S_ISREG(os.fstat(src).st_mode):
			raise OSError("not a regular file")
		dst = os.open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
		try:
			while True:
				chunk = os.read(src, _CHUNK)
				if not chunk:
					break
				view = memoryview(chunk)
				while view:
					view = view[os.write(dst, view):]
			os.fchmod(dst, bits)
		finally:
			os.close(dst)
	finally:
		os.close(src)


def _remove(path: str) -> None:
	info = os.lstat(path)
	if stat.S_ISDIR(info.st_mode):
		os.chmod(path, 0o700)
		os.rmdir(path)
	else:
		os.unlink(path)


def restore(root: Path, before: Record, after: Record, copies: dict[str, str]) -> None:
	"""Moves the tree `after` describes back towards `before`: paths that
	were added or changed kind or content are removed (deepest first, a link
	unlinked and never followed), then directories, links and the files that
	have a copy are created again (shallowest first), then permission bits
	are set. Every step is attempted and no failure is raised: what could not
	be put back shows when the caller records the tree again."""
	base = os.fspath(root)
	changed = changed_paths(before, after)

	def same_but_for_bits(name: str) -> bool:
		old, new = before.entries.get(name), after.entries.get(name)
		return old is not None and new is not None and (old[0], old[2]) == (new[0], new[2])

	for name in sorted((n for n in changed if n in after.entries and not same_but_for_bits(n)), key=lambda n: (-n.count("/"), n)):
		try:
			_remove(os.path.join(base, name))
		except OSError:
			pass
	for name in sorted((n for n in changed if n in before.entries and not same_but_for_bits(n)), key=lambda n: (n.count("/"), n)):
		kind, bits, detail = before.entries[name]
		path = os.path.join(base, name)
		try:
			if kind == "d":
				os.mkdir(path, 0o700)
			elif kind == "l":
				os.symlink(detail, path)
			elif kind == "f" and name in copies:
				_copy(copies[name], path, bits)
		except OSError:
			pass
	# Bits last, deepest first: a directory may have to stay writable until
	# what is inside it is back.
	for name in sorted((n for n in changed if n in before.entries), key=lambda n: (-n.count("/"), n)):
		kind, bits, _ = before.entries[name]
		path = os.path.join(base, name)
		try:
			if kind in ("d", "f") and not os.path.islink(path):
				os.chmod(path, bits)
		except OSError:
			pass
