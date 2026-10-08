#!/bin/sh
# install-finish.sh <installed factoryd>: `make install`'s last step, what a
# new machine otherwise needs three more commands for.
#
#   Step     Does                                              Skipped when
#   Link     symlinks ~/.local/bin/factoryd to the installed    the directory cannot be made or written,
#            binary, so every shell with that on PATH finds it  or another factoryd already sits there
#   PATH     appends `export PATH="$PATH:<bin dir>"` to the     `factoryd` already resolves on PATH (the link
#            shell profile (~/.zshrc, ~/.bash_profile)          does that), in a new shell, or the profile names that directory
#   GitHub   `gh auth login`                                    gh is logged in, or there is no terminal
#   Model    `factoryd setup`: which model, and which coding    the config already names a model;
#            agent where the route can run more than one        asks nothing with no terminal
#   Restart  `factoryd restart`: the worker and console that    nothing is running; left for you while a
#            were running, started again with this binary       request is building
#   Check    `factoryd doctor -fix`                             never
#
# It never fails the install: factoryd is installed by the time it runs.
# Whatever a step could not finish goes into one numbered "Left for you"
# list printed last, after doctor's output, with the next command under it.
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

# What is left for the operator, one line each, printed as a numbered list
# after everything else so it is the last thing on the screen.
todo=""
left() { todo="$todo$1
"; }

# A profile line reaches interactive shells only. An agent, a script or an ssh
# command runs factoryd from a shell that never reads the profile; ~/.local/bin
# is the user's own bin directory and is on PATH for those wherever the user's
# other tools live there. A factoryd there that is not this link (a
# hand-placed build) is not ours to replace.
link_into_user_bin() {
	linkdir="$HOME/.local/bin"
	[ "$linkdir" != "$bindir" ] || return 0
	mkdir -p "$linkdir" 2>/dev/null || return 1
	[ -w "$linkdir" ] || return 1
	link="$linkdir/factoryd"
	[ "$link" -ef "$installed" ] && return 0
	# -e follows the link: a dangling one (its binary was uninstalled) is replaced.
	[ ! -e "$link" ] || return 1
	ln -sfn "$installed" "$link" || return 1
	echo "Linked $link -> $installed"
}
link_into_user_bin || true

if ! have factoryd && ! in_new_shell; then
	line="export PATH=\"\$PATH:$bindir\""
	case "${SHELL##*/}" in
	zsh) profile="${ZDOTDIR:-$HOME}/.zshrc" ;;
	bash) profile="$HOME/.bash_profile" ;;
	*) profile="" ;;
	esac
	if [ -z "$profile" ]; then
		left "Add this line to your shell profile, then open a new terminal: $line"
	elif [ -f "$profile" ] && grep -qF "$bindir" "$profile"; then
		left "Open a new terminal, so 'factoryd' is on your PATH."
	elif printf '\n# factoryd (buildgate make install)\n%s\n' "$line" >>"$profile"; then
		echo "Added $bindir to PATH in $profile"
		left "Open a new terminal, so 'factoryd' is on your PATH ($profile now adds it)."
	else
		left "Add this line to your shell profile (it could not be written to $profile), then open a new terminal: $line"
	fi
fi

if have gh && ! gh auth status >/dev/null 2>&1; then
	if [ -t 0 ] && [ -t 1 ]; then
		echo "GitHub: gh is not logged in -- running gh auth login"
		gh auth login || left "Log in to GitHub: gh auth login (it did not finish just now)."
	else
		left "Log in to GitHub: gh auth login"
	fi
fi

"$installed" setup || left "Choose a model: factoryd setup"

# A worker builds with the code it started with: the one already running is
# the binary this install replaced until it is started again.
"$installed" restart || left "Restart the worker and console with this install, once no request is building: factoryd restart"

echo "Checking the install: factoryd doctor -fix"
"$installed" doctor -fix || left "Fix what the check above marks FAIL (each has a 'fix:' line), then run: factoryd doctor"

echo
echo "buildgate is installed."
if [ -n "$todo" ]; then
	echo "Left for you:"
	n=0
	printf '%s' "$todo" | while IFS= read -r item; do
		n=$((n + 1))
		echo "  $n. $item"
	done
	echo "Then run your first request: factoryd quickstart <repo> \"<request>\""
else
	echo "Nothing is left to do. Run your first request: factoryd quickstart <repo> \"<request>\""
fi
exit 0
