"""Codex CLI and Copilot CLI adapters: parsers pinned to real recorded output
(tests/fixtures, captured 2026-09-29 in the production sandbox against the
run's relay: Codex CLI 0.154.0 via Luna, Copilot CLI 1.0.88 via a local Qwen
over chat completions) and the exact argv/environment each launches with."""

import contextlib
import importlib.util
import os
import subprocess
import sys
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

SCRIPTS = Path(__file__).resolve().parents[1] / "scripts"
FIXTURES = Path(__file__).resolve().parent / "fixtures"


def _load(name):
	spec = importlib.util.spec_from_file_location(name, SCRIPTS / f"{name}.py")
	assert spec and spec.loader
	module = importlib.util.module_from_spec(spec)
	sys.modules[spec.name] = module
	spec.loader.exec_module(module)
	return module


build_app = _load("build_app")
harness_adapters = sys.modules["harness_adapters"]
CODEX = harness_adapters.get("codex")
COPILOT = harness_adapters.get("copilot")

RELAY_URL = "http://model.example.test:8091/backend-api/codex"


def fixture(name: str) -> str:
	return (FIXTURES / name).read_text()


def route(api="openai-responses", **extra):
	"""os.environ with exactly the FACTORY_MODEL_* route facts factoryd hands a
	relay-routed worker (plus anything in extra)."""
	env = {
		"FACTORY_MODEL_ID": "gpt-5.6-luna",
		"FACTORY_MODEL_BASE_URL": RELAY_URL,
		"FACTORY_MODEL_API": api,
		"PATH": "/usr/bin",
	}
	env.update(extra)
	return mock.patch.dict(os.environ, env, clear=True)


def notes(adapter, stdout):
	return [n for n in (adapter.progress_note(line) for line in stdout.splitlines()) if n]


class CodexParseTests(unittest.TestCase):
	def test_round_one(self):
		out = CODEX.parse(fixture("codex_round1.jsonl"))
		self.assertIn("hi", out.final_text)
		self.assertTrue(out.final_text.startswith("Created `probe_hello.txt`"))
		self.assertEqual(out.turn_errors, (0, 1))
		self.assertEqual((out.last_turn_error, out.route_errors, out.traces), ("", [], []))
		# Codex counts the 6912 cached tokens inside input_tokens=14653; Pi's
		# shape keeps `input` free of them and totals the four parts.
		self.assertEqual(out.usage, {
			"input": 14653 - 6912, "cacheRead": 6912, "cacheWrite": 0,
			"output": 174, "reasoning": 37, "totalTokens": 14653 + 174,
		})

	def test_resumed_round_reports_its_own_invocation_only(self):
		# The recorded resumed invocation's turn.completed says output 315,
		# not 174 + 315: `codex exec resume` starts its running total afresh,
		# so usage is per invocation and needs no baseline from the earlier
		# round (checked against the recorded stream, 2026-09-29).
		out = CODEX.parse(fixture("codex_resume.jsonl"))
		self.assertIn("there", out.final_text)
		self.assertEqual(out.usage["output"], 315)
		self.assertEqual(out.usage["cacheRead"], 13824)
		self.assertEqual(out.usage["input"], 29761 - 13824)
		self.assertEqual(out.turn_errors, (0, 1))

	def test_usage_sums_across_turns_within_one_stream(self):
		out = CODEX.parse(fixture("codex_round1.jsonl") + fixture("codex_resume.jsonl"))
		self.assertEqual(out.usage["output"], 174 + 315)
		self.assertEqual(out.usage["cacheRead"], 6912 + 13824)
		self.assertEqual(out.turn_errors, (0, 2))

	def test_a_failed_final_turn_has_no_final_text(self):
		out = CODEX.parse(fixture("codex_round1.jsonl") + fixture("codex_model_error.jsonl"))
		self.assertEqual(out.final_text, "")
		self.assertEqual(out.turn_errors, (1, 2))

	def test_model_error_reads_as_all_errored_with_the_upstream_detail(self):
		out = CODEX.parse(fixture("codex_model_error.jsonl"))
		self.assertEqual(out.turn_errors, (1, 1))
		self.assertEqual(out.final_text, "")
		self.assertEqual(len(out.route_errors), 1)
		self.assertIn("'no-such-model-xyz' model is not supported", out.route_errors[0])
		self.assertEqual(out.last_turn_error, out.route_errors[0])
		self.assertIsNone(out.usage)

	def test_a_later_completed_turn_clears_the_last_turn_error(self):
		out = CODEX.parse(fixture("codex_model_error.jsonl") + fixture("codex_round1.jsonl"))
		self.assertEqual(out.last_turn_error, "")
		self.assertEqual(out.turn_errors, (1, 2))
		self.assertEqual(len(out.route_errors), 1)

	def test_an_error_before_any_turn_is_all_errored(self):
		out = CODEX.parse('{"type":"error","message":"no route"}\n')
		self.assertEqual(out.turn_errors, (1, 1))
		self.assertEqual(out.route_errors, ["no route"])

	def test_repeated_errors_are_listed_once_in_order(self):
		stream = "\n".join([
			'{"type":"turn.started"}',
			'{"type":"error","message":"Reconnecting... 1/5"}',
			'{"type":"error","message":"Reconnecting... 1/5"}',
			'{"type":"turn.failed","error":{"message":"boom"}}',
		])
		self.assertEqual(CODEX.parse(stream).route_errors, ["Reconnecting... 1/5", "boom"])

	def test_empty_and_garbled_output(self):
		for stdout in ("", "not json\n[1,2]\n"):
			out = CODEX.parse(stdout)
			self.assertEqual((out.final_text, out.turn_errors, out.usage, out.route_errors), ("", (0, 0), None, []))

	def test_progress_notes_from_the_recorded_run(self):
		self.assertEqual(notes(CODEX, fixture("codex_round1.jsonl")), [
			"said: I’ll create the file with the exact contents, then run the requested command and report what it prints.",
			"write: /workspace/probe_hello.txt",
			"bash: cat probe_hello.txt",
			"said: Created `probe_hello.txt`. The command output was: ```text hi ```",
		])
		self.assertIn("edit: /workspace/probe_hello.txt", notes(CODEX, fixture("codex_resume.jsonl")))

	def test_progress_note_never_raises_and_bounds_length(self):
		for line in ("", "nope", "[]", '{"item": 3}', '{"type":"item.started","item":{"type":"file_change","changes":"x"}}'):
			self.assertIsNone(CODEX.progress_note(line))
		long_command = '{"type":"item.started","item":{"type":"command_execution","command":"%s"}}' % ("x" * 500)
		self.assertEqual(len(CODEX.progress_note(long_command)), len("bash: ") + 120)


