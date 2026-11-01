"""test-json.py: reads the `go test -json` event streams scripts/test-sharded.sh
records, one file per job.

  test-json.py text <file>      the job's whole output, as `go test -v` prints it
  test-json.py failures <file>  only what a failed job needs read: the output of
                                every test that did not pass or skip, and every
                                line that belongs to no test (package results,
                                build errors, a panic, a timeout)
  test-json.py passed <file>... the top-level tests that passed in some package
                                and skipped or failed in none, one name a line
                                (a name two packages share counts only if
                                neither skipped it)

A line that is not a JSON event (the go command's own stderr) is output that
belongs to no test.
"""

import json
import sys

DONE = ("pass", "fail", "skip")


def events(path):
	with open(path, encoding="utf-8", errors="replace") as f:
		for line in f:
			try:
				event = json.loads(line)
			except ValueError:
				event = None
			if not isinstance(event, dict) or "Action" not in event:
				yield {"Action": "output", "Output": line}
				continue
			yield event


def outcomes(path):
	"""Maps (package, top-level test) to the test's last pass/fail/skip."""
	result = {}
	for event in events(path):
		test = event.get("Test")
		if event["Action"] in DONE and test and "/" not in test:
			result[(event.get("Package"), test)] = event["Action"]
	return result


def output(path, keep):
	for event in events(path):
		if event["Action"] in ("output", "build-output") and keep(event):
			sys.stdout.write(event.get("Output", ""))


def main(argv):
	if len(argv) < 2 or argv[0] not in ("text", "failures", "passed"):
		sys.exit(__doc__)
	verb, paths = argv[0], argv[1:]
	if verb == "text":
		output(paths[0], lambda event: True)
	elif verb == "failures":
		ended = outcomes(paths[0])

		def keep(event):
			test = event.get("Test")
			if not test:
				return True
			return ended.get((event.get("Package"), test.split("/")[0])) not in ("pass", "skip")

		output(paths[0], keep)
	else:
		passed, other = set(), set()
		for path in paths:
			for (_, test), action in outcomes(path).items():
				(passed if action == "pass" else other).add(test)
		for name in sorted(passed - other):
			print(name)


if __name__ == "__main__":
	main(sys.argv[1:])
