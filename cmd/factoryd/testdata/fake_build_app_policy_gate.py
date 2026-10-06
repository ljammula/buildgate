#!/usr/bin/env python3
"""Fixture stand-in for build_app.py, used only by
TestQueueRunEntry*ConformityPolicy* (see queue_run_test.go) to prove
-conformity-policy actually reaches and affects a queue-run-drained
entry's outcome, not just its argv (testdata/fake_build_app.sh's own
"commit"/"fail" modes are static and can't simulate a policy-dependent
exit).

queue-run never forwards -build-app-interpreter (a separate, pre-existing
gap out of scope for issue #164 -- see queueRunConfig's own doc comment),
so a queue-run-drained entry always launches its build script with
run_ticket.go's own "python3" default; this fixture is therefore Python,
unlike fake_build_app.sh's /bin/sh (only ever pointed to explicitly via
-build-app-interpreter by the direct-run integration suite).

Simulates the real build_app.py's own fail-closed conformity review:
"required" simulates an unreachable/unparseable reviewer response and
exits 1 (blocked, matching parse_conformity_verdicts' own documented
fail-closed default); "advisory" simulates the advisory escape valve and
commits normally, exiting 0.
"""
import argparse
import os
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--spec", required=True)
    parser.add_argument("--conformity-policy", required=True)
    parser.add_argument("--verify-command", required=True)
    args, _ = parser.parse_known_args()

    if args.conformity_policy == "required":
        sys.exit(1)

    workspace = args.workspace
    with open(os.path.join(workspace, "content.txt"), "a") as f:
        f.write("edited by fake_build_app_policy_gate.py\n")
    subprocess.run(["git", "-C", workspace, "add", "-A"], check=True)
    subprocess.run(
        ["git", "-C", workspace, "commit", "-q", "-m", "policy gate commit"],
        check=True,
    )
    sys.exit(0)


if __name__ == "__main__":
    main()