class CodexInvocationTests(unittest.TestCase):
	def setUp(self):
		self.tmp = tempfile.TemporaryDirectory()
		self.addCleanup(self.tmp.cleanup)
		self.session = Path(self.tmp.name) / "session"

	def argv(self, *, cont=False, thinking=None, prompt="do it"):
		return CODEX.invocation(Path("/work"), prompt=prompt, session_dir=self.session, continue_session=cont, thinking=thinking)

	def test_fresh_round_argv_is_exactly_this(self):
		with route():
			got = self.argv()
		home = self.session / "codex-home"
		self.assertEqual(got, [
			"env", f"CODEX_HOME={home}", "FACTORYD_RELAY_KEY=factoryd-relay-placeholder",
			"codex", "exec",
			"--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
			"-m", "gpt-5.6-luna",
			"-c", "model_provider=relay",
			"-c", 'model_providers.relay={name="relay",base_url="%s",wire_api="responses",env_key="FACTORYD_RELAY_KEY"}' % RELAY_URL,
			"-c", "features.unbounded_connection_retries=false",
			"-c", "features.plugins=false",
			"-c", "features.apps=false",
			"-c", "analytics.enabled=false",
			"-c", "feedback.enabled=false",
			"-c", "check_for_update_on_startup=false",
			"-c", 'web_search="disabled"',
			"-c", "skills.bundled.enabled=false",
			"do it",
		])
		self.assertTrue(home.is_dir())

	def test_continued_round_resumes_the_last_session(self):
		with route():
			fresh, cont = self.argv(), self.argv(cont=True)
		self.assertEqual(cont[cont.index("codex"):cont.index("codex") + 4], ["codex", "exec", "resume", "--last"])
		self.assertNotIn("resume", fresh)
		# Same flags either way: everything after the subcommand matches.
		self.assertEqual([a for a in cont if a not in ("resume", "--last")], fresh)

	def test_state_lives_under_the_session_dir_and_key_is_the_placeholder(self):
		with route(OPENAI_API_KEY="sk-real", CODEX_HOME="/home/x/.codex"):
			got = self.argv()
		self.assertIn(f"CODEX_HOME={self.session / 'codex-home'}", got)
		self.assertIn("FACTORYD_RELAY_KEY=factoryd-relay-placeholder", got)
		self.assertNotIn("sk-real", " ".join(got))
		self.assertEqual(got[-1], "do it")

	def test_thinking_maps_off_to_none_and_passes_the_rest(self):
		with route():
			self.assertNotIn("model_reasoning_effort", " ".join(self.argv()))
			self.assertIn('model_reasoning_effort="none"', self.argv(thinking="off"))
			for level in ("minimal", "low", "medium", "high", "xhigh", "max"):
				self.assertIn(f'model_reasoning_effort="{level}"', self.argv(thinking=level))

	def test_the_prompt_is_one_argv_element_whatever_it_contains(self):
		prompt = "line one\n--json -c 'x=y' $(rm -rf /)"
		with route():
			self.assertEqual(self.argv(prompt=prompt)[-1], prompt)

	def test_prepare_refuses_without_a_route_or_off_the_responses_api(self):
		with route():
			CODEX.prepare()
		with mock.patch.dict(os.environ, {}, clear=True):
			with self.assertRaisesRegex(SystemExit, "FACTORY_MODEL_ID and FACTORY_MODEL_BASE_URL not set"):
				CODEX.prepare()
			with self.assertRaisesRegex(SystemExit, "FACTORY_MODEL_ID and FACTORY_MODEL_BASE_URL not set"):
				self.argv()
		for api in ("openai-completions", "anthropic-messages", ""):
			with route(api):
				with self.assertRaisesRegex(SystemExit, "speaks only the Responses API"):
					CODEX.prepare()


