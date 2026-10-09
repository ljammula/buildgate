import contextlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock


SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"


def _load(name):
	spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
	assert spec and spec.loader
	module = importlib.util.module_from_spec(spec)
	sys.modules[spec.name] = module
	spec.loader.exec_module(module)
	return module


build_app = _load("build_app")
harness_adapters = sys.modules["harness_adapters"]

PI = harness_adapters.get("pi")
PIFORK = harness_adapters.get("pifork")


def clean_env(**extra):
	"""os.environ without the route pins, plus extra."""
	env = {k: v for k, v in os.environ.items() if k not in ("PI_HARNESS_PROVIDER", "PI_HARNESS_MODEL")}
	env.update(extra)
	return mock.patch.dict(os.environ, env, clear=True)


def message_end(role="assistant", content="hi", **message):
	return json.dumps({"type": "message_end", "message": {"role": role, "content": content, **message}})


class InvocationTests(unittest.TestCase):
	base = dict(prompt="make the change", session_dir=Path("/tmp/session"))

	def test_the_notes_turn_continues_the_pi_session_without_the_repos_skills(self):
		prompt = build_app.HANDOFF_NOTES_PROMPT
		with tempfile.TemporaryDirectory() as workspace, clean_env():
			skill = Path(workspace) / ".agents" / "skills" / "repo-skill"
			skill.mkdir(parents=True)
			(skill / "SKILL.md").write_text("---\nname: repo-skill\n---\n")
			for adapter in (PI, PIFORK):
				turn = adapter.invocation(Path(workspace), prompt=prompt, session_dir=Path("/tmp/session"), continue_session=True, thinking=None, load_repo_skills=False)
				round_ = adapter.invocation(Path(workspace), prompt=prompt, session_dir=Path("/tmp/session"), continue_session=True, thinking=None, load_repo_skills=True)
				self.assertIn("--continue", turn)
				self.assertEqual(turn[-1], prompt)
				self.assertNotIn(str(skill), turn)
				self.assertIn(str(skill), round_)
				self.assertTrue(adapter.can_continue_session(Path("/tmp/session")))

	def test_pi_argv_is_exactly_the_recorded_shape(self):
		with clean_env():
			got = PI.invocation(Path("/tmp/work"), continue_session=False, thinking=None, **self.base)
		self.assertEqual(got, ["pi", "--print", "--mode", "json", "--session-dir", "/tmp/session", "make the change"])

	def test_pi_argv_flag_order_with_every_option(self):
		with clean_env(PI_HARNESS_PROVIDER="factory-relay", PI_HARNESS_MODEL="the-model"):
			got = PI.invocation(Path("/tmp/work"), continue_session=True, thinking="xhigh", **self.base)
		self.assertEqual(got, [
			"pi", "--print", "--mode", "json", "--session-dir", "/tmp/session",
			"--provider", "factory-relay", "--model", "the-model",
			"--thinking", "xhigh", "--continue", "make the change",
		])

	def test_pi_forwards_all_thinking_levels_and_omits_unset(self):
		with clean_env():
			for level in build_app.THINKING_LEVELS:
				argv = PI.invocation(Path("/w"), continue_session=False, thinking=level, **self.base)
				self.assertEqual(argv[argv.index("--thinking") + 1], level)
			self.assertNotIn("--thinking", PI.invocation(Path("/w"), continue_session=False, thinking=None, **self.base))

	def test_pifork_is_pi_under_another_binary(self):
		with clean_env(PI_HARNESS_MODEL="m"):
			pi = PI.invocation(Path("/w"), continue_session=True, thinking="high", **self.base)
			pifork = PIFORK.invocation(Path("/w"), continue_session=True, thinking="high", **self.base)
		self.assertEqual(pifork[0], "pifork")
		self.assertEqual(pifork[1:], pi[1:])

	def test_get_rejects_an_unknown_harness_naming_the_choices(self):
		with self.assertRaisesRegex(ValueError, "unknown harness 'nope'.*codex, copilot, pi, pifork"):
			harness_adapters.get("nope")
		self.assertEqual(sorted(harness_adapters.ADAPTERS), ["codex", "copilot", "pi", "pifork"])


