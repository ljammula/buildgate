#!/bin/sh
# install-prereqs.sh: `make install`'s first step. On macOS it installs, with
# Homebrew, the tools the rest of the install needs and this machine lacks,
# links Docker's buildx plugin for image builds and automatic toolchain
# derivation, and starts Docker, so a new Mac needs no `brew install` or
# `colima start` line first. On Linux it installs nothing and runs no sudo,
# even with Homebrew on PATH: it names each missing tool and where to get it.
#
#   Tool                  macOS: formula     Linux: named source        Missing when
#   go                    go                 go.dev                     the command is not on PATH
#   python3, git          python git         apt-get                    the command is not on PATH
#   gh                    gh                 GitHub CLI's Linux page    the command is not on PATH
#   npm (the console)     node               nodejs.org                 the command is not on PATH
#   docker (the CLI)      docker             Docker's apt repository    the command is not on PATH
#   docker buildx         docker-buildx      Docker's apt repository    `docker buildx version` fails
#   docker compose        -                  Docker's apt repository    Linux, and `docker compose version` fails
#   a Docker daemon       colima             colima (QEMU, /dev/kvm)    macOS: neither docker nor colima is on PATH;
#                                                                       Linux: colima is not on PATH
#   terminal-notifier     terminal-notifier  -                          macOS, and the command is not on PATH
#
# On Linux the Docker daemon runs in colima's VM, as on a Mac: sandboxes do
# not run on Docker Engine on the host (factoryd refuses that backend), so
# colima is required there too. The docker CLI and its plugins come from
# Docker's own apt repository (https://docs.docker.com/engine/install/ubuntu/):
# docker-ce-cli, docker-buildx-plugin and docker-compose-plugin. The
# distribution's docker.io package and the snap are not named: they can lack
# the buildx plugin or Compose v2, which the image builds and compose
# services need.
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
# PATH, on either OS: `colima start` for a VM that already exists (its own
# settings), else `colima start --memory 4`. FACTORYD_AUTOSTART=0 turns the
# start off, as it does for factoryd's own. Any other Docker is the
# operator's to start; this never starts dockerd. `make install` stops at the
# image build and says so.
#
# Without Homebrew on macOS, and always on Linux, it installs nothing: it
# names what is missing and fails, unless only npm is, which costs the console
# alone (`make install` then serves the placeholder page).
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

# Linux: names each missing tool, one line each with where to get it, and
# fails, unless only npm is missing. Installs nothing.
linux_lines=""
need() { linux_lines="$linux_lines  $1
"; }
linux_prereqs() {
	have go || need "go: https://go.dev/doc/install"
	have python3 || need "python3: sudo apt-get install python3"
	have gh || need "gh: https://github.com/cli/cli/blob/trunk/docs/install_linux.md"
	have git || need "git: sudo apt-get install git"
	docker_packages=""
	if ! have docker; then
		docker_packages="docker-ce-cli docker-buildx-plugin docker-compose-plugin"
		need "docker, docker buildx and docker compose: Docker's apt repository"
	else
		if ! docker buildx version >/dev/null 2>&1; then
			docker_packages="docker-buildx-plugin"
			need "docker buildx: Docker's apt repository"
		fi
		if ! docker compose version >/dev/null 2>&1; then
			docker_packages="${docker_packages:+$docker_packages }docker-compose-plugin"
			need "docker compose (the Compose v2 plugin): Docker's apt repository"
		fi
	fi
	have colima || need "colima (sandboxes run in its VM; it needs QEMU and /dev/kvm): https://github.com/abiosoft/colima#installation"
	if [ -z "$linux_lines" ]; then
		have npm || echo "npm not installed -- factoryd will serve the console placeholder page (Node 20+ from https://nodejs.org builds the console)" >&2
		return 0
	fi
	have npm || need "npm (Node 20+, for the console): https://nodejs.org"
	echo "make install needs these, and installs nothing itself on Linux:" >&2
	printf '%s' "$linux_lines" >&2
	if [ -n "$docker_packages" ]; then
		echo "Docker's apt repository: add it as https://docs.docker.com/engine/install/ubuntu/ says, then: sudo apt-get install $docker_packages (not the distribution's docker.io or the snap)" >&2
	fi
	echo "Install them, then re-run 'make install'." >&2
	exit 1
}

os="$(uname -s 2>/dev/null || true)"

if [ "$os" = Linux ]; then
	linux_prereqs
else
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
fi

if [ "$os" = Darwin ] && ! have terminal-notifier; then
	if have brew && brew install terminal-notifier; then
		echo "Installed terminal-notifier: a notification's click opens the request it is about."
		# macOS lists an application under Notifications, and asks to allow
		# it, only once it has been launched as an application: running the
		# command from a shell is refused without a word to the operator.
		app="$(brew --prefix 2>/dev/null || true)/opt/terminal-notifier/terminal-notifier.app"
		if [ -d "$app" ] && have open && open -n "$app" --args -title Buildgate -message "Allow notifications from terminal-notifier, so a click opens the request."; then
			echo "macOS asks once to allow its notifications: choose Allow, or turn it on under System Settings -> Notifications -> terminal-notifier. 'factoryd doctor -notify-test' sends one banner to check."
		else
			echo "macOS shows nothing from it until it is allowed: 'factoryd doctor -notify-test' says how."
		fi
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