class CodexPlaceholderEnvTests(unittest.TestCase):
	def argv(self):
		with tempfile.TemporaryDirectory() as tmp:
			return CODEX.invocation(Path("/work"), prompt="do it", session_dir=Path(tmp) / "s", continue_session=False, thinking=None)

	def test_provider_names_the_placeholder_variables_and_no_assignment_is_added(self):
		with route(FACTORY_MODEL_KEY_ENV="BG_CHATGPT_TOKEN", FACTORY_MODEL_HEADERS_JSON='{"chatgpt-account-id": "BG_CHATGPT_ACCOUNT"}'):
			got = self.argv()
		self.assertIn(
			'model_providers.relay={name="relay",base_url="%s",wire_api="responses",env_key="BG_CHATGPT_TOKEN",'
			'env_http_headers={"chatgpt-account-id"="BG_CHATGPT_ACCOUNT"}}' % RELAY_URL,
			got,
		)
		self.assertEqual(got[0], "env")
		self.assertTrue(got[1].startswith("CODEX_HOME="))
		self.assertEqual(got[2], "codex")
		self.assertNotIn("factoryd-relay-placeholder", " ".join(got))
		self.assertFalse(any(a.startswith("FACTORYD_RELAY_KEY=") or a.startswith("BG_CHATGPT_TOKEN=") for a in got))

	def test_key_env_alone_has_no_header_table(self):
		with route(FACTORY_MODEL_KEY_ENV="BG_CHATGPT_TOKEN"):
			got = self.argv()
		self.assertIn(
			'model_providers.relay={name="relay",base_url="%s",wire_api="responses",env_key="BG_CHATGPT_TOKEN"}' % RELAY_URL,
			got,
		)

	def test_malformed_values_are_errors(self):
		for extra, message in (
			({"FACTORY_MODEL_KEY_ENV": "bad-name"}, "FACTORY_MODEL_KEY_ENV 'bad-name'"),
			({"FACTORY_MODEL_KEY_ENV": "BG_X", "FACTORY_MODEL_HEADERS_JSON": '"x"'}, "must be a JSON object"),
			({"FACTORY_MODEL_KEY_ENV": "BG_X", "FACTORY_MODEL_HEADERS_JSON": '{"a": "no good"}'}, "not an environment variable name"),
			({"FACTORY_MODEL_HEADERS_JSON": '{"a": "BG_Y"}'}, "set without FACTORY_MODEL_KEY_ENV"),
		):
			with self.subTest(extra=extra), route(**extra):
				with self.assertRaisesRegex(SystemExit, message):
					self.argv()


