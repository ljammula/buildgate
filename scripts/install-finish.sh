#!/bin/sh
# install-finish.sh <installed factoryd>: `make install`'s last step, what a
# new machine otherwise needs three more commands for.
#
#   Step     Does                                              Skipped when
#   PATH     appends `export PATH="$PATH:<bin dir>"` to the     `factoryd` already resolves on PATH,
#            shell profile (~/.zshrc, ~/.bash_profile)          in a new shell, or the profile names that directory
#   GitHub   `gh auth login`                                    gh is logged in, or there is no terminal
#   Check    `factoryd doctor -fix`                             never
#
# It never fails the install: factoryd is installed by the time it runs, and
# on a machine with no model route yet doctor is expected to report one
# failure, which `factoryd quickstart` closes.
set -u

installed="${1:-}"
if [ ! -x "$installed" ]; then
	echo "usage: install-finish.sh <installed factoryd> (not an executable: '$installed')" >&2
	exit 2
fi
bindir="$(dirname "$installed")"
have() { command -v "$1" >/dev/null 2>&1; }
# The profile may already put the directory on PATH under another spelling
# ($HOME/go/bin, $(go env GOPATH)/bin), which only a new shell can tell.
in_new_shell() { [ -x "${SHELL:-}" ] && "$SHELL" -ic 'command -v factoryd' </dev/null >/dev/null 2>&1; }

if ! have factoryd && ! in_new_shell; then
	line="export PATH=\"\$PATH:$bindir\""
	case "${SHELL##*/}" in
	zsh) profile="${ZDOTDIR:-$HOME}/.zshrc" ;;
	bash) profile="$HOME/.bash_profile" ;;
	*) profile="" ;;
	esac
	if [ -z "$profile" ]; then
		echo "note: 'factoryd' is not on your PATH yet. Add this to your shell profile: $line"
	elif [ -f "$profile" ] && grep -qF "$bindir" "$profile"; then
		echo "note: $profile already puts $bindir on PATH -- open a new terminal to use 'factoryd'"
	elif printf '\n# factoryd (buildgate make install)\n%s\n' "$line" >>"$profile"; then
		echo "Added $bindir to PATH in $profile -- open a new terminal, or run: $line"
	else
		echo "warning: could not write $profile. Add this to your shell profile: $line" >&2
	fi
fi

if have gh && ! gh auth status >/dev/null 2>&1; then
	if [ -t 0 ] && [ -t 1 ]; then
		echo "GitHub: gh is not logged in -- running gh auth login"
		gh auth login || echo "warning: gh auth login did not finish; run it before your first request" >&2
	else
		echo "note: gh is not logged in and this is not a terminal -- run 'gh auth login' before your first request"
	fi
fi

echo "Checking the install: factoryd doctor -fix"
if ! "$installed" doctor -fix; then
	echo "note: doctor reported the problems above. With no model route configured yet, 'factoryd quickstart <repo> \"<request>\"' writes one."
fi
exit 0
