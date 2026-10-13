#!/bin/sh
# Installs the latest stable Buildgate release from its tagged source, using
# that release's published GHCR images.
set -eu

repo=https://github.com/ljammula/buildgate.git

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 1
}

for command_name in make; do
	command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required; install it, then rerun this installer"
done

if ! command -v git >/dev/null 2>&1; then
	if command -v brew >/dev/null 2>&1; then
		brew install git || fail "could not install git with Homebrew"
	else
		fail "git is required to fetch the release; install git or Homebrew, then rerun this installer"
	fi
fi

release_tags=$(git ls-remote --tags --refs --sort=-version:refname "$repo" 'refs/tags/v*' |
	awk '$2 ~ /^refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+$/ { sub(/^refs\/tags\//, "", $2); print $2 }') ||
	fail "could not list Buildgate release tags"
[ -n "$release_tags" ] || fail "no stable vX.Y.Z release tag is available"

# A tag push starts image publication asynchronously. Select the newest
# release only after all three public image manifests are available, so an
# install during publication can use the previous complete release.
image_available() {
	package=$1
	tag=$2
	token_response=$(curl -fsSL "https://ghcr.io/token?scope=repository:$package:pull") || return 2
	token=$(printf '%s\n' "$token_response" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
	[ -n "$token" ] || return 2
	status=$(curl -sS -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $token" \
		-H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json' \
		"https://ghcr.io/v2/$package/manifests/$tag") || return 2
	case "$status" in
		200) return 0 ;;
		404) return 1 ;;
		*) return 2 ;;
	esac
}

release_tag=
for candidate in $release_tags; do
	images_complete=1
	for package in ljammula/buildgate-worker ljammula/factoryd-meter ljammula/buildgate-registry-proxy; do
		if image_available "$package" "$candidate"; then
			:
		else
			probe_status=$?
			case "$probe_status" in
				1) images_complete=0; break ;;
				*) fail "could not verify the GHCR image $package:$candidate" ;;
			esac
		fi
	done
	if [ "$images_complete" -eq 1 ]; then
		release_tag=$candidate
		break
	fi
done
[ -n "$release_tag" ] || fail "no stable release has all three published GHCR images"

if [ -z "${HOME:-}" ]; then
	fail 'HOME must be set'
fi
data_home=${XDG_DATA_HOME:-"$HOME/.local/share"}
source_dir=$data_home/buildgate/source
mkdir -p "$(dirname "$source_dir")" || fail "could not create the Buildgate install directory"
source_parent=$(CDPATH= cd -P "$(dirname "$source_dir")" && pwd -P) ||
	fail "could not resolve the Buildgate install directory"
source_dir=$source_parent/source

if [ -e "$source_dir" ]; then
	repo_root=$(git -C "$source_dir" rev-parse --show-toplevel 2>/dev/null || true)
	if [ "$repo_root" != "$source_dir" ]; then
		fail "$source_dir exists and is not the installer-managed Buildgate checkout"
	fi
	remote=$(git -C "$source_dir" remote get-url origin 2>/dev/null || true)
	case "$remote" in
		"$repo"|https://github.com/ljammula/buildgate|git@github.com:ljammula/buildgate.git) ;;
		*) fail "$source_dir does not have the official Buildgate repository as origin" ;;
	esac
else
	mkdir -p "$source_dir" || fail "could not create $source_dir"
	git -C "$source_dir" init --quiet || fail "could not initialize $source_dir"
	git -C "$source_dir" remote add origin "$repo" || fail "could not configure the Buildgate repository"
fi

if [ -n "$(git -C "$source_dir" status --porcelain)" ]; then
	fail "$source_dir has local changes; commit or stash them before installing"
fi
if ! git -C "$source_dir" fetch --depth 1 origin "refs/tags/$release_tag:refs/tags/$release_tag"; then
	fail "could not fetch release $release_tag"
fi
if ! git -C "$source_dir" checkout --detach "$release_tag"; then
	fail "could not check out release $release_tag"
fi

# Older release Makefiles leave an ignored console bundle in place when npm
# is unavailable, so remove any bundle from a previous checkout before build.
rm -rf "$source_dir/internal/consoleweb/dist" || fail "could not clear the previous console build"
mkdir -p "$source_dir/internal/consoleweb/dist" || fail "could not prepare the console build directory"
touch "$source_dir/internal/consoleweb/dist/.gitkeep" || fail "could not prepare the console placeholder"

printf 'Installing Buildgate %s from %s\n' "$release_tag" "$source_dir"
IMAGE_SOURCE=ghcr BUILDGATE_IMAGE_TAG="$release_tag" make -C "$source_dir" install