class CopilotParseTests(unittest.TestCase):
	def test_round_one(self):
		out = COPILOT.parse(fixture("copilot_round1.jsonl"))
		self.assertTrue(out.final_text.startswith("Done. Created"))
		self.assertIn("hi", out.final_text)
		self.assertIsNone(out.usage)
		self.assertEqual(out.turn_errors, (0, 3))
		self.assertEqual((out.last_turn_error, out.route_errors, out.traces), ("", [], []))

	def test_resume_round(self):
		out = COPILOT.parse(fixture("copilot_resume.jsonl"))
		self.assertTrue(out.final_text.startswith("Done. Appended `there`"))
		self.assertEqual(out.turn_errors, (0, 3))

	def test_model_error_reads_as_all_errored_with_the_upstream_detail(self):
		out = COPILOT.parse(fixture("copilot_model_error.jsonl"))
		self.assertEqual(out.turn_errors, (1, 1))
		self.assertEqual(out.final_text, "")
		self.assertEqual(out.route_errors, ["400 Bad Request"])
		self.assertEqual(out.last_turn_error, "400 Bad Request")
		self.assertIsNone(out.usage)

	def test_a_session_error_before_any_turn_is_all_errored(self):
		out = COPILOT.parse('{"type":"session.error","data":{"message":"no auth","statusCode":401}}\n')
		self.assertEqual(out.turn_errors, (1, 1))
		self.assertEqual(out.route_errors, ["no auth"])

	def test_a_retried_call_that_then_succeeds_is_not_an_errored_turn(self):
		stream = "\n".join([
			'{"type":"assistant.turn_start","data":{"turnId":"0"}}',
			'{"type":"model.call_failure","data":{"errorMessage":"\\"429 slow down\\""}}',
			'{"type":"model.call_finished","data":{"turnId":"0","outcome":"error"}}',
			'{"type":"model.call_finished","data":{"turnId":"0","outcome":"success"}}',
			'{"type":"assistant.message","data":{"content":"ok"}}',
		])
		out = COPILOT.parse(stream)
		self.assertEqual(out.turn_errors, (0, 1))
		self.assertEqual(out.last_turn_error, "")
		self.assertEqual(out.route_errors, ["429 slow down"])

	def test_a_failed_final_turn_has_no_final_text(self):
		out = COPILOT.parse(fixture("copilot_round1.jsonl") + fixture("copilot_model_error.jsonl"))
		self.assertEqual(out.final_text, "")
		self.assertEqual(out.turn_errors, (1, 3))  # both recordings number their turns from 0

	def test_final_text_is_the_last_non_empty_message(self):
		stream = "\n".join([
			'{"type":"assistant.message","data":{"content":"first"}}',
			'{"type":"assistant.message","data":{"content":"  ","toolRequests":[{}]}}',
		])
		self.assertEqual(COPILOT.parse(stream).final_text, "first")

	def test_empty_and_garbled_output(self):
		for stdout in ("", "not json\n[1]\n", '{"type":"assistant.message","data":"x"}\n'):
			out = COPILOT.parse(stdout)
			self.assertEqual((out.final_text, out.turn_errors, out.usage, out.route_errors), ("", (0, 0), None, []))

	def test_progress_notes_from_the_recorded_runs(self):
		one = notes(COPILOT, fixture("copilot_round1.jsonl"))
		self.assertEqual(one[0], "write: /workspace/probe_hello.txt")
		self.assertEqual(one[1], "bash: cat probe_hello.txt")
		self.assertTrue(one[2].startswith("said: Done. Created"))
		self.assertEqual(len(one), 3)
		self.assertEqual(notes(COPILOT, fixture("copilot_resume.jsonl"))[0], "edit: /workspace/probe_hello.txt")

	def test_view_and_apply_patch_notes(self):
		def start(tool, arguments):
			import json
			return json.dumps({"type": "tool.execution_start", "data": {"toolName": tool, "arguments": arguments}})

		self.assertEqual(COPILOT.progress_note(start("view", {"path": "/w/a.go"})), "read: /w/a.go")
		patch = "*** Begin Patch\n*** Add File: pkg/new.go\n+x\n*** Update File: old.go\n*** End Patch"
		self.assertEqual(COPILOT.progress_note(start("apply_patch", {"input": patch})), "write: pkg/new.go")
		self.assertEqual(COPILOT.progress_note(start("apply_patch", patch)), "write: pkg/new.go")
		self.assertEqual(COPILOT.progress_note(start("apply_patch", "*** Update File: src/a.py\n")), "edit: src/a.py")
		self.assertIsNone(COPILOT.progress_note(start("apply_patch", "no header")))
		self.assertIsNone(COPILOT.progress_note(start("grep", {"pattern": "x"})))
		for line in ("", "nope", "[]", '{"type":"tool.execution_start","data":null}'):
			self.assertIsNone(COPILOT.progress_note(line))


