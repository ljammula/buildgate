#!/bin/sh
# temporal-test-server.sh: runs one disposable `temporal server start-dev`
# that cannot outlive its owner.
#
# Usage: temporal-test-server.sh factoryd-test-temporal <port> [log-file]
#
# The owner (cmd/factoryd's tests, scripts/test-sharded.sh) holds the write
# end of this script's stdin. When the owner exits, even by SIGKILL, the pipe
# closes, `cat` returns and the server is killed. The literal first argument
# is the marker the owners' startup sweep greps for; the server carries it
# too (as an extra namespace), so a server whose wrapper was itself killed is
# swept as well (either orphan has PPID 1). Never touches TEMPORAL_ADDRESS or
# port 7233.
marker="$1"
port="$2"
log="${3:-/dev/null}"
temporal server start-dev --headless --ip 127.0.0.1 --port "$port" --ui-port 0 --namespace "$marker" >"$log" 2>&1 </dev/null &
server=$!
trap 'kill $server 2>/dev/null; exit 0' INT TERM
cat >/dev/null
kill "$server" 2>/dev/null
wait "$server" 2>/dev/null
exit 0