class ParseTests(unittest.TestCase):
	def test_parse_reads_every_field_from_a_recorded_stream(self):
		stream = "\n".join([
			json.dumps({"type": "entry_appended", "entry": {"customType": "pi-stall-trace", "data": {"n": 1}}}),
			message_end(content=[{"type": "text", "text": "first"}], stopReason="error", errorMessage="429 slow down"),
			message_end(content=[{"type": "text", "text": "final"}]),
			json.dumps({"type": "agent_end", "messages": [
				{"role": "assistant", "usage": {"input": 3, "output": 4}},
				{"role": "assistant", "usage": {"input": 1, "output": 1}},
			]}),
			"not json",
		])
		out = PI.parse(stream)
		self.assertEqual(out.final_text, "final")
		self.assertEqual(out.last_turn_error, "")
		self.assertEqual(out.route_errors, ["429 slow down"])
		self.assertEqual(out.turn_errors, (1, 2))
		self.assertEqual(out.usage, {"input": 4, "output": 5})
		self.assertEqual(out.traces, [{"n": 1, "customType": "pi-stall-trace", "extension": "progress-stall-guard", "event": "stall"}])

	def test_parse_of_empty_output_is_all_empty(self):
		out = PIFORK.parse("")
		self.assertEqual((out.final_text, out.last_turn_error, out.route_errors, out.turn_errors, out.usage, out.traces), ("", "", [], (0, 0), None, []))

	def test_last_turn_error_names_the_final_turn_only(self):
		stream = message_end(stopReason="error", errorMessage="boom") + "\n" + message_end(content="ok")
		self.assertEqual(PI.parse(stream).last_turn_error, "")
		self.assertEqual(PI.parse(message_end(stopReason="error", errorMessage="boom")).last_turn_error, "boom")


class ProgressNoteTests(unittest.TestCase):
	def test_tool_call_and_text_notes(self):
		call = message_end(content=[{"type": "toolCall", "name": "bash", "arguments": {"command": "ls   -la"}}])
		self.assertEqual(PI.progress_note(call), "bash: ls -la")
		self.assertEqual(PI.progress_note(message_end(content=[{"type": "text", "text": "done  now"}])), "said: done now")

	def test_never_raises_on_garbage(self):
		for line in ("", "not json", "[1, 2]", json.dumps({"type": "message_end", "message": "x"})):
			self.assertIsNone(PI.progress_note(line))


class PiforkPrepareTests(unittest.TestCase):
	def test_prepare_seeds_the_agent_dir_from_the_image(self):
		with tempfile.TemporaryDirectory() as root:
			agent_dir = Path(root) / "agent"
			agent_seed = Path(root) / "agent-seed"
			(agent_seed / "npm" / "node_modules" / "pi-subagents").mkdir(parents=True)
			(agent_seed / "extensions" / "permissions").mkdir(parents=True)
			(agent_seed / "extensions" / "permissions" / "config.json").write_text('{"default":"deny"}\n')
			(agent_seed / "settings.json").write_text('{"packages":[]}\n')
			env = {
				"PI_CODING_AGENT_DIR": str(agent_dir),
				"PIFORK_AGENT_SEED": str(agent_seed),
			}
			with mock.patch.dict(os.environ, env):
				PIFORK.prepare()

			self.assertEqual((agent_dir / "extensions" / "permissions" / "config.json").read_text(), '{"default":"deny"}\n')
			self.assertEqual((agent_dir / "settings.json").read_text(), '{"packages":[]}\n')
			self.assertTrue((agent_dir / "npm").is_symlink())
			self.assertEqual((agent_dir / "auth.json").read_text(), "{}\n")

	def test_pi_prepare_touches_nothing_without_a_relay_route(self):
		with tempfile.TemporaryDirectory() as root, clean_env(PI_CODING_AGENT_DIR=root):
			before = dict(os.environ)
			PI.prepare()
			self.assertEqual(dict(os.environ), before)
			self.assertEqual(list(Path(root).iterdir()), [])

	def test_pifork_prepare_writes_the_relay_route_into_its_agent_dir(self):
		with tempfile.TemporaryDirectory() as root:
			agent_dir = Path(root) / "agent"
			env = {
				"PI_CODING_AGENT_DIR": str(agent_dir),
				"PIFORK_AGENT_SEED": str(Path(root) / "no-seed"),
				**RELAY_ENV,
			}
			with clean_env(**env):
				PIFORK.prepare()
				self.assertEqual(os.environ["PI_HARNESS_PROVIDER"], "factoryd-relay")
				self.assertEqual(os.environ["PI_HARNESS_MODEL"], "qwen38-mtplx-quality")
			self.assertEqual(json.loads((agent_dir / "models.json").read_text()), WANT_MODELS_JSON)
			self.assertEqual((agent_dir / "auth.json").read_text(), "{}\n")


