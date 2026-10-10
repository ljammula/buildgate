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
    colima) cat "$COLIMA_STUB" > "$BIN/colima"; chmod +x "$BIN/colima" ;;
    python) printf '#!/bin/sh\\n' > "$BIN/python3"; chmod +x "$BIN/python3" ;;
    node) printf '#!/bin/sh\\n' > "$BIN/npm"; chmod +x "$BIN/npm" ;;
    *) printf '#!/bin/sh\\n' > "$BIN/$formula"; chmod +x "$BIN/$formula" ;;
  esac
done
"""

# `docker buildx version` works only once the plugin is linked where Docker
# looks; `docker info` only once the daemon marker exists.
DOCKER = """#!/bin/sh
if [ "$1" = buildx ]; then [ -e "$HOME/.docker/cli-plugins/docker-buildx" ]; exit $?; fi
if [ "$1" = info ]; then [ -e "$HOME/docker-up" ]; exit $?; fi
exit 0
"""

# `colima start` records its arguments and brings the daemon up.
COLIMA = """#!/bin/sh
echo "$*" >> "$HOME/colima.log"
touch "$HOME/docker-up"
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
        self.colima_stub = self.tmp / "colima-stub"
        self.colima_stub.write_text(COLIMA)
        for name in UTILITIES:
            os.symlink(shutil.which(name), self.bin / name)

    def install(self, name, body="#!/bin/sh\n"):
        path = self.bin / name
        path.write_text(body)
        path.chmod(path.stat().st_mode | stat.S_IXUSR)

    def have(self, *tools, brew=True, buildx=True, docker_up=True):
        bodies = {"docker": DOCKER, "colima": COLIMA}
        for tool in tools:
            self.install(tool, bodies.get(tool, "#!/bin/sh\n"))
        if docker_up:
            (self.home / "docker-up").write_text("")
        if brew:
            self.install("brew", BREW)
        if buildx:
            plugins = self.home / ".docker" / "cli-plugins"
            plugins.mkdir(parents=True)
            (plugins / "docker-buildx").write_text("")

    def colima_calls(self):
        log = self.home / "colima.log"
        return log.read_text().splitlines() if log.exists() else []

    def run_script(self, **extra):
        env = {
            "PATH": str(self.bin),
            "HOME": str(self.home),
            "BIN": str(self.bin),
            "BREW_LOG": str(self.log),
            "BREW_PREFIX": str(self.prefix),
            "DOCKER_STUB": str(self.docker_stub),
            "COLIMA_STUB": str(self.colima_stub),
            **extra,
        }
        return subprocess.run([str(self.bin / "sh"), str(SCRIPT)], env=env, capture_output=True, text=True)

    def brew_calls(self):
        return self.log.read_text().splitlines() if self.log.exists() else []

    def test_installs_nothing_when_everything_is_present(self):
        self.have(*EVERYTHING)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), [])
        self.assertEqual(self.colima_calls(), [])
        self.assertEqual(result.stdout, "")

    def test_a_mac_without_terminal_notifier_gets_it(self):
        self.have(*EVERYTHING)
        self.install("uname", "#!/bin/sh\necho Darwin\n")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), ["install terminal-notifier"])
        self.assertIn("System Settings -> Notifications -> terminal-notifier", result.stdout)

    def test_a_mac_with_terminal_notifier_is_left_alone(self):
        self.have(*EVERYTHING, "terminal-notifier")
        self.install("uname", "#!/bin/sh\necho Darwin\n")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), [])

    def test_no_terminal_notifier_and_no_homebrew_is_not_a_failure(self):
        self.have(*EVERYTHING, brew=False)
        self.install("uname", "#!/bin/sh\necho Darwin\n")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("desktop notifications will have no click", result.stderr)

    def test_another_system_is_not_given_terminal_notifier(self):
        self.have(*EVERYTHING)
        self.install("uname", "#!/bin/sh\necho Linux\n")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), [])

    def test_installs_only_what_is_missing(self):
        self.have("python3", "git", "docker", "colima")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), ["install go gh node"])
        self.assertIn("brew install go gh node", result.stdout)

    def test_new_mac_gets_every_tool_the_buildx_link_and_a_started_docker(self):
        self.have(buildx=False, docker_up=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("install go python gh git node docker docker-buildx colima", self.brew_calls())
        link = self.home / ".docker" / "cli-plugins" / "docker-buildx"
        self.assertEqual(os.readlink(link), str(self.prefix / "lib/docker/cli-plugins/docker-buildx"))
        self.assertEqual(self.colima_calls(), ["start --memory 4"])

    def test_existing_docker_without_buildx_gets_the_plugin_and_no_colima(self):
        self.have("go", "python3", "gh", "git", "npm", "docker", buildx=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("install docker-buildx", self.brew_calls())
        self.assertTrue((self.home / ".docker" / "cli-plugins" / "docker-buildx").is_symlink())
        self.assertNotIn("colima", result.stdout)

    def test_an_installed_buildx_formula_is_linked_without_brew_install(self):
        self.have("go", "python3", "gh", "git", "npm", "docker", buildx=False)
        plugin = self.prefix / "lib/docker/cli-plugins/docker-buildx"
        plugin.parent.mkdir(parents=True)
        plugin.write_text("#!/bin/sh\n")
        plugin.chmod(0o755)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), ["--prefix"])
        self.assertEqual(os.readlink(self.home / ".docker" / "cli-plugins" / "docker-buildx"), str(plugin))

    def test_a_stopped_colima_vm_is_started_with_its_own_settings(self):
        self.have(*EVERYTHING, docker_up=False)
        vm = self.home / ".colima" / "default"
        vm.mkdir(parents=True)
        (vm / "colima.yaml").write_text("memory: 8\n")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.colima_calls(), ["start"])

    def test_docker_down_without_colima_is_left_to_the_operator(self):
        self.have("go", "python3", "gh", "git", "npm", "docker", docker_up=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.colima_calls(), [])

    def test_autostart_off_starts_nothing(self):
        self.have(*EVERYTHING, docker_up=False)
        result = self.run_script(FACTORYD_AUTOSTART="0")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.colima_calls(), [])

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
