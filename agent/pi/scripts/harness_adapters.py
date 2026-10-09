"""The one seam between buildgate's model-bound scripts and the coding agent
(the "harness") they drive.

Every script that launches the agent builds its argv with
`adapter.invocation(...)`, reads the agent's stdout through
`adapter.parse(...)`, and labels live progress with
`adapter.progress_note(...)`; none of them knows a harness's flags or wire
format. `--harness <name>` on each script selects the adapter via `get()`.

Four adapters exist: `pi`, `pifork` (a fork of Pi the operator builds into
their own worker image with `make pifork-image`, see
doc/designs/pifork-harness.md; same flags, same JSON event stream, the
image's `pifork` launcher and an agent directory that `PiforkAdapter.prepare`
seeds from the image), `codex` (Codex CLI, Responses
API only) and `copilot` (Copilot CLI in bring-your-own-key mode). A caller
runs `adapter.prepare()` once before its first `invocation`; it also renders
the adapter's own CLI config from the harness-neutral FACTORY_MODEL_* route
facts factoryd puts in a relay-routed worker's environment.

Codex and Copilot keep their per-session state (Codex's sqlite and sessions,
Copilot's session store) under the caller's `session_dir`, the way Pi's
`--session-dir` does, and get the relay's placeholder key and route through
an `env VAR=value ...` prefix on the argv, so `invocation` stays a plain
argv command line.
"""

from __future__ import annotations

import json
import os
import re
import shlex
import shutil
import uuid
from dataclasses import dataclass, field
from pathlib import Path


@dataclass
class AgentOutput:
	"""What one agent run's stdout said, in harness-neutral form."""

	final_text: str
	last_turn_error: str
	route_errors: list[str] = field(default_factory=list)
	turn_errors: tuple[int, int] = (0, 0)
	usage: dict | None = None
	traces: list[dict] = field(default_factory=list)


def model_route_errors(stdout: str) -> list[str]:
	"""Returns each distinct errorMessage pi reported on a message_end event
	in its JSON stdout, in order -- how a model-route failure (an upstream
	4xx/5xx, an unsupported model) surfaces, since pi itself can still exit
	0. Untrusted text: callers redact and bound it before printing."""
	errors: list[str] = []
	for line in stdout.splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		if not isinstance(event, dict):
			continue
		message = event.get("message") or {}
		error_message = message.get("errorMessage") if isinstance(message, dict) else None
		if event.get("type") == "message_end" and isinstance(error_message, str) and error_message and error_message not in errors:
			errors.append(error_message)
	return errors


def parse_pi_traces(output: str) -> list[dict]:
	traces = []
	for line in output.splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		entry = event.get("entry") or {}
		custom_type = entry.get("customType")
		if event.get("type") == "entry_appended" and custom_type in ("pi-harness-trace", "pi-stall-trace"):
			trace = dict(entry.get("data") or {})
			trace["customType"] = custom_type
			if custom_type == "pi-stall-trace":
				trace.setdefault("extension", "progress-stall-guard")
				trace.setdefault("event", "stall")
			traces.append(trace)
	return traces


def parse_usage(output: str) -> dict | None:
	"""Sum token usage across every assistant message a round's own
	agent_end event carries.

	Found live, 2026-09-09 (the notes-demo-ticket-013 doctor/pip hardening
	pass, closing CLAIMS.md's "usage is null on every real run" remaining
	gap): a real `pi --print --mode json` agent_end event has no
	top-level "usage" key at all -- a live capture shows one against a
	real ai-stack-local invocation with `"messages": [...]` instead,
	usage living per-message inside each assistant entry's own "usage"
	dict. `"usage" in event` could therefore never be true against a real
	invocation; this function has silently returned None on every real
	round on record since it was written, the exact class of bug
	agent_turn_errors' own doc comment already found and fixed for a
	different function against the same real wire shape.

	Summed across every assistant message with a usage dict, not just the
	last: a round can carry more than one assistant turn (tool calls
	interleaved with text) before this round's own agent_end event fires,
	and the round's real total consumption is what the caller (this
	round's own BUILD_EVIDENCE.json entry) needs, not just its final
	turn's. Only numeric top-level fields (input/output/cacheRead/
	cacheWrite/reasoning/totalTokens in a real capture) are summed; the
	nested "cost" sub-object is intentionally left out of the summed
	result rather than incorrectly flattened or overwritten -- it was
	all-zero in every real capture this fix was checked against (a local,
	uncosted model), and correctly summing a nested dict is more
	complexity than that field's own current usefulness here justifies.
	"""
	totals: dict[str, int | float] = {}
	for line in output.splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		if event.get("type") != "agent_end":
			continue
		for message in event.get("messages") or []:
			if message.get("role") != "assistant":
				continue
			usage = message.get("usage")
			if not isinstance(usage, dict):
				continue
			for key, value in usage.items():
				if isinstance(value, (int, float)) and not isinstance(value, bool):
					totals[key] = totals.get(key, 0) + value
	return totals or None