# The FACTORY_MODEL_* facts and the models.json the Go side used to write for
# them (taken from internal/sandbox's TestRelayLifecyclePreparesOpenAICompatWorker
# before H2 removed the file: one provider and one model, both at the relay's
# worker URL plus "/v1").
RELAY_ENV = {
	"FACTORY_MODEL_BASE_URL": "http://model.example.test:8091/v1",
	"FACTORY_MODEL_ID": "qwen38-mtplx-quality",
	"FACTORY_MODEL_API": "openai-completions",
}
WANT_MODELS_JSON = {
	"providers": {
		"factoryd-relay": {
			"name": "factoryd-relay",
			"baseUrl": "http://model.example.test:8091/v1",
			"apiKey": "factoryd-relay-placeholder",
			"models": [
				{"id": "qwen38-mtplx-quality", "api": "openai-completions", "baseUrl": "http://model.example.test:8091/v1"}
			],
		}
	}
}
# The old Go TestRelayLifecyclePreparesOpenAICompatWorkerWithExtraJSON operator
# input: extra keys land in the model entry, and the attacker's baseUrl loses.
EXTRA_JSON = (
	'{"samplingParams":{"temperature":0.6,"top_p":0.95,"top_k":20},"reasoning":true,'
	'"compat":{"supportsDeveloperRole":false},"baseUrl":"http://attacker.invalid"}'
)


class PiRelayRouteTests(unittest.TestCase):
	def _prepare(self, **extra):
		with tempfile.TemporaryDirectory() as root:
			with clean_env(PI_CODING_AGENT_DIR=str(Path(root) / "agent"), **RELAY_ENV, **extra):
				PI.prepare()
				models = json.loads((Path(root) / "agent" / "models.json").read_text())
				argv = PI.invocation(Path("/w"), prompt="p", session_dir=Path("/s"), continue_session=False, thinking=None)
		return models, argv

	def test_renders_the_models_json_go_used_to_write(self):
		models, argv = self._prepare()
		self.assertEqual(models, WANT_MODELS_JSON)
		self.assertEqual(
			argv,
			["pi", "--print", "--mode", "json", "--session-dir", "/s", "--provider", "factoryd-relay", "--model", "qwen38-mtplx-quality", "p"],
		)

	def test_extra_json_adds_keys_but_reserved_keys_win(self):
		models, _ = self._prepare(FACTORY_MODEL_EXTRA_JSON=EXTRA_JSON)
		model = models["providers"]["factoryd-relay"]["models"][0]
		self.assertEqual(model["samplingParams"], {"temperature": 0.6, "top_p": 0.95, "top_k": 20})
		self.assertIs(model["reasoning"], True)
		self.assertEqual(model["compat"], {"supportsDeveloperRole": False})
		self.assertEqual(model["baseUrl"], "http://model.example.test:8091/v1")
		self.assertEqual(models["providers"]["factoryd-relay"]["baseUrl"], "http://model.example.test:8091/v1")

	def test_id_api_and_base_url_in_extra_json_never_override(self):
		extra = '{"id":"attacker","api":"anthropic-messages","baseUrl":"http://attacker.invalid"}'
		models, _ = self._prepare(FACTORY_MODEL_EXTRA_JSON=extra)
		self.assertEqual(models, WANT_MODELS_JSON)

	def test_a_non_object_extra_json_is_an_error(self):
		with tempfile.TemporaryDirectory() as root:
			with clean_env(PI_CODING_AGENT_DIR=root, FACTORY_MODEL_EXTRA_JSON="[1]", **RELAY_ENV):
				with self.assertRaises(ValueError):
					PI.prepare()

	def test_default_agent_dir_is_under_home(self):
		with tempfile.TemporaryDirectory() as home:
			env = {k: v for k, v in os.environ.items() if k != "PI_CODING_AGENT_DIR"}
			env.update(HOME=home, **RELAY_ENV)
			with mock.patch.dict(os.environ, env, clear=True):
				PI.prepare()
			self.assertEqual(json.loads((Path(home) / ".pi" / "agent" / "models.json").read_text()), WANT_MODELS_JSON)


