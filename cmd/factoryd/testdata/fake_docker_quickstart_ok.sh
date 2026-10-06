#!/bin/sh
# Minimal fake `docker` CLI for TestQuickstartEnsureImagesSkipsWhenAlreadyPassing:
# answers exactly the subcommands quickstartPullDoctorInputs' own check set
# issues (doctorCheckDockerReachable, doctorCheckImagePresent x3,
# doctorCheckComposeVersion), always reporting success, so that test can
# exercise the "doctor already passing -> skip straight through" path
# without a real Docker daemon or real images. Unlike testdata/fake_docker.sh
# (which backs tests needing internal/sandbox's real container-launch
# machinery), this script does no bookkeeping at all -- every call it
# understands just exits 0.
set -eu

case "${1:-}" in
version)
	echo "fake-docker/0.0.0"
	exit 0
	;;
image)
	# `docker image inspect <ref>` -- sandbox.ImagePresent's own probe.
	exit 0
	;;
compose)
	if [ "${2:-}" = "version" ]; then
		echo "2.99.0"
		exit 0
	fi
	echo "fake_docker_quickstart_ok: unsupported compose subcommand ${2:-}" >&2
	exit 1
	;;
*)
	echo "fake_docker_quickstart_ok: unsupported subcommand ${1:-}" >&2
	exit 1
	;;
esac
