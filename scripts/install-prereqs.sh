#!/bin/sh
# install-prereqs.sh: `make install`'s first step. Installs, with Homebrew,
# the tools the rest of the install needs and this machine lacks, links
# Docker's buildx plugin and starts Docker, so a new Mac needs no
# `brew install` or `colima start` line first.
#
#   Tool                    Formula          Missing when
#   go, python3, gh, git    go python gh git the command is not on PATH
#   npm (the console)       node             the command is not on PATH
#   docker                  docker           the command is not on PATH
#   docker buildx           docker-buildx    `docker buildx version` fails
#   a Docker daemon         colima           neither docker nor colima is on PATH
#   terminal-notifier       terminal-notifier  macOS, and the command is not on PATH
#
# terminal-notifier is what makes a desktop notification's click open the
# request it is about. It is optional: without Homebrew, or when its install
# fails, the install goes on and the banner has no click. Once installed it is
# launched as an application one time, which is what makes macOS list it
# under Notifications and ask to allow it.
#
# A machine that already has a docker CLI keeps its own daemon (Docker
# Desktop, colima, another VM): colima is installed only beside a docker CLI
# this script installs. Nothing is upgraded or removed, and a tool already on
# PATH is left as it is, whatever installed it.
#
# Docker not answering is started only through colima, when colima is on
# PATH: `colima start` for a VM that already exists (its own settings), else
# `colima start --memory 4`. FACTORYD_AUTOSTART=0 turns the start off, as it
# does for factoryd's own. Any other Docker is the operator's to start;
# `make install` stops at the image build and says so.
#
# Without Homebrew it installs nothing: it names what is missing and fails,
# unless only npm is, which costs the console alone (`make install` then
# serves the placeholder page).
set -eu

missing=""
want() { missing="$missing $1"; }
have() { command -v "$1" >/dev/null 2>&1; }

# Homebrew installs the buildx plugin outside Docker's plugin directory;
# link it there when Docker does not find it.
link_buildx() {
	have docker && have brew || return 0
	docker buildx version >/dev/null 2>&1 && return 0
	plugin="$(brew --prefix)/lib/docker/cli-plugins/docker-buildx"
	[ -x "$plugin" ] || return 0
	mkdir -p "$HOME/.docker/cli-plugins"
	ln -sf "$plugin" "$HOME/.docker/cli-plugins/docker-buildx"
	echo "Linked docker buildx: ~/.docker/cli-plugins/docker-buildx"
}

have go || want go
have python3 || want python
have gh || want gh
have git || want git
have npm || want node
link_buildx # a formula already installed needs only its link
if ! have docker; then
	want docker
	want docker-buildx
	have colima || want colima
elif ! docker buildx version >/dev/null 2>&1; then
	want docker-buildx
fi
missing="${missing# }"

if [ -n "$missing" ]; then
	if ! have brew; then
		if [ "$missing" = node ]; then
			echo "npm not installed and no Homebrew to install it with -- factoryd will serve the console placeholder page" >&2
			exit 0
		fi
		echo "make install needs these, and found no Homebrew (https://brew.sh) to install them with: $missing" >&2
		echo "Install Homebrew or the tools themselves, then re-run 'make install'." >&2
		exit 1
	fi
	echo "Installing missing tools: brew install $missing"
	# shellcheck disable=SC2086 # one argument per formula
	brew install $missing
fi

link_buildx

if [ "$(uname -s 2>/dev/null || true)" = Darwin ] && ! have terminal-notifier; then
	if have brew && brew install terminal-notifier; then
		echo "Installed terminal-notifier: a notification's click opens the request it is about."
		# macOS lists an application under Notifications, and asks to allow
		# it, only once it has been launched as an application: running the
		# command from a shell is refused without a word to the operator.
		app="$(brew --prefix)/opt/terminal-notifier/terminal-notifier.app"
		if [ -d "$app" ] && have open; then
			open -n "$app" --args -title Buildgate -message "Allow notifications from terminal-notifier, so a click opens the request." || true
		fi
		echo "macOS asks once to allow its notifications: choose Allow, or turn it on under System Settings -> Notifications -> terminal-notifier. 'factoryd doctor -notify-test' sends one banner to check."
	else
		echo "terminal-notifier not installed -- desktop notifications will have no click ('brew install terminal-notifier' adds it)" >&2
	fi
fi

if [ "${FACTORYD_AUTOSTART:-}" != 0 ] && have docker && have colima && ! docker info >/dev/null 2>&1; then
	if [ -e "${COLIMA_HOME:-$HOME/.colima}/default/colima.yaml" ]; then
		echo "Starting Docker: colima start"
		colima start
	else
		echo "Starting Docker: colima start --memory 4"
		colima start --memory 4
	fi
fi
