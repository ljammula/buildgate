#!/usr/bin/env python3
"""Fixture stand-in for build_app.py, used only by the end-to-end worker
test: appends a line to content.txt and commits it.
"""
import argparse
import os
import subprocess


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workspace", required=True)
    parser.add_argument("--spec", required=True)
    parser.add_argument("--verify-command", required=True)
    args, _ = parser.parse_known_args()

    with open(os.path.join(args.workspace, "content.txt"), "a") as f:
        f.write("edited by the worker end-to-end fixture\n")
    subprocess.run(["git", "-C", args.workspace, "add", "-A"], check=True)
    subprocess.run(["git", "-C", args.workspace, "commit", "-q", "-m", "fixture commit"], check=True)


if __name__ == "__main__":
    main()
