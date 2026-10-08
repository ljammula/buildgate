#!/usr/bin/env python3
"""private-module-proxy.py <module source dir> <dest>: writes <dest> as a Go
module proxy (https://go.dev/ref/mod#goproxy-protocol) serving one version,
v1.0.0, of the module in <module source dir>.

It is how scripts/live-private-module.sh gives this machine, and only this
machine, a module no public proxy has: GOPROXY=file://<dest>,... on the host
stands in for an operator's own access to a company's private modules.
"""
import json
import pathlib
import sys
import zipfile

VERSION = "v1.0.0"


def main(source: str, dest: str) -> None:
    src = pathlib.Path(source)
    module = next(
        line.split()[1]
        for line in (src / "go.mod").read_text().splitlines()
        if line.startswith("module ")
    )
    out = pathlib.Path(dest) / module / "@v"
    out.mkdir(parents=True, exist_ok=True)
    (out / "list").write_text(VERSION + "\n")
    (out / (VERSION + ".info")).write_text(json.dumps({"Version": VERSION, "Time": "2026-01-01T00:00:00Z"}))
    (out / (VERSION + ".mod")).write_bytes((src / "go.mod").read_bytes())
    with zipfile.ZipFile(out / (VERSION + ".zip"), "w", zipfile.ZIP_DEFLATED) as archive:
        # Not a dotfile (a .DS_Store would change the module's hash).
        for path in sorted(p for p in src.rglob("*") if p.is_file() and not p.name.startswith(".")):
            # A fixed timestamp: the module's hash is over names and contents,
            # and the zip itself should not differ from run to run either.
            entry = zipfile.ZipInfo(f"{module}@{VERSION}/{path.relative_to(src).as_posix()}", (2026, 1, 1, 0, 0, 0))
            archive.writestr(entry, path.read_bytes())


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    main(sys.argv[1], sys.argv[2])
