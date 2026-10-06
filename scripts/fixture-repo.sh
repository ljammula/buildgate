#!/usr/bin/env bash
# fixture-repo.sh SRC DEST -- give a live-validation script a disposable git
# repo at DEST to build against.
#
#   SRC a git work tree   -> `git clone --local` it (an operator's own repo)
#   SRC a plain directory -> copy it and commit it as one initial commit
#                            (a fixture under testdata/fixtures/, which this
#                            repo tracks as plain files, not as a nested repo)
#
# The commit identity and date are fixed, and the operator's git hooks and
# line-ending settings are off, so a fixture's commit SHA changes only when
# its files do.
set -euo pipefail

if [ "$#" -ne 2 ]; then
	echo "usage: fixture-repo.sh SRC DEST" >&2
	exit 2
fi
src="$1"
dest="$2"

[ -d "$src" ] || { echo "fixture-repo: $src is not a directory" >&2; exit 1; }
[ ! -e "$dest" ] || { echo "fixture-repo: $dest already exists" >&2; exit 1; }

if git -C "$src" rev-parse --is-inside-work-tree >/dev/null 2>&1 &&
	[ "$(git -C "$src" rev-parse --show-toplevel)" = "$(cd "$src" && pwd -P)" ]; then
	git clone --quiet --local "$src" "$dest"
	exit 0
fi

mkdir -p "$dest"
cp -R "$src/." "$dest/"
fixture_git() { git -C "$dest" -c core.hooksPath=/dev/null -c core.autocrlf=false -c commit.gpgsign=false "$@"; }
git init --quiet --template= -b main "$dest"
fixture_git add -A
GIT_AUTHOR_NAME=buildgate-fixture GIT_AUTHOR_EMAIL=fixture@example.invalid \
GIT_COMMITTER_NAME=buildgate-fixture GIT_COMMITTER_EMAIL=fixture@example.invalid \
GIT_AUTHOR_DATE=2026-01-01T00:00:00Z GIT_COMMITTER_DATE=2026-01-01T00:00:00Z \
	fixture_git commit --quiet --no-verify -m "fixture: $(basename "$src")"