def agent_turn_errors(output: str) -> tuple[int, int]:
	"""Count assistant turns whose model call errored out (e.g. the local
	route being unreachable, or the local model's own context budget
	exhausted) vs. the total assistant turns in this round.

	`pi -p` exits 0 and `verify` legitimately fails in this case -- from
	round_blockers' other signals alone this is indistinguishable from the
	model actually trying and producing bad code, which silently burns
	real round budget against an outage instead of surfacing it distinctly
	(observed live: budget-pilot ticket 005, 2026-08-20).

	Found live, 2026-09-07 (notes-demo ticket 007): this function's own
	entry_appended-wrapped shape assumption never matches real `pi --print
	--mode json` stdout -- a live capture against an actually-installed pi
	binary shows every message-lifecycle event (message_start, message_end,
	turn_start, turn_end, agent_start, agent_end) emitted as its own flat
	top-level event, never wrapped in `{"type": "entry_appended", "entry":
	...}`. That wrapper shape is real, but only for the *custom* trace
	events pi-harness's own extensions emit (parse_pi_traces above, which
	does see them) -- not for native message events. The practical result:
	`total` was always 0 in production, `if total and errored == total`
	(round_blockers' own route-unreachable short-circuit) could never
	fire, and a real model-route outage -- including a local model's
	context budget getting exhausted mid-round, which then makes every
	`--continue` round after it error out immediately with zero tokens --
	silently counted as ordinary "no changes" rounds all the way to
	max_rounds instead of being surfaced distinctly, exactly the failure
	mode this function exists to catch. `message_end` (not `message_start`,
	which fires before `stopReason` is known, and not `turn_end`, which
	duplicates the same assistant message and would double-count) is the
	one event per real assistant turn this function now keys off."""
	total = 0
	errored = 0
	for line in output.splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		if event.get("type") != "message_end":
			continue
		message = event.get("message") or {}
		if message.get("role") != "assistant":
			continue
		total += 1
		if message.get("stopReason") == "error":
			errored += 1
	return errored, total


def final_assistant_text(output: str) -> str:
	"""Concatenates every text block from the LAST completed assistant
	turn in output (`pi --print --mode json`'s own per-turn event stream
	-- see agent_turn_errors' own doc comment for why message_end/role
	assistant is this wire shape's reliable per-turn event). Returns ""
	if no assistant turn ever completed."""
	text = ""
	for line in output.splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		if event.get("type") != "message_end":
			continue
		message = event.get("message") or {}
		if message.get("role") != "assistant":
			continue
		content = message.get("content")
		if isinstance(content, str):
			text = content
		elif isinstance(content, list):
			text = "".join(
				block.get("text", "") for block in content if isinstance(block, dict) and block.get("type") == "text"
			)
	return text


def _last_review_turn_error(output: str) -> str:
	"""Returns the last completed assistant turn's own errorMessage when
	that turn's stopReason is "error", else "" -- companion to
	final_assistant_text (same message_end/role assistant event; see that
	function's own doc comment for the wire shape), used by
	run_review_turn to explain an unavailable review turn instead of
	returning only "". Unlike model_route_errors (every distinct error
	anywhere in the output), this looks only at the LAST assistant turn,
	since that is the turn whose empty/missing text run_review_turn is
	actually reporting on -- an earlier, already-superseded turn's error
	is not why the final answer came back empty."""
	message = None
	for line in output.splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		if event.get("type") != "message_end":
			continue
		candidate = event.get("message") or {}
		if isinstance(candidate, dict) and candidate.get("role") == "assistant":
			message = candidate
	if message and message.get("stopReason") == "error":
		error = message.get("errorMessage")
		if isinstance(error, str):
			return error
	return ""


def _pi_event_note(event: dict) -> str | None:
	"""Classifies one `pi --print --mode json` event into a single short
	human-readable summary for a live FACTORY_PROGRESS "agent" note, or
	None if this event isn't a tool call or assistant text turn. Tool
	calls and assistant text both arrive as content blocks on a
	message_end event whose message role is "assistant" -- the same
	wire shape final_assistant_text() above already relies on (see its
	own doc comment for why message_end/role assistant is the reliable
	per-turn event). A content block is either a "tool_use"/"toolCall"
	block (a call, with a tool name and its arguments) or a "text" block
	(assistant prose) -- this only summarizes those two, never raw model
	output beyond one identifying line."""
	if event.get("type") != "message_end":
		return None
	message = event.get("message") or {}
	if message.get("role") != "assistant":
		return None
	content = message.get("content")
	if not isinstance(content, list):
		return None
	for block in content:
		if not isinstance(block, dict):
			continue
		block_type = block.get("type")
		if block_type in ("tool_use", "toolCall"):
			name = str(block.get("name") or block.get("toolName") or "").lower()
			args = block.get("input") or block.get("arguments") or {}
			if not isinstance(args, dict):
				args = {}
			if "bash" in name or "shell" in name:
				command = " ".join(str(args.get("command") or args.get("cmd") or "").split())
				return f"bash: {command[:120]}"
			path = str(args.get("path") or args.get("file_path") or args.get("filePath") or "")
			if "edit" in name:
				return f"edit: {path}"
			if "write" in name or "create" in name:
				return f"write: {path}"
			if "read" in name:
				return f"read: {path}"
			return None
		if block_type == "text":
			text = " ".join(str(block.get("text") or "").split())
			return f"said: {text[:160]}" if text else None
	return None


