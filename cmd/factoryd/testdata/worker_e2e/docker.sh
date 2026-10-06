#!/bin/sh
# ../fake_docker.sh plus the docker calls a relay launch makes, for the
# end-to-end worker test: the request driver's drafting jobs always launch a
# relay. Each answer is what the real CLI prints when the call succeeds:
#
#   network create        the network id
#   network <any other>   nothing (connect, rm; ls: the network is gone)
#   run --detach          the container id
#   inspect               true (the only inspect is the relay's {{.State.Running}})
#   logs                  the relay's listening line, no usage line
#   ps --filter name=     nothing (the relay container is gone)
#
# Everything else goes to ../fake_docker.sh unchanged.
set -eu

case "${1:-}" in
network)
	if [ "${2:-}" = create ]; then
		echo "fake-relay-network"
	fi
	exit 0
	;;
inspect)
	echo "true"
	exit 0
	;;
logs)
	echo "serving inference relay on fake"
	exit 0
	;;
run)
	for a in "$@"; do
		if [ "$a" = "--detach" ]; then
			echo "fake-relay-container"
			exit 0
		fi
	done
	;;
ps)
	for a in "$@"; do
		case "$a" in
		name=*) exit 0 ;;
		esac
	done
	;;
esac

exec "$(dirname "$0")/../fake_docker.sh" "$@"