class CopilotInvocationTests(unittest.TestCase):
	def setUp(self):
		self.tmp = tempfile.TemporaryDirectory()
		self.addCleanup(self.tmp.cleanup)
		self.session = Path(self.tmp.name) / "session"

	def argv(self, *, cont=False, thinking=None, prompt="do it"):
		return COPILOT.invocation(Path("/work"), prompt=prompt, session_dir=self.session, continue_session=cont, thinking=thinking)

	@staticmethod
	def session_id(argv):
		for flag in ("--session-id", "--resume"):
			if flag in argv:
				return flag, argv[argv.index(flag) + 1]
		raise AssertionError(f"no session flag in {argv}")

	def test_fresh_round_argv_and_environment_are_exactly_this(self):
		with route("openai-completions"):
			got = self.argv()
		flag, sid = self.session_id(got)
		self.assertEqual(flag, "--session-id")
		self.assertEqual(got, [
			"env",
			f"COPILOT_HOME={self.session / 'copilot-home'}",
			f"COPILOT_PROVIDER_BASE_URL={RELAY_URL}",
			"COPILOT_PROVIDER_TYPE=openai",
			"COPILOT_PROVIDER_WIRE_API=completions",
			"COPILOT_PROVIDER_API_KEY=factoryd-relay-placeholder",
			"COPILOT_MODEL=gpt-5.6-luna",
			"COPILOT_OFFLINE=true",
			"COPILOT_AUTO_UPDATE=false",
			"copilot", "--allow-all-tools", "--no-auto-update", "--output-format", "json",
			"--session-id", sid,
			"-p", "do it",
		])
		self.assertEqual((self.session / "copilot-session-id").read_text().strip(), sid)

	def test_a_continued_round_resumes_the_stored_session(self):
		with route():
			first = self.argv()
			second = self.argv(cont=True)
		self.assertEqual(self.session_id(first)[0], "--session-id")
		self.assertEqual(self.session_id(second), ("--resume", self.session_id(first)[1]))
		self.assertNotIn("--session-id", second)

	def test_a_continued_round_without_a_stored_id_starts_fresh(self):
		with route():
			got = self.argv(cont=True)
		self.assertEqual(self.session_id(got)[0], "--session-id")

	def test_each_fresh_round_gets_its_own_session_id(self):
		with route():
			self.assertNotEqual(self.session_id(self.argv())[1], self.session_id(self.argv())[1])

	def test_wire_api_and_provider_type_follow_the_route(self):
		for api, want in (
			("openai-completions", ["COPILOT_PROVIDER_TYPE=openai", "COPILOT_PROVIDER_WIRE_API=completions"]),
			("openai-responses", ["COPILOT_PROVIDER_TYPE=openai", "COPILOT_PROVIDER_WIRE_API=responses"]),
		):
			with route(api):
				got = self.argv()
			for entry in want:
				self.assertIn(entry, got)
		with route("anthropic-messages"):
			got = self.argv()
		self.assertIn("COPILOT_PROVIDER_TYPE=anthropic", got)
		self.assertFalse([a for a in got if a.startswith("COPILOT_PROVIDER_WIRE_API")])

	def test_never_trusts_the_repo_and_never_carries_a_real_key(self):
		with route(COPILOT_ALLOW_ALL="true", GH_TOKEN="ghp_real", COPILOT_GITHUB_TOKEN="ghp_real"):
			got = self.argv()
		joined = " ".join(got)
		self.assertNotIn("COPILOT_ALLOW_ALL", joined)
		self.assertNotIn("ghp_real", joined)
		self.assertIn("--allow-all-tools", got)
		self.assertIn("COPILOT_OFFLINE=true", got)
		self.assertEqual(got[-2:], ["-p", "do it"])

	def test_thinking_maps_off_to_none_and_passes_the_rest(self):
		with route():
			self.assertNotIn("--reasoning-effort", self.argv())
			off = self.argv(thinking="off")
			self.assertEqual(off[off.index("--reasoning-effort") + 1], "none")
			for level in ("minimal", "low", "medium", "high", "xhigh", "max"):
				got = self.argv(thinking=level)
				self.assertEqual(got[got.index("--reasoning-effort") + 1], level)

	def test_prepare_refuses_without_a_route(self):
		with route():
			COPILOT.prepare()
		with mock.patch.dict(os.environ, {"FACTORY_MODEL_BASE_URL": RELAY_URL}, clear=True):
			with self.assertRaisesRegex(SystemExit, "FACTORY_MODEL_ID not set"):
				COPILOT.prepare()
		with mock.patch.dict(os.environ, {"FACTORY_MODEL_ID": "m"}, clear=True):
			with self.assertRaisesRegex(SystemExit, "FACTORY_MODEL_BASE_URL not set"):
				COPILOT.prepare()