RELAY_PROVIDER_ID = "factoryd-relay"
# Satisfies pi's models.json schema, which requires a non-empty apiKey per
# provider. Not a credential: the relay strips whatever credential a worker
# request carries and injects the real one itself.
RELAY_API_KEY_PLACEHOLDER = "factoryd-relay-placeholder"
# Runs a command behind a loopback proxy that fills an OpenAI Responses
# stream's empty final output from the items the stream delivered.
RESPONSES_OUTPUT_PROXY = Path(__file__).resolve().parent / "fill_responses_output.mjs"
_RESERVED_MODEL_KEYS = ("id", "api", "baseUrl")
_ENV_NAME = re.compile(r"[A-Z][A-Z0-9_]*")
_HEADER_NAME = re.compile(r"[A-Za-z0-9-]+")


def credential_placeholders(env=None) -> tuple[str, dict[str, str]] | None:
	"""(key env name, {header: env name}) from FACTORY_MODEL_KEY_ENV and
	FACTORY_MODEL_HEADERS_JSON, or None when FACTORY_MODEL_KEY_ENV is unset
	(a relay-routed worker). Each names an environment variable that holds a
	credential placeholder, never a value: a supervisor swaps the placeholder
	for the real secret on the wire. A malformed value is an error, never a
	silent skip."""
	env = os.environ if env is None else env
	key_env = env.get("FACTORY_MODEL_KEY_ENV", "")
	headers_json = env.get("FACTORY_MODEL_HEADERS_JSON", "")
	if not key_env:
		if headers_json.strip():
			raise SystemExit("FACTORY_MODEL_HEADERS_JSON is set without FACTORY_MODEL_KEY_ENV")
		return None
	if not _ENV_NAME.fullmatch(key_env):
		raise SystemExit(f"FACTORY_MODEL_KEY_ENV {key_env!r} is not an environment variable name")
	headers = json.loads(headers_json) if headers_json.strip() else {}
	if not isinstance(headers, dict):
		raise SystemExit("FACTORY_MODEL_HEADERS_JSON must be a JSON object")
	for header, name in headers.items():
		if not _HEADER_NAME.fullmatch(header):
			raise SystemExit(f"FACTORY_MODEL_HEADERS_JSON header name {header!r} is not [A-Za-z0-9-]+")
		if not isinstance(name, str) or not _ENV_NAME.fullmatch(name):
			raise SystemExit(f"FACTORY_MODEL_HEADERS_JSON value for {header!r} is not an environment variable name")
	return key_env, headers


def pi_models_config(env=None) -> dict | None:
	"""The models.json value pi reads, rendered from the harness-neutral
	FACTORY_MODEL_* facts factoryd hands a relay-routed worker (see
	internal/sandbox/relay_lifecycle.go), or None when FACTORY_MODEL_ID is
	unset (an Anthropic-shaped route, or a host run).

	FACTORY_MODEL_EXTRA_JSON's keys add to the model entry, but id/api/baseUrl
	always win over it: the extra JSON is an operator input and must not
	redirect the worker off its run-scoped relay. Go validated it is a JSON
	object at launch; a violation here is an error, never a silent skip."""
	env = os.environ if env is None else env
	model_id = env.get("FACTORY_MODEL_ID")
	if not model_id:
		return None
	base_url = env["FACTORY_MODEL_BASE_URL"]
	model = {"id": model_id, "api": env["FACTORY_MODEL_API"], "baseUrl": base_url}
	extra_json = env.get("FACTORY_MODEL_EXTRA_JSON", "")
	if extra_json.strip():
		extra = json.loads(extra_json)
		if not isinstance(extra, dict):
			raise ValueError("FACTORY_MODEL_EXTRA_JSON must be a JSON object")
		for key, value in extra.items():
			if key not in _RESERVED_MODEL_KEYS:
				model[key] = value
	provider = {
		"name": RELAY_PROVIDER_ID,
		"baseUrl": base_url,
		"apiKey": RELAY_API_KEY_PLACEHOLDER,
		"models": [model],
	}
	placeholders = credential_placeholders(env)
	if placeholders is not None:
		key_env, headers = placeholders
		provider["apiKey"] = "$" + key_env
		if headers:
			provider["headers"] = {header: "$" + name for header, name in headers.items()}
	return {"providers": {RELAY_PROVIDER_ID: provider}}


# Operator skills the factory mounts read-only in the worker, one directory
# per skill holding a SKILL.md. Absent or empty means no skills.
SKILLS_MOUNT = Path("/inputs/skills")


def mounted_skills(root: Path | None = None) -> list[Path]:
	"""The skill directories under `root` (default SKILLS_MOUNT) that hold a
	SKILL.md, sorted by name. Empty when the root is absent or unreadable."""
	root = SKILLS_MOUNT if root is None else Path(root)
	try:
		return sorted(d for d in root.iterdir() if d.is_dir() and (d / "SKILL.md").is_file())
	except OSError:
		return []