class PiforkThroughSharedOrchestrationTests(unittest.TestCase):
	def _workspace_and_spec(self, root):
		workspace = Path(root) / "workspace"
		workspace.mkdir()
		spec = workspace / "ticket.md"
		spec.write_text("Verify-Command: true\n")
		return workspace, spec

	def test_run_review_turn_launches_pifork_and_parses_its_output(self):
		commands = []

		def fake_sh(args, cwd=None, timeout=None, env=None):
			commands.append(args)
			return subprocess.CompletedProcess(args, 0, message_end(content="hello"), "")

		with tempfile.TemporaryDirectory() as root, mock.patch.object(build_app, "sh", side_effect=fake_sh):
			workspace = Path(root)
			turn = build_app.run_review_turn(
				workspace, prompt="review this", session_dir=workspace / ".session",
				review_base_sha=None, thinking=None, adapter=PIFORK,
			)
		self.assertEqual((turn.text, turn.error), ("hello", ""))
		self.assertEqual([c[0] for c in commands if "--print" in c], ["pifork"])

	def test_run_spec_conformity_review_launches_pifork(self):
		commands = []

		def fake_sh(args, cwd=None, timeout=None, env=None):
			commands.append(args)
			payload = json.dumps({"criteria": [{"criterion": "1. Widget exists", "verdict": "clean"}]})
			return subprocess.CompletedProcess(args, 0, message_end(content=payload), "")

		with tempfile.TemporaryDirectory() as root, mock.patch.object(build_app, "sh", side_effect=fake_sh):
			verdicts, error = build_app.run_spec_conformity_review(
				Path(root), criteria=["1. Widget exists"], review_base_sha=None, thinking=None, adapter=PIFORK,
			)
		self.assertEqual((verdicts[0]["verdict"], error), ("clean", ""))
		self.assertEqual([c[0] for c in commands if "--print" in c], ["pifork"])

	def test_corrective_rounds_continue_through_the_pifork_adapter(self):
		with tempfile.TemporaryDirectory() as root:
			workspace, spec = self._workspace_and_spec(root)
			model_commands = []

			def fake_run_agent_streaming(command, **kwargs):
				model_commands.append(command)
				return subprocess.CompletedProcess(command, 0, "", ""), False

			with (
				mock.patch.object(build_app, "run_agent_streaming", side_effect=fake_run_agent_streaming),
				mock.patch.object(build_app, "workspace_fingerprint", side_effect=[("before-1",), ("after-1",), ("before-2",), ("after-2",)]),
				mock.patch.object(build_app, "run_verification", side_effect=[
					("make verify", False, False, "first failure", False, None),
					("make verify", True, False, "", False, None),
				]),
			):
				result = build_app.run_build(
					workspace, spec, max_rounds=2, timeout_minutes=1, review_policy="advisory", adapter=PIFORK,
				)

			self.assertTrue(result.succeeded)
			self.assertEqual([r.agent for r in result.rounds], ["pifork", "pifork"])
			self.assertEqual(model_commands[0][0:4], ["pifork", "--print", "--mode", "json"])
			self.assertIn("--continue", model_commands[1])

	def test_pifork_prompt_names_the_launched_compose_services(self):
		with tempfile.TemporaryDirectory() as root:
			workspace, spec = self._workspace_and_spec(root)
			model_commands = []

			def fake_run_agent_streaming(command, **kwargs):
				model_commands.append(command)
				return subprocess.CompletedProcess(command, 0, "", ""), False

			with (
				mock.patch.dict(os.environ, {"BG_COMPOSE_SERVICES": "up", "BG_SERVICE_POSTGRES": "postgres", "BG_SERVICE_POSTGRES_PORT": "5432"}),
				mock.patch.object(build_app, "run_agent_streaming", side_effect=fake_run_agent_streaming),
				mock.patch.object(build_app, "workspace_fingerprint", side_effect=[("before",), ("after",)]),
				mock.patch.object(build_app, "run_verification", return_value=("make verify", True, False, "", False, None)),
				contextlib.redirect_stderr(io.StringIO()),
			):
				build_app.run_build(workspace, spec, max_rounds=1, timeout_minutes=1, review_policy="advisory", adapter=PIFORK)

			self.assertEqual(model_commands[0][0], "pifork")
			self.assertIn("reachable by host name: postgres:5432", model_commands[0][-1])


class MainHarnessFlagTests(unittest.TestCase):
	ARGV = [str(SCRIPTS / "build_app.py"), "--workspace", "/tmp/work", "--spec", "/tmp/spec.md"]

	def _run_main(self, extra):
		result = mock.Mock(succeeded=True, stopped_reason="ok")
		with (
			mock.patch.object(build_app, "run_build", return_value=result) as run_build,
			mock.patch.object(build_app, "write_report", return_value=Path("/tmp/report.md")),
			mock.patch.object(build_app, "write_evidence_json", return_value=Path("/tmp/evidence.json")),
			mock.patch.object(PIFORK, "prepare") as pifork_prepare,
			mock.patch.object(sys, "argv", self.ARGV + extra),
			contextlib.redirect_stdout(io.StringIO()),
		):
			self.assertEqual(build_app.main(), 0)
		return run_build.call_args.kwargs["adapter"], pifork_prepare

	def test_default_harness_is_pi_and_needs_no_preparation(self):
		adapter, pifork_prepare = self._run_main([])
		self.assertIs(adapter, PI)
		pifork_prepare.assert_not_called()

	def test_harness_pifork_selects_and_prepares_the_pifork_adapter(self):
		adapter, pifork_prepare = self._run_main(["--harness", "pifork"])
		self.assertIs(adapter, PIFORK)
		pifork_prepare.assert_called_once_with()

	def test_unknown_harness_is_rejected_by_argparse(self):
		with mock.patch.object(sys, "argv", self.ARGV + ["--harness", "nope"]), contextlib.redirect_stderr(io.StringIO()):
			with self.assertRaises(SystemExit) as raised:
				build_app.main()
		self.assertEqual(raised.exception.code, 2)