class CopilotPlaceholderEnvTests(unittest.TestCase):
	def _invocation(self, tmp):
		return COPILOT.invocation(Path("/work"), prompt="p", session_dir=Path(tmp) / "s", continue_session=False, thinking=None)

	def test_provider_key_is_the_routes_placeholder(self):
		with tempfile.TemporaryDirectory() as tmp, route("openai-completions", FACTORY_MODEL_KEY_ENV="BG_MODEL_KEY", BG_MODEL_KEY="openshell:placeholder"):
			args = self._invocation(tmp)
		self.assertIn("COPILOT_PROVIDER_API_KEY=openshell:placeholder", args)
		self.assertNotIn(f"COPILOT_PROVIDER_API_KEY={harness_adapters.RELAY_API_KEY_PLACEHOLDER}", args)

	def test_route_with_no_credential_keeps_the_fixed_placeholder(self):
		with tempfile.TemporaryDirectory() as tmp, route("openai-completions"):
			args = self._invocation(tmp)
		self.assertIn(f"COPILOT_PROVIDER_API_KEY={harness_adapters.RELAY_API_KEY_PLACEHOLDER}", args)

	def test_refuses_a_route_that_needs_extra_credential_headers(self):
		env = {"FACTORY_MODEL_KEY_ENV": "BG_CHATGPT_TOKEN", "FACTORY_MODEL_HEADERS_JSON": '{"chatgpt-account-id": "BG_CHATGPT_ACCOUNT"}'}
		with tempfile.TemporaryDirectory() as tmp, route("openai-responses", **env):
			with self.assertRaisesRegex(SystemExit, "chatgpt-account-id.*no output items.*pi or codex"):
				self._invocation(tmp)

	def test_refuses_a_key_name_with_no_value(self):
		with tempfile.TemporaryDirectory() as tmp, route("openai-completions", FACTORY_MODEL_KEY_ENV="BG_MODEL_KEY"):
			with self.assertRaisesRegex(SystemExit, "BG_MODEL_KEY .* is not set"):
				self._invocation(tmp)