# The target repo's own skills that pi and pifork load explicitly: pi treats
# the project as untrusted in print mode and skips its project skills, while
# codex and copilot read this same folder natively, in every role: an
# invocation's load_repo_skills cannot switch that off for them. Repo content, so no more trusted
# than any other file the agent reads; factoryd refuses a launch when one
# shares an operator skill's name.
REPO_SKILLS_DIR = Path(".agents") / "skills"
# The worker can add folders here between rounds; each becomes a --skill
# argument and system-prompt text, so only the first MAX_REPO_SKILLS load.
MAX_REPO_SKILLS = 64


def repo_skills(workspace: Path) -> list[Path]:
	"""The skill directories under `workspace`/.agents/skills that hold a
	SKILL.md, sorted by name, at most MAX_REPO_SKILLS. Empty when there are
	none."""
	return mounted_skills(Path(workspace) / REPO_SKILLS_DIR)[:MAX_REPO_SKILLS]


def sync_skills(skills_dir: Path, root: Path | None = None) -> None:
	"""Replaces `skills_dir/<name>` with a fresh copy of each mounted skill,
	for harnesses that read skills only from their home. Runs every round, so
	a worker's edit to its copy lasts at most one round. Touches only the
	mounted names: the harness keeps its own entries (codex's `.system`)."""
	skills = mounted_skills(root)
	if not skills:
		return
	skills_dir.mkdir(parents=True, exist_ok=True)
	for skill in skills:
		target = skills_dir / skill.name
		# The worker owns this directory between rounds: a file or symlink
		# planted at the name must not survive (rmtree refuses a symlink).
		if target.is_symlink() or target.is_file():
			target.unlink()
		elif target.exists():
			shutil.rmtree(target)
		shutil.copytree(skill, target)


class PiAdapter:
	"""`pi --print --mode json`."""

	name = "pi"
	binary = "pi"

	def agent_dir(self) -> Path:
		"""Where pi reads its state, models.json included."""
		configured = os.environ.get("PI_CODING_AGENT_DIR")
		return Path(configured) if configured else Path.home() / ".pi" / "agent"

	def prepare(self) -> None:
		"""Renders pi's models.json from the FACTORY_MODEL_* route facts and
		points --provider/--model at it. Does nothing without
		FACTORY_MODEL_ID."""
		config = pi_models_config()
		if config is None:
			return
		agent_dir = self.agent_dir()
		agent_dir.mkdir(parents=True, exist_ok=True)
		(agent_dir / "models.json").write_text(json.dumps(config, indent=2) + "\n")
		os.environ["PI_HARNESS_PROVIDER"] = RELAY_PROVIDER_ID
		os.environ["PI_HARNESS_MODEL"] = os.environ["FACTORY_MODEL_ID"]

	def invocation(
		self,
		workspace: Path,
		*,
		prompt: str,
		session_dir: Path,
		continue_session: bool,
		thinking: str | None,
		load_repo_skills: bool = False,
	) -> list[str]:
		args = [
			self.binary, "--print", "--mode", "json",
			"--session-dir", str(session_dir),
		]
		# Omit these flags by default so the installed settings.json policy (or a
		# `/model` selection made through it) applies. An explicit override remains
		# available via PI_HARNESS_PROVIDER/PI_HARNESS_MODEL/--thinking for
		# controlled experiments and reproductions.
		provider = os.environ.get("PI_HARNESS_PROVIDER")
		model = os.environ.get("PI_HARNESS_MODEL")
		if provider is not None:
			args += ["--provider", provider]
		if model is not None:
			args += ["--model", model]
		if thinking is not None:
			args += ["--thinking", thinking]
		if continue_session:
			args += ["--continue"]
		# The target repo's skills become system-prompt text, and the worker
		# can add one mid-run, so only the build turn asks for them. This
		# closes the skill path only: pi still loads the working tree's
		# AGENTS.md-style context files on every turn.
		skills = mounted_skills()
		if load_repo_skills:
			skills += repo_skills(workspace)
		for skill in skills:
			args += ["--skill", str(skill)]
		args += [prompt]
		return args

	def can_continue_session(self, session_dir: Path) -> bool:
		"""Whether invocation(continue_session=True) continues the session a
		round started, rather than opening a new one. Pi's --continue picks the
		latest session in the folder, which the rounds of this process made."""
		return True

	def parse(self, stdout: str) -> AgentOutput:
		return AgentOutput(
			final_text=final_assistant_text(stdout),
			last_turn_error=_last_review_turn_error(stdout),
			route_errors=model_route_errors(stdout),
			turn_errors=agent_turn_errors(stdout),
			usage=parse_usage(stdout),
			traces=parse_pi_traces(stdout),
		)

	def progress_note(self, line: str) -> str | None:
		"""One streamed stdout line as a short live-progress note, or None.
		Never raises: worker output is untrusted and this is display-only."""
		try:
			event = json.loads(line)
			return _pi_event_note(event) if isinstance(event, dict) else None
		except Exception:
			return None

	def line_event(self, line: str) -> dict | None:
		"""One streamed stdout line as the harness-neutral facts a bounded
		streaming digest needs (see draft_acceptance_oracles.AgentDigest):
		`stream_reset`/`stream_delta` (text being streamed), `turn` (an
		assistant turn ended), `failed`, `error`, `text` (the turn's full
		text; "" clears it) and `usage`. None for a line that carries none."""
		try:
			event = json.loads(line)
		except (json.JSONDecodeError, RecursionError):
			return None
		if not isinstance(event, dict):
			return None
		kind = event.get("type")
		if kind == "message_start":
			return {"stream_reset": True}
		if kind == "message_update":
			delta = event.get("assistantMessageEvent")
			if isinstance(delta, dict) and isinstance(delta.get("delta"), str):
				return {"stream_delta": delta["delta"]}
			return None
		if kind == "agent_end":
			return {"usage": parse_usage(line)}
		if kind != "message_end":
			return None
		message = event.get("message")
		if not isinstance(message, dict) or message.get("role") != "assistant":
			return None
		out: dict = {"turn": True}
		if message.get("stopReason") == "error":
			out["failed"] = True
			out["error"] = str(message.get("errorMessage") or "stopReason=error")
		content = message.get("content")
		if isinstance(content, list):
			texts = [c.get("text") for c in content if isinstance(c, dict) and c.get("type") == "text" and isinstance(c.get("text"), str)]
			if texts:
				out["text"] = "\n".join(texts)
		elif isinstance(content, str):
			out["text"] = content
		return out


