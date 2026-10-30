"""Offline tests for scripts/install-prereqs.sh, `make install`'s first step.

Each test runs the script with PATH holding only a temp directory: the few
system utilities the script needs, the tools that test says are installed,
a stand-in `uname` naming the system (Darwin unless the test says Linux),
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
# looks; `docker info` only once the daemon marker exists; `docker compose
# version` unless the no-compose marker exists.
DOCKER = """#!/bin/sh
if [ "$1" = buildx ]; then [ -e "$HOME/.docker/cli-plugins/docker-buildx" ]; exit $?; fi
if [ "$1" = info ]; then [ -e "$HOME/docker-up" ]; exit $?; fi
if [ "$1" = compose ]; then [ ! -e "$HOME/no-compose" ]; exit $?; fi
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
        # A Mac that already has terminal-notifier, unless a test says
        # otherwise: the tests of the install path then see only its calls.
        self.system("Darwin")
        self.install("terminal-notifier")
        # Logs any call: nothing here may run sudo or start dockerd.
        for name in ("sudo", "dockerd", "systemctl"):
            self.install(name, f'#!/bin/sh\necho "{name} $*" >> "$HOME/privileged.log"\n')

    def system(self, name):
        self.install("uname", f"#!/bin/sh\necho {name}\n")

    def without_terminal_notifier(self):
        (self.bin / "terminal-notifier").unlink()

    def privileged_calls(self):
        log = self.home / "privileged.log"
        return log.read_text().splitlines() if log.exists() else []

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
        self.without_terminal_notifier()
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), ["install terminal-notifier", "--prefix"])
        # Nothing to launch here (no application bundle, no `open`): the
        # operator is not told of a prompt that will not come.
        self.assertNotIn("macOS asks once", result.stdout)
        self.assertIn("factoryd doctor -notify-test", result.stdout)

    def test_a_new_terminal_notifier_is_launched_as_an_application_once(self):
        self.have(*EVERYTHING)
        self.without_terminal_notifier()
        self.install("open", '#!/bin/sh\necho "$*" >> "$HOME/open.log"\n')
        app = self.prefix / "opt" / "terminal-notifier" / "terminal-notifier.app"
        app.mkdir(parents=True)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        opened = (self.home / "open.log").read_text().splitlines()
        self.assertEqual(len(opened), 1)
        self.assertTrue(opened[0].startswith(f"-n {app} --args -title Buildgate"), opened[0])
        self.assertIn("System Settings -> Notifications -> terminal-notifier", result.stdout)
        # Already installed: nothing is launched again.
        result = self.run_script()
        self.assertEqual(len((self.home / "open.log").read_text().splitlines()), 1)

    def test_a_mac_with_terminal_notifier_is_left_alone(self):
        self.have(*EVERYTHING, "terminal-notifier")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), [])

    def test_no_terminal_notifier_and_no_homebrew_is_not_a_failure(self):
        self.have(*EVERYTHING, brew=False)
        self.without_terminal_notifier()
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("desktop notifications will have no click", result.stderr)

    def test_another_system_is_not_given_terminal_notifier(self):
        self.have(*EVERYTHING)
        self.system("Linux")
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

    def test_ghcr_image_install_still_installs_buildx_for_toolchain_derivation(self):
        self.have("go", "python3", "gh", "git", "npm", "docker", buildx=False)
        result = self.run_script(FACTORYD_IMAGE_SOURCE="ghcr")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), ["--prefix", "install docker-buildx", "--prefix"])
        self.assertTrue((self.home / ".docker" / "cli-plugins" / "docker-buildx").is_symlink())

    def test_make_local_images_pulls_release_refs_and_records_digests(self):
        self.install(
            "docker",
            """#!/bin/sh
case "$1 $2" in
  "info ") exit 0 ;;
  "pull "*) echo "$*" >> "$HOME/docker.log"; exit 0 ;;
  "image inspect")
    case "$5" in
      *buildgate-worker*) echo ghcr.io/ljammula/buildgate-worker@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
      *factoryd-meter*) echo ghcr.io/ljammula/factoryd-meter@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
      *) echo ghcr.io/ljammula/buildgate-registry-proxy@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc ;;
    esac; exit 0 ;;
  "buildx version") exit 0 ;;
  "inspect factoryd-local-registry") echo "$*" >> "$HOME/docker.log"; exit 0 ;;
esac
exit 1
""",
        )
        self.install(
            "go",
            """#!/bin/sh
if [ "$1" = run ] && [ "$2" = ./cmd/factoryd ]; then
  if [ "$3" = build-ca-bundle ]; then exit 0; fi
  printf '%s\n' "$*" >> "$HOME/go.log"
fi
""",
        )
        for utility in ("grep", "tail"):
            os.symlink(shutil.which(utility), self.bin / utility)
        self.install("curl", "#!/bin/sh\nexit 0\n")
        makefile = Path(__file__).resolve().parents[2] / "Makefile"
        result = subprocess.run(
            [
                shutil.which("make"),
                "-f",
                str(makefile),
                "IMAGE_SOURCE=ghcr",
                "BUILDGATE_IMAGE_TAG=v1.2.3",
                "local-images",
            ],
            env={"PATH": str(self.bin), "HOME": str(self.home)},
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(
            (self.home / "docker.log").read_text().splitlines(),
            [
                "inspect factoryd-local-registry",
                "pull ghcr.io/ljammula/buildgate-worker:v1.2.3",
                "pull ghcr.io/ljammula/factoryd-meter:v1.2.3",
                "pull ghcr.io/ljammula/buildgate-registry-proxy:v1.2.3",
            ],
        )
        configured = (self.home / "go.log").read_text()
        self.assertIn("-sandbox-image ghcr.io/ljammula/buildgate-worker@sha256:" + "a" * 64, configured)
        self.assertIn("-meter-image ghcr.io/ljammula/factoryd-meter@sha256:" + "b" * 64, configured)
        self.assertIn("-registry-proxy-image ghcr.io/ljammula/buildgate-registry-proxy@sha256:" + "c" * 64, configured)

    def test_release_image_publish_only_builds_after_confirmed_tag_absence(self):
        workflow = Path(__file__).resolve().parents[2] / ".github/workflows/publish-images.yml"
        lines = workflow.read_text().splitlines()
        step_index = next(index for index, line in enumerate(lines) if "Build and publish" in line)
        run_index = next(index for index in range(step_index + 1, len(lines)) if lines[index].strip() == "run: |")
        indent = len(lines[run_index]) - len(lines[run_index].lstrip()) + 2
        script_lines = []
        for line in lines[run_index + 1 :]:
            if line.strip() and not line.startswith(" " * indent):
                break
            script_lines.append(line[indent:] if line.strip() else "")
        workflow_script = "\n".join(script_lines)
        # GitHub's Ubuntu runner uses Bash 5; the system Bash on macOS 3.2
        # cannot parse the lowercase expansion. The test owner is lowercase.
        workflow_script = workflow_script.replace("${GITHUB_REPOSITORY_OWNER,,}", "$GITHUB_REPOSITORY_OWNER")

        self.install("go", "#!/bin/sh\nprintf '%064d\\n' 0\n")
        log = self.home / "publish-docker.log"
        for raw_error, child_error, want_build, expected_message in (
            ("ERROR: registry: 503 Service Unavailable", "", False, "uncertain tag"),
            ("ERROR: ghcr.io/ljammula/buildgate-worker:v1.2.3: not found", "", True, ""),
            ("", "ERROR: child manifest config: not found", False, "existing manifest list"),
        ):
            with self.subTest(raw_error=raw_error, child_error=child_error):
                log.unlink(missing_ok=True)
                self.install(
                    "docker",
                    "#!/bin/sh\n"
                    'printf "%s\\n" "$*" >> "$DOCKER_LOG"\n'
                    'if [ "$1 $2 $3" = "buildx imagetools inspect" ]; then\n'
                    '  if [ "$4" = "--raw" ]; then\n'
                    '    if [ -n "$RAW_ERROR" ]; then echo "$RAW_ERROR" >&2; exit 1; fi\n'
                    '    echo \'{"schemaVersion":2}\'\n    exit 0\n  fi\n'
                    '  if [ -n "$CHILD_ERROR" ]; then echo "$CHILD_ERROR" >&2; exit 1; fi\n'
                    '  echo \'{"digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}\'\n'
                    '  exit 0\nfi\n'
                    'if [ "$1 $2" = "buildx build" ]; then exit 71; fi\n'
                    "exit 0\n",
                )
                env = {
                    **os.environ,
                    "PATH": f"{self.bin}:{os.environ['PATH']}",
                    "DOCKER_LOG": str(log),
                    "RAW_ERROR": raw_error,
                    "CHILD_ERROR": child_error,
                    "IMAGE_TAG": "v1.2.3",
                    "IMAGE_KIND": "worker",
                    "IMAGE_NAME": "buildgate-worker",
                    "DOCKERFILE": "internal/sandbox/Dockerfile",
                    "SOURCE_REVISION": "test-revision",
                    "GITHUB_REPOSITORY_OWNER": "ljammula",
                    "GITHUB_REPOSITORY": "ljammula/buildgate",
                    "GITHUB_STEP_SUMMARY": str(self.home / "summary.md"),
                }
                result = subprocess.run(
                    ["/bin/bash", "-c", workflow_script],
                    env=env,
                    capture_output=True,
                    text=True,
                )
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertTrue(log.exists(), f"docker was not called: {result.stderr}")
                calls = log.read_text().splitlines()
                self.assertEqual(any(line.startswith("buildx build ") for line in calls), want_build)
                if expected_message:
                    self.assertIn(expected_message, result.stderr)

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

    def test_linux_with_everything_installs_nothing_even_with_homebrew(self):
        self.have(*EVERYTHING)
        self.system("Linux")
        self.without_terminal_notifier()
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.brew_calls(), [])
        self.assertEqual(self.colima_calls(), [])
        self.assertEqual(self.privileged_calls(), [])
        self.assertEqual(result.stdout, "")

    def test_linux_names_each_missing_tool_and_fails_without_brew_or_sudo(self):
        self.have("python3", "docker")
        self.system("Linux")
        result = self.run_script()
        self.assertEqual(result.returncode, 1)
        for named in ("go: https://go.dev", "gh: https://github.com/cli/cli", "git: sudo apt-get install git", "npm (Node 20+", "colima (sandboxes run in its VM"):
            self.assertIn(named, result.stderr)
        self.assertNotIn("python3:", result.stderr)
        self.assertNotIn("Docker's apt repository", result.stderr)
        self.assertNotIn("brew.sh", result.stderr)
        self.assertEqual(self.brew_calls(), [])
        self.assertEqual(self.privileged_calls(), [])

    def test_linux_without_docker_names_dockers_apt_repository(self):
        self.have("go", "python3", "gh", "git", "npm", "colima")
        self.system("Linux")
        result = self.run_script()
        self.assertEqual(result.returncode, 1)
        self.assertIn("https://docs.docker.com/engine/install/ubuntu/", result.stderr)
        self.assertIn("sudo apt-get install docker-ce-cli docker-buildx-plugin docker-compose-plugin", result.stderr)
        self.assertNotIn("install docker.io", result.stderr)
        self.assertNotIn("snap install", result.stderr)
        self.assertEqual(self.brew_calls(), [])
        self.assertEqual(self.colima_calls(), [])
        self.assertEqual(self.privileged_calls(), [])

    def test_linux_without_compose_v2_fails_and_names_the_plugin(self):
        self.have(*EVERYTHING)
        self.system("Linux")
        (self.home / "no-compose").write_text("")
        result = self.run_script()
        self.assertEqual(result.returncode, 1)
        self.assertIn("docker compose (the Compose v2 plugin)", result.stderr)
        self.assertIn("sudo apt-get install docker-compose-plugin ", result.stderr)

    def test_linux_without_buildx_names_the_plugin_and_links_nothing(self):
        self.have(*EVERYTHING, buildx=False)
        self.system("Linux")
        plugin = self.prefix / "lib/docker/cli-plugins/docker-buildx"
        plugin.parent.mkdir(parents=True)
        plugin.write_text("#!/bin/sh\n")
        plugin.chmod(0o755)
        result = self.run_script()
        self.assertEqual(result.returncode, 1)
        self.assertIn("docker buildx: Docker's apt repository", result.stderr)
        self.assertIn("sudo apt-get install docker-buildx-plugin ", result.stderr)
        self.assertEqual(self.brew_calls(), [])
        self.assertFalse((self.home / ".docker" / "cli-plugins" / "docker-buildx").exists())

    def test_linux_missing_npm_alone_is_not_a_failure(self):
        self.have("go", "python3", "gh", "git", "docker", "colima")
        self.system("Linux")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("placeholder", result.stderr)
        self.assertEqual(self.brew_calls(), [])

    def test_linux_without_colima_names_it_and_fails(self):
        self.have("go", "python3", "gh", "git", "npm", "docker")
        self.system("Linux")
        result = self.run_script()
        self.assertEqual(result.returncode, 1)
        self.assertIn("colima (sandboxes run in its VM; it needs QEMU and /dev/kvm)", result.stderr)
        self.assertNotIn("systemctl", result.stderr)
        self.assertEqual(self.privileged_calls(), [])
        self.assertEqual(self.colima_calls(), [])

    def test_linux_docker_down_with_colima_starts_colima(self):
        self.have(*EVERYTHING, docker_up=False)
        self.system("Linux")
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.colima_calls(), ["start --memory 4"])
        self.assertNotIn("systemctl", result.stderr)
        self.assertEqual(self.privileged_calls(), [])

    def test_a_mac_with_docker_down_and_no_colima_gets_no_systemd_hint(self):
        self.have("go", "python3", "gh", "git", "npm", "docker", docker_up=False)
        result = self.run_script()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")


if __name__ == "__main__":
    unittest.main()