class AgentDigestTests(unittest.TestCase):
	"""The oracle drafter's bounded streaming digest reads each harness's own
	events through its adapter's line_event hook."""

	def digest(self, adapter, name):
		draft = _load("draft_acceptance_oracles")
		d = draft.AgentDigest(adapter)
		for line in fixture(name).splitlines():
			d.feed(line)
		return d

	def test_codex_round(self):
		d = self.digest(CODEX, "codex_round1.jsonl")
		self.assertTrue(d.last_full_text.startswith("Created `probe_hello.txt`"))
		self.assertEqual((d.turns, d.errored, d.last_error), (1, 0, None))
		self.assertEqual(d.usage()["output"], 174)
		self.assertEqual(d.usage()["cacheRead"], 6912)

	def test_codex_model_error(self):
		d = self.digest(CODEX, "codex_model_error.jsonl")
		self.assertIsNone(d.last_full_text)
		self.assertIsNone(d.usage())
		self.assertEqual((d.turns, d.errored), (1, 1))
		self.assertIn("'no-such-model-xyz' model is not supported", d.last_error)
		self.assertIn("codex events", d.summary())
		self.assertIn("not supported", d.summary())

	def test_copilot_round(self):
		d = self.digest(COPILOT, "copilot_round1.jsonl")
		self.assertTrue(d.last_full_text.startswith("Done. Created"))
		self.assertIsNone(d.usage())
		self.assertEqual((d.turns, d.errored, d.last_error), (3, 0, None))

	def test_copilot_model_error(self):
		d = self.digest(COPILOT, "copilot_model_error.jsonl")
		self.assertIsNone(d.last_full_text)
		self.assertIsNone(d.usage())
		self.assertEqual((d.turns, d.errored), (1, 1))
		self.assertEqual(d.last_error, "400 Bad Request")

	def test_a_failed_final_turn_drops_the_earlier_text(self):
		d = self.digest(CODEX, "codex_round1.jsonl")
		for line in fixture("codex_model_error.jsonl").splitlines():
			d.feed(line)
		self.assertIsNone(d.last_full_text)

	def test_pi_events_still_digest(self):
		pi = harness_adapters.get("pi")
		draft = _load("draft_acceptance_oracles")
		d = draft.AgentDigest(pi)
		for line in (
			'{"type":"message_start"}',
			'{"type":"message_update","assistantMessageEvent":{"delta":"abc"}}',
			'{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}',
			'{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"boom","content":[]}}',
			'{"type":"agent_end","messages":[{"role":"assistant","usage":{"input":3,"output":4}}]}',
		):
			d.feed(line)
		self.assertEqual((d.turns, d.errored, d.last_error, d.last_full_text), (2, 1, "boom", "done"))
		self.assertEqual(d.usage(), {"input": 3, "output": 4})


class LaunchTests(unittest.TestCase):
	"""The launch helpers give every agent subprocess /dev/null for stdin:
	Codex reads stdin whenever it is not a TTY and would otherwise wait on it."""

	def test_agent_launch_helpers_close_stdin(self):
		seen = []

		def fake_popen(*args, **kwargs):
			seen.append(kwargs.get("stdin"))
			raise OSError("stop here")

		with mock.patch.object(subprocess, "Popen", side_effect=fake_popen):
			completed, _ = build_app.run_agent_streaming(["codex"], cwd=Path("/"), timeout=1, env={}, on_event=lambda line: None)
		self.assertEqual(completed.returncode, 127)
		with mock.patch.object(subprocess, "run") as run:
			build_app.sh(["codex"])
		self.assertEqual(run.call_args.kwargs["stdin"], subprocess.DEVNULL)
		self.assertEqual(seen, [subprocess.DEVNULL])


