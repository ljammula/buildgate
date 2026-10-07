"""Offline tests for scripts/install-prereqs.sh, `make install`'s first step.

Each test runs the script with PATH holding only a temp directory: the few
system utilities the script needs, the tools that test says are installed,
and a stand-in `brew` that records its arguments and "installs" a formula by
dropping its command into that directory. Nothing is installed for real.
"""

import os
import shutil
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "install-prereqs.sh"
UTILITIES = ["sh", "mkdir", "ln", "cat", "chmod", "touch"]
COMMAND_OF = {"python": "python3", "node": "npm"}
EVERYTHING = ["go", "python3", "gh", "git", "npm", "docker", "colima"]

BREW = """#!/bin/sh
echo "$*" >> "$BREW_LOG"
if [ "$1" = --prefix ]; then echo "$BREW_PREFIX"; exit 0; fi
[ "$1" = install ] || exit 1
shift
for formula in "$@"; do
  case "$formula" in
    docker-buildx)
      mkdir -p "$BREW_PREFIX/lib/docker/cli-plugins"
      printf '#!/bin/sh\\n' > "$BREW_PREFIX/lib/docker/cli-plugins/docker-buildx"
      chmod +x "$BREW_PREFIX/lib/docker/cli-plugins/docker-buildx" ;;
    docker) cat "$DOCKER_STUB" > "$BIN/docker"; chmod +x "$BIN/docker" ;;
    python) printf '#!/bin/sh\\n' > "$BIN/python3"; chmod +x "$BIN/python3" ;;
    node) printf '#!/bin/sh\\n' > "$BIN/npm"; chmod +x "$BIN/npm" ;;
    *) printf '#!/bin/sh\\n' > "$BIN/$formula"; chmod +x "$BIN/$formula" ;;
  esac
done
"""

# `docker buildx version` works only once the plugin is linked where Docker looks.
DOCKER = """#!/bin/sh
if [ "$1" = buildx ]; then [ -e "$HOME/.docker/cli-plugins/docker-buildx" ]; exit $?; fi
exit 0
"""


class InstallPrereqsTest(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp)
        self.bin = self.tmp / "bin"
        self.home = self.tmp / "home"
        self.prefix = self.tmp / "brew"
        for d in (self.bin, self.home, self.prefix):
            d.mkdir()
        self.log = self.tmp / "brew.log"
        self.docker_stub = self.tmp / "docker-stub"
        self.docker_stub.write_text(DOCKER)
        for name in UTILITIES:
            os.symlink(shutil.which(name), self.bin / name)

    def install(self, name, body="#!/bin/sh\n"):
        path = self.bin / name
        path.write_text(body)
        path.chmod(path.stat().st_mode | stat.S_IXUSR)

    def have(self, *tools, brew=True, buildx=True):
        for tool in tools:
            self.install(tool, DOCKER if tool == "docker" else "#!/bin/sh\n")
        if brew:
            self.install("brew", BREW)
        if buildx:
            plugins = self.home / ".docker" / "cli-plugins"
            plugins.mkdir(parents=True)
            (plugins / "docker-buildx").write_text("")

    def run_script(self):
        env = {
            "PATH": str(self.bin),
            "HOME": str(self.home),
            "BIN": str(self.bin),
            "BREW_LOG": str(self.log),
            "BREW_PREFIX": str(self.prefix),
            "DOCKER_STUB": str(self.docker_stub),
        }
        return subprocess.run([str(self.bin / "sh"), str(SCRIPT)], env=env, capture_output=True, text=True)

    def brew_calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def test_installs_nothing_when_everything_is_present(self):
        self.have(*EVERYTHING)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), [])
        self.assertEqual(result.stdout, "")

    def test_installs_only_what_is_missing(self):
        self.have("python3", "git", "docker", "colima")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), ["install go gh node"])
        self.assertIn("brew install go gh node", result.stdout)

    def test_new_mac_gets_every_tool_and_the_buildx_link(self):
        self.have(buildx=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls()[0], "install go python gh git node docker docker-buildx colima")
        link = self.home / ".docker" / "cli-plugins" / "docker-buildx"
        self.assertEqual(os.readlink(link), str(self.prefix / "lib/docker/cli-plugins/docker-buildx"))
        self.assertIn("colima start --memory 4", result.stdout)

    def test_existing_docker_without_buildx_gets_the_plugin_and_no_colima(self):
        self.have("go", "python3", "gh", "git", "npm", "docker", buildx=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls()[0], "install docker-buildx")
        self.assertTrue((self.home / ".docker" / "cli-plugins" / "docker-buildx").is_symlink())
        self.assertNotIn("colima", result.stdout)

    def test_without_homebrew_names_what_is_missing_and_fails(self):
        self.have("python3", "git", "npm", "docker", brew=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 1)
        self.assertIn("go gh", result.stderr)
        self.assertIn("brew.sh", result.stderr)

    def test_without_homebrew_a_missing_npm_alone_is_not_a_failure(self):
        self.have("go", "python3", "gh", "git", "docker", brew=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("placeholder", result.stderr)

    def test_a_failed_brew_install_fails_the_step(self):
        self.have("python3", "git", "npm", "docker", "colima", brew=False)
        self.install("brew", "#!/bin/sh\nexit 1\n")
        self.assertNotEqual(self.run_script().returncode, 0)


if __name__ == "__main__":
    unittest.main()
