#!/bin/sh
# Fixture stand-in for the `docker` CLI, used only by tests that need
# internal/sandbox's real launch code (LaunchSpec.DockerCommand/Run) to
# proceed without a real Docker daemon. Sandboxing is now unconditional
# (factoryd has no host-execution opt-out at all), so any test that used
# to avoid Docker by passing -allow-unsandboxed now needs this instead:
# point -sandbox-docker at this script (with SANDBOX_SKIP_MOUNT_VISIBILITY_
# CHECK=1 set, so internal/sandbox never shells out to the real docker for
# that separate probe) and pass any digest-pinned -sandbox-image string
# (its own bytes are never used -- this script does no real containment).
#
# `run` is the only subcommand real work depends on: it translates the
# container-side paths in the trailing command's own argv back to the host
# paths they were bind-mounted from (read off the --volume host:container:
# mode flags docker.go's own DockerCommand always passes), cds to the
# host equivalent of --workdir, and execs the (translated) command
# directly on the host -- inheriting this process's own environment
# unchanged, exactly as a real container's environment here would (see
# internal/sandbox.dockerClientEnv's own doc comment: docker itself is
# always invoked with this host process's full environment already,
# which is why this script does not need to interpret --env at all).
#
# `ps`/`rm` back the reconciliation liveness checks (sandbox.
# WorkerContainerPresentForRun, called from cmd/factoryd's own
# liveSandboxContainerName) with a real, if minimal, container registry:
# `run` below records the launched "container" (name, labels, and this
# script's own PID -- preserved by exec into the real worker process, so
# it stays valid for exactly as long as a real container would run) into
# containerDir; `ps -a --filter label=K=V [--filter label=K=V ...]
# --format {{.Names}}` (the only query shape either caller issues) lists
# every recorded name whose labels satisfy every given filter and whose
# PID is still alive, matching --rm's own auto-removal semantics for a
# dead one; `rm -f <name>` removes the record outright. `exec`/`version`
# remain the harmless no-ops every other best-effort call site already
# treats a real miss as.
set -eu

# FAKE_DOCKER_CONTAINER_DIR: cmd/factoryd's TestMain gives each test
# process its own registry, so test processes running at once (make test's
# shards) never prune or half-read each other's records.
containerDir="${FAKE_DOCKER_CONTAINER_DIR:-${TMPDIR:-/tmp}/factoryd-fake-docker-containers}"
mkdir -p "$containerDir"

subcommand="${1:-}"
shift || true

case "$subcommand" in
run) ;;
exec)
	exit 0
	;;
rm)
	for a in "$@"; do
		case "$a" in
		-*) ;;
		*) rm -f "$containerDir/$a" ;;
		esac
	done
	exit 0
	;;