class MountedSkillsTests(unittest.TestCase):
	"""Operator skills mounted at /inputs/skills reach every harness."""

	def setUp(self):
		tmp = tempfile.TemporaryDirectory()
		self.addCleanup(tmp.cleanup)
		self.tmp = Path(tmp.name)
		self.mount = self.tmp / "skills"
		self.session = self.tmp / "session"
		patch = mock.patch.object(harness_adapters, "SKILLS_MOUNT", self.mount)
		patch.start()
		self.addCleanup(patch.stop)

	def add_skill(self, name, body="body"):
		d = self.mount / name
		d.mkdir(parents=True, exist_ok=True)
		(d / "SKILL.md").write_text(f"---\nname: {name}\ndescription: d\n---\n{body}\n")
		return d

	def argv(self, name):
		with route("openai-responses" if name == "codex" else "openai-completions"):
			return harness_adapters.get(name).invocation(
				Path("/work"), prompt="p", session_dir=self.session, continue_session=False, thinking=None)

	def test_only_directories_with_a_skill_md_count_and_they_sort(self):
		self.add_skill("b")
		self.add_skill("a")
		(self.mount / "empty").mkdir()
		(self.mount / "file.txt").write_text("x")
		self.assertEqual([d.name for d in harness_adapters.mounted_skills()], ["a", "b"])

	def test_absent_or_empty_mount_adds_nothing(self):
		self.assertEqual(harness_adapters.mounted_skills(), [])
		for name in ("pi", "pifork"):
			self.assertNotIn("--skill", self.argv(name))
		self.mount.mkdir()
		for name in ("pi", "pifork", "codex", "copilot"):
			self.argv(name)
		self.assertFalse((self.session / "codex-home" / "skills").exists())
		self.assertFalse((self.session / "copilot-home" / "skills").exists())

	def test_pi_and_pifork_get_one_skill_flag_each_before_the_prompt(self):
		self.add_skill("b")
		self.add_skill("a")
		for name in ("pi", "pifork"):
			got = self.argv(name)
			self.assertEqual(got[-5:], ["--skill", str(self.mount / "a"), "--skill", str(self.mount / "b"), "p"])

	def test_pi_and_pifork_also_load_the_repos_agents_skills(self):
		self.add_skill("op")
		workspace = self.tmp / "ws"
		for name in ("repo-b", "repo-a"):
			d = workspace / ".agents" / "skills" / name
			d.mkdir(parents=True)
			(d / "SKILL.md").write_text(f"---\nname: {name}\ndescription: d\n---\n")
		(workspace / ".agents" / "skills" / "no-skill-md").mkdir()
		(workspace / ".github" / "skills" / "elsewhere").mkdir(parents=True)
		(workspace / ".github" / "skills" / "elsewhere" / "SKILL.md").write_text("x")
		for name in ("pi", "pifork"):
			with route("openai-completions"):
				got = harness_adapters.get(name).invocation(
					workspace, prompt="p", session_dir=self.session, continue_session=False, thinking=None)
			want = ["--skill", str(self.mount / "op"),
				"--skill", str(workspace / ".agents" / "skills" / "repo-a"),
				"--skill", str(workspace / ".agents" / "skills" / "repo-b"), "p"]
			self.assertEqual(got[-7:], want)

	def test_repo_skills_are_capped(self):
		workspace = self.tmp / "many"
		for i in range(harness_adapters.MAX_REPO_SKILLS + 5):
			d = workspace / ".agents" / "skills" / f"s{i:03d}"
			d.mkdir(parents=True)
			(d / "SKILL.md").write_text("x")
		got = harness_adapters.repo_skills(workspace)
		self.assertEqual(len(got), harness_adapters.MAX_REPO_SKILLS)
		self.assertEqual(got[0].name, "s000")

	def test_codex_and_copilot_copy_each_skill_into_their_home(self):
		self.add_skill("a", "from mount")
		for name, home in (("codex", "codex-home"), ("copilot", "copilot-home")):
			self.argv(name)
			self.assertIn("from mount", (self.session / home / "skills" / "a" / "SKILL.md").read_text())

	def test_next_round_replaces_an_edited_copy_and_leaves_other_entries(self):
		self.add_skill("a", "from mount")
		for name, home in (("codex", "codex-home"), ("copilot", "copilot-home")):
			self.argv(name)
			skills = self.session / home / "skills"
			(skills / ".system").mkdir(exist_ok=True)
			(skills / ".system" / "keep").write_text("k")
			(skills / "a" / "SKILL.md").write_text("edited")
			(skills / "a" / "stray").write_text("x")
			self.argv(name)
			self.assertIn("from mount", (skills / "a" / "SKILL.md").read_text())
			self.assertFalse((skills / "a" / "stray").exists())
			self.assertEqual((skills / ".system" / "keep").read_text(), "k")

	def test_next_round_replaces_a_planted_symlink_or_file(self):
		self.add_skill("a", "from mount")
		elsewhere = self.tmp / "elsewhere"
		elsewhere.mkdir()
		(elsewhere / "SKILL.md").write_text("planted")
		for name, home in (("codex", "codex-home"), ("copilot", "copilot-home")):
			skills = self.session / home / "skills"
			skills.mkdir(parents=True, exist_ok=True)
			(skills / "a").symlink_to(elsewhere, target_is_directory=True)
			self.argv(name)
			self.assertFalse((skills / "a").is_symlink())
			self.assertIn("from mount", (skills / "a" / "SKILL.md").read_text())
			self.assertEqual((elsewhere / "SKILL.md").read_text(), "planted")
			shutil.rmtree(skills / "a")
			(skills / "a").write_text("a file")
			self.argv(name)
			self.assertIn("from mount", (skills / "a" / "SKILL.md").read_text())

	def test_codex_turns_its_bundled_skills_off(self):
		self.assertIn("skills.bundled.enabled=false", harness_adapters.get("codex").fixed_config)


if __name__ == "__main__":
	unittest.main()