class PiforkAdapter(PiAdapter):
	"""A fork of Pi packaged into the operator's own worker image: the same
	flags and event stream under the image's `pifork` launcher, plus the
	agent directory seeded from the image. See doc/designs/pifork-harness.md
	for what the image must provide."""

	name = "pifork"
	binary = "pifork"

	def prepare(self) -> None:
		# The worker environment sets this too; the default keeps a direct
		# invocation's state out of a developer's home directory.
		os.environ.setdefault("PI_CODING_AGENT_DIR", "/tmp/factoryd-pi-agent")
		agent_dir = Path(os.environ["PI_CODING_AGENT_DIR"])
		agent_seed = Path(os.environ.get("PIFORK_AGENT_SEED", "/opt/pifork-agent-seed"))
		agent_dir.mkdir(parents=True, exist_ok=True)
		if agent_seed.is_dir():
			for source in agent_seed.iterdir():
				target = agent_dir / source.name
				if source.name == "npm":
					if not target.exists():
						target.symlink_to(source, target_is_directory=True)
				elif source.is_dir():
					shutil.copytree(source, target, dirs_exist_ok=True)
				elif not target.exists():
					shutil.copy2(source, target)
		# A fork may take a missing auth.json for a first run and start its
		# login flow. The sandbox's supervisor owns the real credential, so the
		# worker gets an intentionally empty file.
		auth_file = agent_dir / "auth.json"
		if not auth_file.exists():
			auth_file.write_text("{}\n")
		# The route goes last, into the same agent directory.
		super().prepare()


def _jsonl_events(stdout: str):
	"""Every JSON-object line of stdout, in order; anything else is skipped."""
	for line in (stdout or "").splitlines():
		try:
			event = json.loads(line)
		except json.JSONDecodeError:
			continue
		if isinstance(event, dict):
			yield event


def _relay_route(harness: str, env=None) -> tuple[str, str, str]:
	"""(model id, base URL, api) of this run's relay route from the
	FACTORY_MODEL_* facts, or a SystemExit naming what is missing: Codex and
	Copilot cannot pick a model or endpoint on their own, and must never fall
	back to a vendor default."""
	env = os.environ if env is None else env
	missing = [name for name in ("FACTORY_MODEL_ID", "FACTORY_MODEL_BASE_URL") if not env.get(name)]
	if missing:
		raise SystemExit(
			f"harness {harness}: {' and '.join(missing)} not set; {harness} runs only against the run's relay "
			"route, which factoryd hands the worker through FACTORY_MODEL_* (a role that names this harness "
			"needs a relay-routed model)"
		)
	return env["FACTORY_MODEL_ID"], env["FACTORY_MODEL_BASE_URL"], env.get("FACTORY_MODEL_API", "")


def _effort(thinking: str | None) -> str | None:
	"""Pi's thinking level as the reasoning-effort word Codex and Copilot take:
	`off` is `none`; every other level passes through unchanged."""
	if thinking is None:
		return None
	return "none" if thinking == "off" else thinking


def _shell_display(command: str) -> str:
	"""A command as one line for a progress note, without the `bash -lc '...'`
	wrapper Codex reports around every shell command."""
	try:
		parts = shlex.split(command)
	except ValueError:
		parts = []
	if len(parts) == 3 and parts[1] in ("-c", "-lc") and os.path.basename(parts[0]) in ("bash", "sh", "zsh"):
		command = parts[2]
	return " ".join(command.split())


def _patch_note(patch: str) -> str | None:
	"""`write: <path>` / `edit: <path>` for the first file an apply_patch body
	touches."""
	match = re.search(r"\*\*\* (Add|Update|Delete) File:[ \t]*(\S[^\n]*)", patch)
	if match is None:
		return None
	verb = "write" if match.group(1) == "Add" else "edit"
	return f"{verb}: {match.group(2).strip()}"


def _said_note(text: object) -> str | None:
	text = " ".join(str(text or "").split())
	return f"said: {text[:160]}" if text else None