ps)
	filtercount=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--filter)
			case "$2" in
			label=*)
				filtercount=$((filtercount + 1))
				eval "ps_filter_${filtercount}=\${2#label=}"
				;;
			esac
			shift 2
			;;
		*) shift ;;
		esac
	done
	for entry in "$containerDir"/*; do
		[ -e "$entry" ] || continue
		pid=$(head -n 1 "$entry")
		if ! kill -0 "$pid" 2>/dev/null; then
			rm -f "$entry"
			continue
		fi
		matched=1
		fi_i=1
		while [ "$fi_i" -le "$filtercount" ]; do
			eval "want=\$ps_filter_${fi_i}"
			if ! grep -qxF "$want" "$entry"; then
				matched=0
			fi
			fi_i=$((fi_i + 1))
		done
		if [ "$matched" -eq 1 ]; then
			basename "$entry"
		fi
	done
	exit 0
	;;
version)
	echo "fake-docker/0.0.0"
	exit 0
	;;
*)
	echo "fake_docker: unsupported subcommand $subcommand" >&2
	exit 1
	;;
esac

image=""
workdir_c=""
volcount=0
name=""
labelcount=0
entrypoint=""

while [ $# -gt 0 ]; do
	arg="$1"
	case "$arg" in
	--rm | --read-only)
		shift
		;;
	--cap-drop=* | --security-opt=* | --cgroupns=*)
		shift
		;;
	--name)
		name="$2"
		shift 2
		;;
	--label)
		labelcount=$((labelcount + 1))
		eval "label_${labelcount}=\$2"
		shift 2
		;;
	--user | --ulimit | --pids-limit | --memory | --memory-swap | --cpus | --tmpfs | --network | --env)
		shift 2
		;;
	--workdir)
		workdir_c="$2"
		shift 2
		;;
	--entrypoint)
		entrypoint="$2"
		shift 2
		;;
	--volume)
		spec="$2"
		host="${spec%%:*}"
		rest="${spec#*:}"
		container="${rest%%:*}"
		volcount=$((volcount + 1))
		eval "vol_host_${volcount}=\$host"
		eval "vol_container_${volcount}=\$container"
		shift 2
		;;
	--*)
		# An unrecognized flag -- assume it takes no separate value rather
		# than risk misconsuming the image name as one.
		shift
		;;
	*)
		image="$arg"
		shift
		break
		;;
	esac
done

if [ -n "$name" ]; then
	entry="$containerDir/$name"
	: >"$entry"
	echo "$$" >>"$entry"
	label_i=1
	while [ "$label_i" -le "$labelcount" ]; do
		eval "echo \"\$label_${label_i}\"" >>"$entry"
		label_i=$((label_i + 1))
	done
fi

if [ -z "$image" ]; then
	echo "fake_docker: run: no image found in argv" >&2
	exit 1
fi

# translate rewrites one argument, replacing the longest matching
# container-mount prefix with its host equivalent, into the global
# $translated -- not printed for a caller to capture via $(...), which
# would silently strip any trailing newlines from a multi-line argument
# (e.g. a lockfile's own JSON content passed as one build_app.py argument).
# Uses $tr_i as its own loop counter, distinct from any loop counter a
# caller might be mid-iteration on, since this runs in the same shell
# rather than a subshell.
translate() {
	arg="$1"
	best_len=0
	best_host=""
	best_container=""
	tr_i=1
	while [ "$tr_i" -le "$volcount" ]; do
		eval "c=\$vol_container_${tr_i}"
		eval "h=\$vol_host_${tr_i}"
		case "$arg" in
		"$c" | "$c"/*)
			clen=${#c}
			if [ "$clen" -gt "$best_len" ]; then
				best_len=$clen
				best_host="$h"
				best_container="$c"
			fi
			;;
		esac
		tr_i=$((tr_i + 1))
	done
	if [ "$best_len" -gt 0 ]; then
		suffix="${arg#$best_container}"
		translated="$best_host$suffix"
	else
		translated="$arg"
	fi
}

hostwd=""
vol_i=1
while [ "$vol_i" -le "$volcount" ]; do
	eval "c=\$vol_container_${vol_i}"
	eval "h=\$vol_host_${vol_i}"
	if [ "$c" = "$workdir_c" ]; then
		hostwd="$h"
	fi
	vol_i=$((vol_i + 1))
done

# Rebuild the trailing command's own argv with translated paths. Positional
# parameters, not an array (POSIX sh has none), and never round-tripped
# through a newline-delimited file or command substitution: a build_app.py
# argument can itself be an embedded multi-line value (e.g. a lockfile's
# own JSON content), which either would silently corrupt. Captured into
# indexed variables instead (the same technique already used for --volume
# above), which preserves each argument's own bytes exactly, embedded
# newlines included.
argcount=0
for a in "$@"; do
	argcount=$((argcount + 1))
	eval "orig_arg_${argcount}=\$a"
done
set --
arg_i=1
while [ "$arg_i" -le "$argcount" ]; do
	eval "a=\$orig_arg_${arg_i}"
	translate "$a"
	set -- "$@" "$translated"
	arg_i=$((arg_i + 1))
done

if [ -n "$hostwd" ]; then
	cd "$hostwd"
fi
# --entrypoint (real docker: the trailing image-command argv becomes
# arguments TO the entrypoint, not a command in its own right --
# doctorCheckMountVisibilityFor's own "--entrypoint /bin/sh, image, -c,
# test -f \"$1\", <name>, <path>" invocation only makes sense executed as
# "/bin/sh -c ... <path>", not as "-c" run standalone). The translated
# <path> reaches sh as its own argv element above, so the probe sees the
# host-side marker the same way a real bind mount would.
if [ -n "$entrypoint" ]; then
	exec "$entrypoint" "$@"
fi
exec "$@"
