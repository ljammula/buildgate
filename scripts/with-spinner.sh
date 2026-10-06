#!/usr/bin/env bash
# with-spinner.sh "<words>" <cmd> [args...]
#
# Runs <cmd> so a minutes-long build does not look frozen.
#
#   stderr is a terminal: the command's output (stdout and stderr) goes to a
#     temp log, one line animates a braille frame, <words> and the elapsed
#     time, then it is replaced by a check mark or a cross. A failing command
#     also gets the last 40 lines of its log printed.
#   otherwise (CI, a pipe, a log file): the command runs exactly as if this
#     script were not there, output untouched.
#
# The exit status is always the command's. The terminal is judged on stderr,
# not stdout, because callers capture stdout (`$(make sandbox-image | grep ...)`)
# while the operator still watches stderr. On a terminal the command's stdout
# is discarded with the log on success.
set -u

if [ $# -lt 2 ]; then
	echo "usage: with-spinner.sh \"<words>\" <cmd> [args...]" >&2
	exit 2
fi
words=$1
shift

if [ ! -t 2 ]; then
	exec "$@"
fi

log="$(mktemp "${TMPDIR:-/tmp}/with-spinner.XXXXXX")"
pid=""
cleanup() { rm -f "$log"; }
interrupted() {
	[ -n "$pid" ] && kill "$pid" 2>/dev/null
	printf '\r\033[2K' >&2
	cleanup
	exit 130
}
trap interrupted INT TERM
trap cleanup EXIT

elapsed_text() {
	local s=$1
	if [ "$s" -lt 60 ]; then
		printf '%ds' "$s"
	else
		printf '%dm%02ds' $((s / 60)) $((s % 60))
	fi
}

"$@" >"$log" 2>&1 </dev/null &
pid=$!
frames=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)
start=$SECONDS
i=0
while kill -0 "$pid" 2>/dev/null; do
	printf '\r\033[2K%s %s · %s' "${frames[i % ${#frames[@]}]}" "$words" "$(elapsed_text $((SECONDS - start)))" >&2
	i=$((i + 1))
	sleep 0.1
done
wait "$pid"
status=$?
pid=""
took="$(elapsed_text $((SECONDS - start)))"
printf '\r\033[2K' >&2
if [ "$status" -eq 0 ]; then
	printf '✓ %s · %s\n' "$words" "$took" >&2
else
	printf '✗ %s · %s (exit %d); last 40 lines of output:\n' "$words" "$took" "$status" >&2
	tail -n 40 "$log" >&2
fi
exit "$status"