def _codex_usage(usage: object) -> dict[str, int]:
	"""Codex's turn.completed usage in the keys PiAdapter.parse returns. Codex
	counts cached prompt tokens inside input_tokens (and reasoning inside
	output_tokens); Pi's `input` excludes cache reads and consumers add
	`cacheRead`/`cacheWrite` back (cmd/factoryd/round_summary.go), so `input`
	is input_tokens minus the cached part and `totalTokens` is the sum of the
	four. Reasoning tokens are reported separately, not added again."""
	if not isinstance(usage, dict):
		return {}

	def count(key: str) -> int:
		value = usage.get(key)
		return value if isinstance(value, int) and not isinstance(value, bool) else 0

	cached = count("cached_input_tokens")
	written = count("cache_write_input_tokens")
	fresh = max(count("input_tokens") - cached, 0)
	output = count("output_tokens")
	return {
		"input": fresh,
		"output": output,
		"cacheRead": cached,
		"cacheWrite": written,
		"reasoning": count("reasoning_output_tokens"),
		"totalTokens": fresh + cached + written + output,
	}


class CodexAdapter:
	"""`codex exec --json` against the run's relay, over the Responses API."""

	name = "codex"
	binary = "codex"
	# Environment variable Codex reads its provider key from; it carries the
	# placeholder, and the relay strips whatever credential a request has.
	key_env = "FACTORYD_RELAY_KEY"
	# Codex's own switches for everything that would reach a vendor service
	# or fetch code: the curated-plugin sync (a clone into $CODEX_HOME/.tmp),
	# connector apps, product analytics, feedback upload, the update check
	# and hosted web search. Found in the 0.154.0 binary's config keys and
	# `codex features list`; with them set a fresh CODEX_HOME stays free of a
	# plugin clone.
	fixed_config = (
		# A dead relay fails the turn after 5 retries (an all-errored round the
		# loop stops on) instead of retrying until the round timeout.
		"features.unbounded_connection_retries=false",
		"features.plugins=false",
		"features.apps=false",
		"analytics.enabled=false",
		"feedback.enabled=false",
		"check_for_update_on_startup=false",
		'web_search="disabled"',
		# Codex's five bundled system skills (imagegen, openai-docs,
		# plugin-creator, skill-creator, skill-installer): the worker gets
		# only the operator's skills. Key from the 0.154.0 binary's
		# BundledSkillsConfig (`skills.bundled.enabled`).
		"skills.bundled.enabled=false",
	)

	def prepare(self) -> None:
		_, _, api = _relay_route(self.name)
		if api != "openai-responses":
			raise SystemExit(
				f"harness codex speaks only the Responses API, but this run's model route is {api or 'unspecified'!r}; "
				"give the role a model on an openai-responses route or another harness"
			)

	def invocation(
		self,
		workspace: Path,
		*,
		prompt: str,
		session_dir: Path,
		continue_session: bool,
		thinking: str | None,
		load_repo_skills: bool = False,
	) -> list[str]:
		model_id, base_url, _ = _relay_route(self.name)
		codex_home = Path(session_dir) / "codex-home"
		codex_home.mkdir(parents=True, exist_ok=True)
		sync_skills(codex_home / "skills")
		placeholders = credential_placeholders()
		if placeholders is None:
			key_env, headers = self.key_env, {}
			env_assignments = [f"{self.key_env}={RELAY_API_KEY_PLACEHOLDER}"]
		else:
			(key_env, headers), env_assignments = placeholders, []
		env_http_headers = ""
		if headers:
			table = ",".join(f"{json.dumps(header)}={json.dumps(name)}" for header, name in headers.items())
			env_http_headers = f",env_http_headers={{{table}}}"
		provider = (
			f'{{name="relay",base_url={json.dumps(base_url)},wire_api="responses",'
			f'env_key={json.dumps(key_env)}{env_http_headers}}}'
		)
		args = [
			"env", f"CODEX_HOME={codex_home}", *env_assignments,
			self.binary, "exec",
		]
		if continue_session:
			args += ["resume", "--last"]
		args += [
			"--json", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
			"-m", model_id,
			"-c", "model_provider=relay",
			"-c", f"model_providers.relay={provider}",
		]
		for setting in self.fixed_config:
			args += ["-c", setting]
		effort = _effort(thinking)
		if effort is not None:
			args += ["-c", f"model_reasoning_effort={json.dumps(effort)}"]
		args += [prompt]
		return args

	def can_continue_session(self, session_dir: Path) -> bool:
		"""`resume --last` in this session's own codex home continues the
		round's session."""
		return True

	def parse(self, stdout: str) -> AgentOutput:
		final_text = ""
		last_turn_error = ""
		route_errors: list[str] = []
		started = failed = 0
		usage: dict[str, int] = {}
		for event in _jsonl_events(stdout):
			kind = event.get("type")
			if kind == "turn.started":
				started += 1
			elif kind == "turn.completed":
				last_turn_error = ""
				for key, value in _codex_usage(event.get("usage")).items():
					usage[key] = usage.get(key, 0) + value
			elif kind in ("error", "turn.failed"):
				message = event.get("message") if kind == "error" else (event.get("error") or {}).get("message")
				if isinstance(message, str) and message:
					if message not in route_errors:
						route_errors.append(message)
					if kind == "turn.failed":
						last_turn_error = message
				if kind == "turn.failed":
					failed += 1
					final_text = ""  # a failed final turn has no answer, as in Pi
			elif kind == "item.completed":
				item = event.get("item") or {}
				if isinstance(item, dict) and item.get("type") == "agent_message" and isinstance(item.get("text"), str):
					final_text = item["text"]
		total = max(started, failed)
		if total == 0 and route_errors:
			# The run died before any turn began: nothing succeeded.
			total = failed = 1
		return AgentOutput(
			final_text=final_text,
			last_turn_error=last_turn_error,
			route_errors=route_errors,
			turn_errors=(failed, total),
			usage=usage or None,
		)

	def line_event(self, line: str) -> dict | None:
		try:
			event = json.loads(line)
		except (json.JSONDecodeError, RecursionError):
			return None
		if not isinstance(event, dict):
			return None
		kind = event.get("type")
		if kind == "turn.completed":
			out: dict = {"turn": True}
			usage = _codex_usage(event.get("usage"))
			if usage:
				out["usage"] = usage
			return out
		if kind in ("error", "turn.failed"):
			message = event.get("message") if kind == "error" else (event.get("error") or {}).get("message")
			out = {"turn": True, "failed": True, "text": ""} if kind == "turn.failed" else {}
			if isinstance(message, str) and message:
				out["error"] = message
			return out or None
		item = event.get("item")
		if kind == "item.completed" and isinstance(item, dict) and item.get("type") == "agent_message" and isinstance(item.get("text"), str):
			return {"text": item["text"]}
		return None

	def progress_note(self, line: str) -> str | None:
		try:
			event = json.loads(line)
			if not isinstance(event, dict):
				return None
			item = event.get("item") or {}
			if not isinstance(item, dict):
				return None
			kind, item_type = event.get("type"), item.get("type")
			if kind == "item.started" and item_type == "command_execution":
				return f"bash: {_shell_display(str(item.get('command') or ''))[:120]}"
			if kind == "item.started" and item_type == "file_change":
				for change in item.get("changes") or []:
					if isinstance(change, dict) and change.get("path"):
						verb = "write" if change.get("kind") == "add" else "edit"
						return f"{verb}: {change['path']}"
				return None
			if kind == "item.completed" and item_type == "agent_message":
				return _said_note(item.get("text"))
			return None
		except Exception:
			return None