class HarnessFlagTests(unittest.TestCase):
	"""Every model-bound script exposes --harness with the adapters' names, default pi."""

	SCRIPT_NAMES = ("build_app", "code_review", "combined_review", "conformity_review", "draft_spec", "plan_tickets", "draft_acceptance_oracles")

	def test_each_script_lists_the_adapter_choices(self):
		for name in self.SCRIPT_NAMES:
			with self.subTest(script=name):
				text = (SCRIPTS / f"{name}.py").read_text()
				self.assertIn('"--harness", choices=sorted(harness_adapters.ADAPTERS), default="pi"', text)
				self.assertIn("adapter.prepare()", text)

	def test_no_script_still_accepts_containment(self):
		for path in SCRIPTS.glob("*.py"):
			with self.subTest(script=path.name):
				self.assertNotIn("--containment", path.read_text())


if __name__ == "__main__":
	unittest.main()


class PiPlaceholderEnvTests(unittest.TestCase):
	PLACEHOLDERS = {
		"FACTORY_MODEL_KEY_ENV": "BG_CHATGPT_TOKEN",
		"FACTORY_MODEL_HEADERS_JSON": '{"chatgpt-account-id": "BG_CHATGPT_ACCOUNT"}',
	}

	def _config(self, **extra):
		with clean_env(**RELAY_ENV, **extra):
			return harness_adapters.pi_models_config()

	def test_names_the_placeholder_variables_instead_of_the_relay_placeholder(self):
		self.assertEqual(self._config(**self.PLACEHOLDERS), {
			"providers": {
				"factoryd-relay": {
					"name": "factoryd-relay",
					"baseUrl": "http://model.example.test:8091/v1",
					"apiKey": "$BG_CHATGPT_TOKEN",
					"headers": {"chatgpt-account-id": "$BG_CHATGPT_ACCOUNT"},
					"models": [
						{"id": "qwen38-mtplx-quality", "api": "openai-completions", "baseUrl": "http://model.example.test:8091/v1"}
					],
				}
			}
		})

	def test_key_env_alone_adds_no_headers(self):
		provider = self._config(FACTORY_MODEL_KEY_ENV="BG_CHATGPT_TOKEN")["providers"]["factoryd-relay"]
		self.assertEqual(provider["apiKey"], "$BG_CHATGPT_TOKEN")
		self.assertNotIn("headers", provider)

	def test_unset_is_the_relay_config(self):
		self.assertEqual(self._config(), WANT_MODELS_JSON)

	def test_malformed_values_are_errors(self):
		cases = [
			({"FACTORY_MODEL_KEY_ENV": "bad-name"}, "FACTORY_MODEL_KEY_ENV 'bad-name'"),
			({"FACTORY_MODEL_KEY_ENV": "BG_X", "FACTORY_MODEL_HEADERS_JSON": "[1]"}, "must be a JSON object"),
			({"FACTORY_MODEL_KEY_ENV": "BG_X", "FACTORY_MODEL_HEADERS_JSON": '{"a": "lower"}'}, "not an environment variable name"),
			({"FACTORY_MODEL_KEY_ENV": "BG_X", "FACTORY_MODEL_HEADERS_JSON": '{"a": 1}'}, "not an environment variable name"),
			({"FACTORY_MODEL_KEY_ENV": "BG_X", "FACTORY_MODEL_HEADERS_JSON": '{"bad header": "BG_Y"}'}, "header name 'bad header'"),
			({"FACTORY_MODEL_HEADERS_JSON": '{"a": "BG_Y"}'}, "set without FACTORY_MODEL_KEY_ENV"),
		]
		for extra, message in cases:
			with self.subTest(extra=extra):
				with self.assertRaisesRegex(SystemExit, message):
					self._config(**extra)