class CopilotAdapter:
	"""`copilot -p --output-format json` in bring-your-own-key mode against
	the run's relay: no GitHub login, offline mode so it neither
	authenticates nor reports telemetry, and no self-update."""

	name = "copilot"
	binary = "copilot"
	# Holds the id of the session the first round started, so a continued
	# round resumes it (Pi's --continue equivalent).
	session_id_file = "copilot-session-id"

	def prepare(self) -> None:
		_relay_route(self.name)

	def _session_id(self, session_dir: Path, continue_session: bool) -> tuple[str, bool]:
		"""(session id, resuming). A continued round resumes the id the first
		round stored; without a readable one it starts a fresh session."""
		id_file = session_dir / self.session_id_file
		if continue_session:
			try:
				stored = id_file.read_text().strip()
			except OSError:
				stored = ""
			if stored:
				return stored, True
		session_id = str(uuid.uuid4())
		id_file.write_text(session_id + "\n")
		return session_id, False

	def invocation(
		self,
		workspace: Path,
		*,
		prompt: str,
		session_dir: Path,
		continue_session: bool,
		thinking: str | None,
		load_repo_skills: bool = False,
	) -> list[str]:
		model_id, base_url, api = _relay_route(self.name)
		api_key = RELAY_API_KEY_PLACEHOLDER
		provider_headers: list[str] = []
		placeholders = credential_placeholders()
		if placeholders is not None:
			# The CLI sends its provider key as the bearer token and each
			# further credential header as written; all are the route's
			# placeholders, which the supervisor swaps on the wire.
			key_env, headers = placeholders
			api_key = os.environ.get(key_env, "")
			if not api_key:
				raise SystemExit(f"harness copilot: {key_env} (named by FACTORY_MODEL_KEY_ENV) is not set")
			for header, name in sorted(headers.items()):
				value = os.environ.get(name, "")
				if not value:
					raise SystemExit(f"harness copilot: {name} (named by FACTORY_MODEL_HEADERS_JSON for {header}) is not set")
				provider_headers.append(f"{header}: {value}")
		session_dir = Path(session_dir)
		session_dir.mkdir(parents=True, exist_ok=True)
		if api == "anthropic-messages":
			provider = ["COPILOT_PROVIDER_TYPE=anthropic"]
		else:
			provider = [
				"COPILOT_PROVIDER_TYPE=openai",
				"COPILOT_PROVIDER_WIRE_API=" + ("responses" if api == "openai-responses" else "completions"),
			]
		session_id, resuming = self._session_id(session_dir, continue_session)
		sync_skills(session_dir / "copilot-home" / "skills")
		# Never COPILOT_ALLOW_ALL=true: that exact value also trusts the
		# repository's own hooks, plugins and MCP servers, and the workspace
		# is untrusted input. --allow-all-tools is only the tool approval.
		args = [
			"env",
			f"COPILOT_HOME={session_dir / 'copilot-home'}",
			f"COPILOT_PROVIDER_BASE_URL={base_url}",
			*provider,
			f"COPILOT_PROVIDER_API_KEY={api_key}",
			f"COPILOT_MODEL={model_id}",
			"COPILOT_OFFLINE=true",
			"COPILOT_AUTO_UPDATE=false",
		]
		if provider_headers:
			# Only a chatgpt-codex route has a further credential header, and
			# its backend leaves the final response event's output empty,
			# which is where the CLI reads a turn from: run the CLI behind
			# the proxy that fills it in (see RESPONSES_OUTPUT_PROXY).
			args += [
				"COPILOT_PROVIDER_HEADERS=" + "\n".join(provider_headers),
				"node", str(RESPONSES_OUTPUT_PROXY), base_url, "COPILOT_PROVIDER_BASE_URL", "--",
			]
		args += [
			self.binary,
			"--allow-all-tools", "--no-auto-update", "--output-format", "json",
			"--resume" if resuming else "--session-id", session_id,
		]
		effort = _effort(thinking)
		if effort is not None:
			args += ["--reasoning-effort", effort]
		return args + ["-p", prompt]

	def can_continue_session(self, session_dir: Path) -> bool:
		"""Only with the id the first round stored: without it a continued
		invocation starts a fresh session that knows nothing of the build."""
		try:
			return bool((Path(session_dir) / self.session_id_file).read_text().strip())
		except OSError:
			return False

	def parse(self, stdout: str) -> AgentOutput:
		final_text = ""
		last_error = ""
		route_errors: list[str] = []
		turns: set[str] = set()
		errored: set[str] = set()
		session_failed = False
		for event in _jsonl_events(stdout):
			kind = event.get("type")
			data = event.get("data") if isinstance(event.get("data"), dict) else {}
			if kind == "assistant.turn_start":
				turns.add(str(data.get("turnId")))
			elif kind == "assistant.message":
				content = data.get("content")
				if isinstance(content, str) and content.strip():
					final_text = content
			elif kind == "model.call_finished":
				turn = str(data.get("turnId"))
				if data.get("outcome") == "success":
					last_error = ""
					errored.discard(turn)
				else:
					errored.add(turn)
					final_text = ""  # a failed final turn has no answer, as in Pi
			elif kind in ("model.call_failure", "session.error"):
				message = data.get("errorMessage") if kind == "model.call_failure" else data.get("message")
				if isinstance(message, str):
					# Copilot JSON-encodes the failure text once more.
					message = message.strip().strip('"')
					if message:
						last_error = message
						if message not in route_errors:
							route_errors.append(message)
				if kind == "session.error":
					session_failed = True
		total = len(turns | errored)
		failed = len(errored)
		if session_failed and total == 0:
			# The session died before any model turn ran: nothing succeeded.
			total = failed = 1
		return AgentOutput(
			final_text=final_text,
			last_turn_error=last_error,
			route_errors=route_errors,
			turn_errors=(failed, total),
			usage=None,  # the CLI reports no token counts; the relay ledger is authoritative
		)

	def line_event(self, line: str) -> dict | None:
		try:
			event = json.loads(line)
		except (json.JSONDecodeError, RecursionError):
			return None
		if not isinstance(event, dict):
			return None
		kind = event.get("type")
		data = event.get("data") if isinstance(event.get("data"), dict) else {}
		if kind == "assistant.message":
			content = data.get("content")
			return {"text": content} if isinstance(content, str) and content.strip() else None
		if kind == "model.call_finished":
			if data.get("outcome") == "success":
				return {"turn": True}
			return {"turn": True, "failed": True, "text": ""}
		if kind in ("model.call_failure", "session.error"):
			message = data.get("errorMessage") if kind == "model.call_failure" else data.get("message")
			if isinstance(message, str) and message.strip().strip('"'):
				return {"error": message.strip().strip('"')}
		return None

	def progress_note(self, line: str) -> str | None:
		try:
			event = json.loads(line)
			if not isinstance(event, dict):
				return None
			data = event.get("data") if isinstance(event.get("data"), dict) else {}
			kind = event.get("type")
			if kind == "assistant.message":
				return _said_note(data.get("content"))
			if kind != "tool.execution_start":
				return None
			tool = str(data.get("toolName") or "").lower()
			args = data.get("arguments")
			if not isinstance(args, dict):
				args = {"input": args} if isinstance(args, str) else {}
			path = str(args.get("path") or args.get("file_path") or args.get("filePath") or "")
			if tool == "bash":
				return f"bash: {' '.join(str(args.get('command') or '').split())[:120]}"
			if tool == "apply_patch":
				for value in args.values():
					if isinstance(value, str) and (note := _patch_note(value)):
						return note
				return None
			if tool == "edit":
				return f"edit: {path}"
			if tool == "create":
				return f"write: {path}"
			if tool == "view":
				return f"read: {path}"
			return None
		except Exception:
			return None


ADAPTERS = {
	"pi": PiAdapter(),
	"pifork": PiforkAdapter(),
	"codex": CodexAdapter(),
	"copilot": CopilotAdapter(),
}


def get(name: str):
	try:
		return ADAPTERS[name]
	except KeyError:
		raise ValueError(f"unknown harness {name!r}; choose one of {', '.join(sorted(ADAPTERS))}") from None
