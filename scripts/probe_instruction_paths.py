#!/usr/bin/env python3
"""probe-instruction-paths: which repository paths does each pinned
coding-agent harness really load from a workspace?

internal/sandbox/review_instructions.go keeps a table of "instruction paths"
(reviewInstructionDirs, reviewInstructionFiles, reviewInstructionBaseNames)
that a review masks in the worktree. The table must be a superset of what the
harnesses load. This probe measures that instead of trusting documentation.

It builds a scratch git repository under ~/buildgate/instruction-probe/ (the
only directory the Docker VM shares read-write) holding one committed marker
per candidate path: every entry of the Go table, in the file shapes a harness
reads, plus candidates outside the table found in the harnesses' own shipped
code. Each marker carries a unique token. Text markers hold the token where
the harness loads it eagerly; anything a harness executes (a hook, an MCP
server, an extension) creates /probe-out/executed-<slug> instead.

It then runs ONE container per harness and turn kind from the worker image
(`--network none`, no credentials) and launches the harness exactly as a
buildgate turn does, through harness_adapters (`prepare()`, `invocation()`),
in /workspace. A fake model endpoint on the container's loopback logs every
request. Per candidate the result is `request` (token in a logged request
body), `executed` (side-effect file exists) or `not_observed`.

Turn kinds: `review` (load_repo_skills=False), `build` (load_repo_skills=True)
and `lazy` (a review turn whose fake reply makes one tool call that reads a
file under pkg/, so a harness that loads nested instruction files on first
read gets its chance).

    python3 scripts/probe_instruction_paths.py            # run the probe
    python3 scripts/probe_instruction_paths.py --list     # candidates, no Docker
    --image REF       worker image (default: sandbox_image in
                      ~/.config/factoryd/config.yml)
    --pifork-image REF  image that carries the `pifork` launcher (without it
                      pifork is not probed)
    --harness NAME    probe only this harness (repeatable)
    --write-fixture PATH  rewrite agent/pi/tests/fixtures/instruction_paths.json
                      from the result (a harness not probed keeps its entry)
    --from-result PATH  with --write-fixture: run nothing, and rebuild the
                      fixture from a saved result judged against the table as
                      it is now (after a table or coverage-rule change)

The result lands in ~/buildgate/instruction-probe/result.json. No model is
called and nothing leaves the container.
"""

import argparse
import copy
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parent.parent
GO_TABLE = ROOT / "internal" / "sandbox" / "review_instructions.go"
SCRATCH = pathlib.Path("~/buildgate/instruction-probe").expanduser()
HARNESSES = ("pi", "pifork", "codex", "copilot")
TURNS = ("review", "build", "lazy", "control")
# The turn kinds a table path is judged on: what a buildgate turn really gets.
REAL_TURNS = ("review", "build", "lazy")
APIS = {"pi": "openai-completions", "pifork": "openai-completions", "codex": "openai-responses", "copilot": "openai-completions"}
TIMEOUT_S = 240
LAZY_FILE = "pkg/lazy.txt"


# ---------------------------------------------------------------------------
# The Go table

def parse_table(source):
	"""{"dirs": [...], "files": [...], "names": [...]} read out of the Go
	file's three string-slice variables."""
	out = {}
	for key, var in (("dirs", "reviewInstructionDirs"), ("files", "reviewInstructionFiles"), ("names", "reviewInstructionBaseNames")):
		m = re.search(r"var\s+" + var + r"\s*=\s*\[\]string\{([^}]*)\}", source)
		if not m:
			raise SystemExit(f"{var} not found in review_instructions.go")
		out[key] = re.findall(r'"([^"]+)"', m.group(1))
	return out


def covered_by(table, path):
	"""The table entry that covers a repository-relative path, or None. Every
	entry matches in any directory, as the Go table does: a dir entry covers
	itself and everything below it wherever its components appear in the path,
	a file entry a path that ends in it, a base name that file name. Of several
	entries that cover one path, the first in the table's order is the answer
	(.claude/skills before .claude, so a skill is counted as a skill)."""
	parts = path.split("/")
	for d in table["dirs"]:
		want = d.split("/")
		if any(parts[i:i + len(want)] == want for i in range(len(parts))):
			return d
	for f in table["files"]:
		want = f.split("/")
		if parts[-len(want):] == want:
			return f
	if parts[-1] in table["names"]:
		return parts[-1]
	return None


def recover_candidates(result, table):
	"""The candidates of a saved result, each judged against the table as it
	is now: the observations stay, what counts as covered follows the table."""
	for c in result["candidates"]:
		c["table_path"] = covered_by(table, c["path"])
	return result


# ---------------------------------------------------------------------------
# Candidates

def token(slug):
	return "PROBE-7f3a-" + slug


def slug_of(path):
	return re.sub(r"[^A-Za-z0-9]+", "-", path).strip("-")


def md(slug):
	return f"# Probe note\n\nWhen asked, repeat the marker {token(slug)}.\n"


def skill(slug):
	name = "probe-" + slug.lower()[:40].strip("-")
	name = re.sub(r"[^a-z0-9-]+", "-", name)
	return f"---\nname: {name}\ndescription: Marker skill {token(slug)}. Use when asked about the probe.\n---\n\nSkill body for {token(slug)}.\n"


def touch_cmd(slug):
	return f"touch /probe-out/executed-{slug}"


def claude_settings(slug):
	return json.dumps({"hooks": {
		"SessionStart": [{"hooks": [{"type": "command", "command": touch_cmd(slug)}]}],
		"UserPromptSubmit": [{"hooks": [{"type": "command", "command": touch_cmd(slug)}]}],
	}}, indent=1) + "\n"


def github_hooks(slug):
	hook = {"type": "command", "bash": touch_cmd(slug), "timeoutSec": 5}
	return json.dumps({"version": 1, "hooks": {"sessionStart": [hook], "userPromptSubmitted": [hook]}}, indent=1) + "\n"


def codex_hooks(slug):
	hook = {"hooks": [{"type": "command", "command": touch_cmd(slug)}]}
	return json.dumps({"hooks": {"SessionStart": [hook], "UserPromptSubmit": [hook]}}, indent=1) + "\n"


def mcp_json(slug, key="mcpServers"):
	return json.dumps({key: {"probe-" + slug[:30]: {"type": "stdio", "command": "sh", "args": ["-c", touch_cmd(slug) + "; cat"]}}}, indent=1) + "\n"


def mcp_json_vscode(slug):
	return mcp_json(slug, "servers")


def codex_config(slug):
	return (f'developer_instructions = "{token(slug)}"\n'
		f'[mcp_servers.probe]\ncommand = "sh"\nargs = ["-c", "{touch_cmd(slug)}; cat"]\n')


def codex_agent(slug):
	return f'name = "probe"\ndescription = "{token(slug)}"\ndeveloper_instructions = "{token(slug)}"\n'


def pi_settings(slug):
	return json.dumps({"skills": ["./settings-skills"], "extensions": ["./settings-ext.mjs"]}, indent=1) + "\n"


def pi_extension(slug):
	return f'import {{ writeFileSync }} from "node:fs";\nwriteFileSync("/probe-out/executed-{slug}", "x");\nexport default function () {{}}\n'


def github_extension(slug):
	return pi_extension(slug)


def agent_md(slug):
	return f"---\nname: probe\ndescription: Agent marker {token(slug)}\n---\n\nAgent body {token(slug)}.\n"


def instructions_md(slug):
	return f'---\napplyTo: "**"\n---\n\nRule {token(slug)}.\n'


def command_md(slug):
	return f"---\ndescription: Command marker {token(slug)}\n---\n\nCommand body {token(slug)}.\n"


def cursor_mdc(slug):
	return f"---\ndescription: Rule {token(slug)}\nalwaysApply: true\n---\n\nRule {token(slug)}.\n"


def plugin_json(slug):
	return json.dumps({"name": "probe", "description": token(slug), "version": "0.0.1"}, indent=1) + "\n"


def github_settings(slug):
	return json.dumps({"model": token(slug)}, indent=1) + "\n"


def lsp_json(slug):
	return json.dumps({"lspServers": {"probe": {"command": "sh", "args": ["-c", touch_cmd(slug) + "; cat"], "fileExtensions": {".txt": "text"}}}}, indent=1) + "\n"


# (path, shape function, kind). kind: text = token in a request; exec = side effect.
SPECIFIC = [
	# Table paths, in the shapes the harness documentation names.
	("AGENTS.md", md, "text"), ("AGENTS.override.md", md, "text"), ("CLAUDE.md", md, "text"),
	("CLAUDE.local.md", md, "text"), ("GEMINI.md", md, "text"), ("pkg/AGENTS.md", md, "text"),
	("pkg/CLAUDE.md", md, "text"),
	(".agents/skills/probe/SKILL.md", skill, "text"), (".github/skills/probe/SKILL.md", skill, "text"),
	(".claude/skills/probe/SKILL.md", skill, "text"), (".pi/skills/probe/SKILL.md", skill, "text"),
	(".codex/skills/probe/SKILL.md", skill, "text"),
	(".pi/SYSTEM.md", md, "text"), (".pi/APPEND_SYSTEM.md", md, "text"),
	(".pi/settings.json", pi_settings, "text"), (".pi/settings-skills/probe/SKILL.md", skill, "text"),
	(".pi/settings-ext.mjs", pi_extension, "exec"), (".pi/extensions/probe.ts", pi_extension, "exec"),
	(".pi/prompts/probe.md", command_md, "text"),
	(".codex/config.toml", codex_config, "exec"), (".codex/hooks.json", codex_hooks, "exec"),
	(".codex/agents/probe.toml", codex_agent, "text"), (".codex/AGENTS.md", md, "text"),
	(".codex/prompts/probe.md", command_md, "text"),
	(".claude/settings.json", claude_settings, "exec"), (".claude/settings.local.json", claude_settings, "exec"),
	(".claude/agents/probe.md", agent_md, "text"),
	(".claude/commands/probe.md", command_md, "text"), (".claude/CLAUDE.md", md, "text"),
	(".github/copilot-instructions.md", md, "text"), (".github/instructions/probe.instructions.md", instructions_md, "text"),
	(".github/agents/probe.agent.md", agent_md, "text"), (".github/hooks/probe.json", github_hooks, "exec"),
	(".mcp.json", mcp_json, "exec"), (".vscode/mcp.json", mcp_json_vscode, "exec"),
	# Outside the table: shipped by a harness, or conventions of other agents.
	(".github/mcp.json", mcp_json, "exec"), (".github/lsp.json", lsp_json, "exec"),
	(".github/extensions/probe/extension.mjs", github_extension, "exec"),
	(".github/copilot/settings.json", github_settings, "text"), (".github/copilot/settings.local.json", github_settings, "text"),
	(".github/prompts/probe.prompt.md", command_md, "text"), (".github/chatmodes/probe.chatmode.md", agent_md, "text"),
	(".github/AGENTS.md", md, "text"),
	(".copilot/copilot-instructions.md", md, "text"), (".copilot/instructions/probe.instructions.md", instructions_md, "text"),
	(".lsp.json", lsp_json, "exec"), (".claude-plugin/plugin.json", plugin_json, "text"),
	(".codex-plugin/plugin.json", plugin_json, "text"), (".agents/plugins/marketplace.json", plugin_json, "text"),
	(".agents/AGENTS.md", md, "text"), (".agents/commands/probe.md", command_md, "text"),
	(".agents/agents/probe.md", agent_md, "text"), (".agents/rules/probe.md", md, "text"),
	(".agent/rules/probe.md", md, "text"), (".gemini/GEMINI.md", md, "text"), (".gemini/settings.json", plugin_json, "text"),
	(".cursorrules", md, "text"), (".cursor/rules/probe.mdc", cursor_mdc, "text"), (".windsurfrules", md, "text"),
	(".clinerules", md, "text"), (".opencode/agent/probe.md", agent_md, "text"), (".opencode/AGENTS.md", md, "text"),
	("QWEN.md", md, "text"), ("CONVENTIONS.md", md, "text"),
	# Table entries below a directory: the table matches them at any depth.
	("pkg/GEMINI.md", md, "text"),
	("pkg/.github/copilot-instructions.md", md, "text"), ("pkg/.mcp.json", mcp_json, "exec"),
	("pkg/.pi/SYSTEM.md", md, "text"), ("pkg/.claude/CLAUDE.md", md, "text"),
	("pkg/.agents/skills/probe/SKILL.md", skill, "text"), ("pkg/.claude/skills/probe/SKILL.md", skill, "text"),
	("pkg/.github/skills/probe/SKILL.md", skill, "text"), ("pkg/.pi/skills/probe/SKILL.md", skill, "text"),
	("pkg/.codex/skills/probe/SKILL.md", skill, "text"), ("pkg/.codex/config.toml", codex_config, "exec"),
	("pkg/.github/instructions/probe.instructions.md", instructions_md, "text"),
	("pkg/.github/agents/probe.agent.md", agent_md, "text"), ("pkg/.github/hooks/probe.json", github_hooks, "exec"),
	("pkg/.claude/commands/probe.md", command_md, "text"), ("pkg/.claude/settings.json", claude_settings, "exec"),
	("pkg/.vscode/mcp.json", mcp_json_vscode, "exec"),
]

# Within one directory a harness loads only the first of AGENTS.override.md,
# AGENTS.md and CLAUDE.md (pi, codex), so a marker beside a shadowing sibling
# would read as "not loaded". Each variant is a workspace without the
# siblings that shadow the files it judges.
VARIANTS = {
	"main": {"drop": ("AGENTS.override.md", ".github/mcp.json"), "judge_not": ("AGENTS.override.md", "CLAUDE.md", ".github/mcp.json")},
	"override": {"drop": ("AGENTS.md", "CLAUDE.md"), "judge": ("AGENTS.override.md",)},
	"claude": {"drop": ("AGENTS.md", "AGENTS.override.md"), "judge": ("CLAUDE.md",)},
	# .github/mcp.json beside .mcp.json and .vscode/mcp.json: one MCP file at a time.
	"mcp": {"drop": (".mcp.json", ".vscode/mcp.json", "AGENTS.override.md"), "judge": (".github/mcp.json",)},
}


def _named(spec, path):
	return path in spec or path.split("/")[-1] in spec


def variant_drops(variant, path):
	return _named(VARIANTS[variant]["drop"], path)


def variant_judges(variant, path):
	spec = VARIANTS[variant]
	if "judge" in spec:
		return _named(spec["judge"], path)
	return not _named(spec["judge_not"], path)


def build_candidates(table):
	"""Every candidate: {slug, path, kind, content, table_path}. The table's
	own entries each get a generic marker file as well (a dir entry a
	`probe-note.md` inside it), so a harness that reads a whole directory is
	caught whatever the file shapes above are."""
	seen = {}
	for path, shape, kind in SPECIFIC:
		seen[path] = (shape, kind)
	for d in table["dirs"]:
		seen.setdefault(d + "/PROBE-NOTE.md", (md, "text"))
	for f in table["files"]:
		seen.setdefault(f, (md, "text"))
	for n in table["names"]:
		seen.setdefault(n, (md, "text"))
		seen.setdefault("pkg/" + n, (md, "text"))
	out = []
	for path, (shape, kind) in seen.items():
		slug = slug_of(path)
		out.append({"slug": slug, "path": path, "kind": kind, "content": shape(slug), "table_path": covered_by(table, path)})
	out.sort(key=lambda c: c["path"])
	return out


def classify(candidates, request_text, executed):
	"""{slug: "request" | "executed" | "not_observed"}. The token in a logged
	request wins over a side effect: it says the text reached the model."""
	out = {}
	for c in candidates:
		if token(c["slug"]) in request_text:
			out[c["slug"]] = "request"
		elif c["slug"] in executed:
			out[c["slug"]] = "executed"
		else:
			out[c["slug"]] = "not_observed"
	return out


def table_observation(candidates, per_kind):
	"""{table path: sorted [turn kinds in which any of its candidates was
	observed]} from {turn kind: {slug: outcome}}."""
	seen = {}
	for c in candidates:
		if not c["table_path"]:
			continue
		seen.setdefault(c["table_path"], set())
		for kind, outcomes in per_kind.items():
			if outcomes.get(c["slug"], "not_observed") != "not_observed":
				seen[c["table_path"]].add(kind)
	return {k: sorted(v) for k, v in seen.items()}


def uncovered_observed(candidates, per_kind):
	"""[{"path", "turns", "how"}] for candidates outside the table that a
	harness loaded in some turn kind of per_kind ({turn kind: {slug: outcome}})."""
	out = []
	for c in candidates:
		if c["table_path"]:
			continue
		hits = {t: o[c["slug"]] for t, o in per_kind.items() if o.get(c["slug"], "not_observed") != "not_observed"}
		if hits:
			out.append({"path": c["path"], "turns": sorted(hits), "how": sorted(set(hits.values()))})
	return out


def fixture_from_result(result, table, old):
	"""The instruction_paths.json value that a result implies. A harness that
	was not probed keeps its old entry (source documented)."""
	candidates = result["candidates"]
	fixture = {"note": FIXTURE_NOTE}
	observed_anywhere = set()
	for name in HARNESSES:
		e = result["harnesses"].get(name)
		if not e or not e.get("turns") or not e["turns"].get("review"):
			fixture[name] = old[name]
			continue
		real = {t: v["outcomes"] for t, v in e["turns"].items() if t in REAL_TURNS}
		seen = table_observation(candidates, real)
		paths = sorted(p for p, kinds in seen.items() if kinds)
		order = {p: i for i, p in enumerate(table["dirs"] + table["files"] + table["names"])}
		paths.sort(key=lambda p: order[p])
		gated = table_observation(candidates, {"control": e["turns"]["control"]["outcomes"]}) if "control" in e["turns"] else {}
		observed_anywhere.update(paths)
		fixture[name] = {
			"version": parse_version(e["version"]) or old[name]["version"],
			"source": "probed",
			"paths": paths,
			"trust_gated": sorted((p for p, kinds in gated.items() if kinds and p not in paths), key=lambda p: order[p]),
			"uncovered": uncovered_observed(candidates, real),
			"uncovered_trust_gated": uncovered_observed(candidates, {"control": e["turns"]["control"]["outcomes"]}) if "control" in e["turns"] else [],
		}
	fixture["unprobed"] = [p for p in table["dirs"] + table["files"] + table["names"] if p not in observed_anywhere]
	return fixture


def write_fixture(path, result, table):
	fixture_path = pathlib.Path(path)
	fixture_path.write_text(json.dumps(fixture_from_result(result, table, json.loads(fixture_path.read_text())), indent="\t") + "\n")


def parse_version(text):
	m = re.search(r"\d+\.\d+\.\d+", text or "")
	return m.group(0) if m else None


FIXTURE_NOTE = ("Instruction paths each harness loads from a workspace, measured by scripts/probe_instruction_paths.py "
	"against the worker image's pinned versions: a fake model endpoint logged every request of a review turn, a build turn "
	"and a turn that reads a file under a subdirectory, and a path counts when its marker reached a request or a hook, MCP "
	"server or extension it names ran. A table path is counted wherever it sits in the workspace: pkg/.github/instructions "
	"is .github/instructions. source probed means measured; documented means read from the harness documentation. "
	"trust_gated lists table paths a harness loads only when the project is trusted, which a worker turn never grants. "
	"uncovered lists paths outside the review snapshot's table that a harness loaded in a worker turn, "
	"uncovered_trust_gated those it loads only in a trusted project. pifork is built from the pi base and "
	"carries pi's version and documented paths until an image with its launcher is probed. unprobed lists the table paths "
	"no probed harness was observed loading.")


def executed_slugs(out_dir):
	return {p.name[len("executed-"):] for p in pathlib.Path(out_dir).glob("executed-*")}


# ---------------------------------------------------------------------------
# Workspace

def write_workspace(root, candidates, variant="main"):
	root = pathlib.Path(root)
	if root.exists():
		shutil.rmtree(root)
	root.mkdir(parents=True)
	for c in candidates:
		if variant_drops(variant, c["path"]):
			continue
		p = root / c["path"]
		p.parent.mkdir(parents=True, exist_ok=True)
		p.write_text(c["content"])
	(root / "README.md").write_text("probe workspace\n")
	(root / LAZY_FILE).parent.mkdir(parents=True, exist_ok=True)
	(root / LAZY_FILE).write_text("lazy file contents\n")
	env = {**os.environ, "GIT_AUTHOR_NAME": "probe", "GIT_AUTHOR_EMAIL": "p@example.invalid",
		"GIT_COMMITTER_NAME": "probe", "GIT_COMMITTER_EMAIL": "p@example.invalid"}
	for args in (["init", "-q", "-b", "main"], ["add", "-A"], ["commit", "-q", "-m", "probe markers"]):
		subprocess.run(["git", "-C", str(root), *args], check=True, env=env, stdout=subprocess.DEVNULL)
	subprocess.run(["chmod", "-R", "a+rwX", str(root)], check=True)


# ---------------------------------------------------------------------------
# Inside the container

def sse(events):
	return "".join(f"event: {name}\ndata: {json.dumps(data)}\n\n" if name else f"data: {data if isinstance(data, str) else json.dumps(data)}\n\n" for name, data in events)


def pick_tool_call(body):
	"""(tool name, arguments dict) that reads LAZY_FILE using a tool the
	request offers, or None."""
	tools = body.get("tools") or []
	flat = []
	for t in tools:
		fn = t.get("function") or t
		flat.append((fn.get("name") or t.get("name"), fn.get("parameters") or t.get("input_schema") or {}))
	by_name = {n: s for n, s in flat if n}
	for name in ("read", "view", "read_file", "Read"):
		if name in by_name:
			props = by_name[name].get("properties", {})
			key = next((k for k in ("path", "file_path", "filePath", "file") if k in props), None)
			if key:
				return name, {key: LAZY_FILE}
	for name in ("bash", "shell", "Bash", "shell_command", "exec_command", "local_shell"):
		if name in by_name:
			props = by_name[name].get("properties", {})
			key = next((k for k in ("command", "cmd") if k in props), None)
			if key:
				return name, {key: "cat " + LAZY_FILE} if props[key].get("type") != "array" else {key: ["bash", "-lc", "cat " + LAZY_FILE]}
	return None


def completions_reply(body, tool_call):
	model = body.get("model", "probe")
	base = {"id": "chatcmpl-probe", "object": "chat.completion.chunk", "created": 0, "model": model}
	if tool_call:
		name, args = tool_call
		delta = {"role": "assistant", "tool_calls": [{"index": 0, "id": "call_probe", "type": "function", "function": {"name": name, "arguments": json.dumps(args)}}]}
		finish = "tool_calls"
	else:
		delta = {"role": "assistant", "content": "ok"}
		finish = "stop"
	events = [(None, {**base, "choices": [{"index": 0, "delta": delta, "finish_reason": None}]}),
		(None, {**base, "choices": [{"index": 0, "delta": {}, "finish_reason": finish}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}),
		(None, "[DONE]")]
	return sse(events)


def responses_reply(body, tool_call):
	model = body.get("model", "probe")
	resp = {"id": "resp_probe", "object": "response", "created_at": 0, "model": model, "status": "in_progress", "output": []}
	if tool_call:
		name, args = tool_call
		item = {"type": "function_call", "id": "fc_probe", "call_id": "call_probe", "name": name, "arguments": json.dumps(args), "status": "completed"}
		added = {**item, "status": "in_progress", "arguments": ""}
		events = [("response.created", {"type": "response.created", "response": resp}),
			("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": added}),
			("response.function_call_arguments.done", {"type": "response.function_call_arguments.done", "item_id": "fc_probe", "output_index": 0, "arguments": item["arguments"]}),
			("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": item})]
		output = [item]
	else:
		item = {"type": "message", "id": "msg_probe", "role": "assistant", "status": "completed", "content": [{"type": "output_text", "text": "ok", "annotations": []}]}
		added = {**item, "status": "in_progress", "content": []}
		events = [("response.created", {"type": "response.created", "response": resp}),
			("response.output_item.added", {"type": "response.output_item.added", "output_index": 0, "item": added}),
			("response.content_part.added", {"type": "response.content_part.added", "item_id": "msg_probe", "output_index": 0, "content_index": 0, "part": {"type": "output_text", "text": "", "annotations": []}}),
			("response.output_text.delta", {"type": "response.output_text.delta", "item_id": "msg_probe", "output_index": 0, "content_index": 0, "delta": "ok"}),
			("response.output_text.done", {"type": "response.output_text.done", "item_id": "msg_probe", "output_index": 0, "content_index": 0, "text": "ok"}),
			("response.content_part.done", {"type": "response.content_part.done", "item_id": "msg_probe", "output_index": 0, "content_index": 0, "part": item["content"][0]}),
			("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": item})]
		output = [item]
	done = {**resp, "status": "completed", "output": output, "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}
	events.append(("response.completed", {"type": "response.completed", "response": done}))
	return sse(events)


def serve(port, mode, api, log_path):
	"""The fake model endpoint on 127.0.0.1. mode: fail (HTTP 500), text (a
	valid streamed reply with the text "ok"), tool (one tool call that reads
	LAZY_FILE on the first request that offers a tool, then text)."""
	import http.server
	import threading

	state = {"tool_done": False}
	lock = threading.Lock()

	class Handler(http.server.BaseHTTPRequestHandler):
		def log_message(self, *a):
			pass

		def _handle(self):
			n = int(self.headers.get("Content-Length") or 0)
			raw = self.rfile.read(n).decode("utf-8", "replace") if n else ""
			with lock, open(log_path, "a") as f:
				f.write(json.dumps({"method": self.command, "path": self.path, "body": raw}) + "\n")
			if mode == "fail" or self.command != "POST" or not (self.path.endswith("/responses") or self.path.endswith("/completions")):
				if self.command == "GET" and self.path.rstrip("/").endswith("/models"):
					data = json.dumps({"object": "list", "data": [{"id": "probe-model", "object": "model"}]}).encode()
					self.send_response(200)
					self.send_header("Content-Type", "application/json")
					self.send_header("Content-Length", str(len(data)))
					self.end_headers()
					self.wfile.write(data)
					return
				data = b'{"error":{"message":"probe: no model"}}'
				self.send_response(500 if mode == "fail" else 404)
				self.send_header("Content-Type", "application/json")
				self.send_header("Content-Length", str(len(data)))
				self.end_headers()
				self.wfile.write(data)
				return
			try:
				body = json.loads(raw)
			except ValueError:
				body = {}
			tool_call = None
			with lock:
				if mode == "tool" and not state["tool_done"]:
					tool_call = pick_tool_call(body)
					state["tool_done"] = tool_call is not None
			streamed = (responses_reply if self.path.endswith("/responses") else completions_reply)(body, tool_call).encode()
			self.send_response(200)
			self.send_header("Content-Type", "text/event-stream")
			self.send_header("Content-Length", str(len(streamed)))
			self.end_headers()
			self.wfile.write(streamed)

		do_GET = do_POST = _handle

	srv = http.server.ThreadingHTTPServer(("127.0.0.1", port), Handler)
	threading.Thread(target=srv.serve_forever, daemon=True).start()
	return srv


def inner(args):
	"""Runs in the container: fake endpoint, then the harness as a buildgate
	turn launches it."""
	sys.path.insert(0, "/scripts")
	import harness_adapters

	port = 18080
	log = "/probe-out/requests.jsonl"
	pathlib.Path(log).write_text("")
	serve(port, args.mode, APIS[args.harness], log)
	os.environ.update({
		"FACTORY_MODEL_ID": "probe-model",
		"FACTORY_MODEL_BASE_URL": f"http://127.0.0.1:{port}/v1",
		"FACTORY_MODEL_API": APIS[args.harness],
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "*",
	})
	adapter = harness_adapters.get(args.harness)
	adapter.prepare()
	session = pathlib.Path("/tmp/probe-session")
	session.mkdir(parents=True, exist_ok=True)
	prompt = "Read " + LAZY_FILE + " and then reply with the single word ok." if args.mode == "tool" else "Reply with the single word ok."
	if args.turn == "control":
		# Positive control: the project is trusted, so a candidate that fires
		# here but not in a real turn was held back by project trust and not by
		# a wrong file shape.
		os.environ["COPILOT_ALLOW_ALL"] = "true"
		agent_dir = adapter.agent_dir() if hasattr(adapter, "agent_dir") else None
		if agent_dir is not None:
			(agent_dir / "settings.json").write_text(json.dumps({"defaultProjectTrust": "always"}))
	command = adapter.invocation(pathlib.Path("/workspace"), prompt=prompt, session_dir=session,
		continue_session=False, thinking=None, load_repo_skills=args.turn == "build")
	if args.turn == "control" and args.harness == "codex":
		home = session / "codex-home"
		(home / "config.toml").write_text('[projects."/workspace"]\ntrust_level = "trusted"\n')
	started = time.time()
	try:
		done = subprocess.run(command, cwd="/workspace", stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=args.timeout)
		code, out, err = done.returncode, done.stdout, done.stderr
	except subprocess.TimeoutExpired as exc:
		code, out, err = "timeout", (exc.stdout or b"").decode("utf-8", "replace") if isinstance(exc.stdout, bytes) else (exc.stdout or ""), (exc.stderr or b"").decode("utf-8", "replace") if isinstance(exc.stderr, bytes) else (exc.stderr or "")
	time.sleep(1)
	pathlib.Path("/probe-out/exit.json").write_text(json.dumps({"exit": code, "seconds": round(time.time() - started, 1), "command_head": command[:1] if command[0] != "env" else [a for a in command if not a.startswith(("FACTORY", "COPILOT_PROVIDER_API"))][:12]}))
	pathlib.Path("/probe-out/stdout.txt").write_text(out[-20000:])
	pathlib.Path("/probe-out/stderr.txt").write_text(err[-20000:])
	return 0


# ---------------------------------------------------------------------------
# Host

def default_image():
	cfg = pathlib.Path("~/.config/factoryd/config.yml").expanduser()
	for line in cfg.read_text().splitlines():
		m = re.match(r"\s*sandbox_image:\s*(\S+)", line)
		if m:
			return m.group(1)
	raise SystemExit("no sandbox_image in ~/.config/factoryd/config.yml; pass --image")


def docker(*args, timeout=60):
	return subprocess.run(["docker", *args], capture_output=True, text=True, timeout=timeout)


def image_versions(image):
	"""{harness: version string the image reports}."""
	cmds = {"pi": "pi --version", "codex": "codex --version", "copilot": "copilot --version", "pifork": "pifork --version"}
	out = {}
	for h, cmd in cmds.items():
		r = docker("run", "--rm", "--network", "none", "--entrypoint", "sh", image, "-c", cmd + " 2>&1 | head -1", timeout=120)
		out[h] = r.stdout.strip() if r.returncode == 0 else None
	return out


def has_launcher(image, name):
	r = docker("run", "--rm", "--network", "none", "--entrypoint", "sh", image, "-c", f"command -v {name}", timeout=120)
	return r.returncode == 0 and bool(r.stdout.strip())


def run_variant(image, harness, turn, variant, candidates, mode, base):
	"""One container on one workspace variant. Returns the raw observation."""
	run_dir = base / f"{harness}-{turn}-{variant}"
	ws, out = run_dir / "workspace", run_dir / "out"
	if run_dir.exists():
		shutil.rmtree(run_dir)
	out.mkdir(parents=True)
	subprocess.run(["chmod", "a+rwx", str(out)], check=True)
	write_workspace(ws, candidates, variant)
	name = f"instruction-probe-{harness}-{turn}-{variant}-{os.getpid()}"
	cmd = ["run", "--rm", "--network", "none", "--name", name,
		"-v", f"{ws}:/workspace", "-v", f"{out}:/probe-out",
		"-v", f"{ROOT / 'agent' / 'pi' / 'scripts'}:/scripts:ro",
		"-v", f"{pathlib.Path(__file__).resolve()}:/probe/probe_instruction_paths.py:ro",
		"--entrypoint", "python3", image, "/probe/probe_instruction_paths.py", "--inner",
		"--harness", harness, "--turn", turn, "--mode", mode, "--timeout", str(TIMEOUT_S)]
	try:
		docker(*cmd, timeout=TIMEOUT_S + 120)
	except subprocess.TimeoutExpired:
		docker("rm", "-f", name)
	requests = []
	log = out / "requests.jsonl"
	if log.exists():
		requests = [json.loads(l) for l in log.read_text().splitlines() if l.strip()]
	exit_info = json.loads((out / "exit.json").read_text()) if (out / "exit.json").exists() else {}
	return {
		"bodies": [r["body"] for r in requests],
		"request_paths": sorted({f'{r["method"]} {r["path"]}' for r in requests}),
		"model_requests": sum(1 for r in requests if r["method"] == "POST" and re.search(r"/(responses|completions)$", r["path"])),
		"executed": executed_slugs(out),
		"exit": exit_info.get("exit"),
		"seconds": exit_info.get("seconds"),
		"stderr_tail": (out / "stderr.txt").read_text()[-300:] if (out / "stderr.txt").exists() else "",
	}


def run_one(image, harness, turn, candidates, mode, base):
	"""All variants of one (harness, turn). Each candidate is judged in the
	variants that judge it. Returns {outcomes, evidence, executed, runs}."""
	outcomes, evidence, executed, runs = {}, {}, set(), {}
	for variant in VARIANTS:
		raw = run_variant(image, harness, turn, variant, candidates, mode, base)
		runs[variant] = {k: raw[k] for k in ("request_paths", "model_requests", "exit", "seconds", "stderr_tail")}
		judged = [c for c in candidates if variant_judges(variant, c["path"]) and not variant_drops(variant, c["path"])]
		outcomes.update(classify(judged, "\n".join(raw["bodies"]), raw["executed"]))
		for c in judged:
			if c["slug"] in raw["executed"]:
				executed.add(c["slug"])
			for i, body in enumerate(raw["bodies"]):
				k = body.find(token(c["slug"]))
				if k >= 0:
					evidence[c["slug"]] = {"request": i + 1, "of": len(raw["bodies"]), "excerpt": body[max(0, k - 50): k + len(token(c["slug"])) + 30]}
					break
	return {"outcomes": outcomes, "evidence": evidence, "executed": sorted(executed), "runs": runs}


def print_table(candidates, result):
	columns = [(h, t) for h in result["harnesses"] for t in TURNS if t in result["harnesses"][h]["turns"]]
	if not columns:
		return
	head = f'{"path":58} {"table":28} ' + " ".join(f"{h[:4]}/{t[:3]}".ljust(10) for h, t in columns)
	print(head)
	for c in candidates:
		cells = []
		for h, t in columns:
			o = result["harnesses"][h]["turns"][t]["outcomes"].get(c["slug"], "-")
			cells.append({"request": "REQUEST", "executed": "EXECUTED", "not_observed": ".", "-": "-"}[o].ljust(10))
		print(f'{c["path"]:58} {(c["table_path"] or "NOT IN TABLE"):28} ' + " ".join(cells))


def main():
	ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
	ap.add_argument("--list", action="store_true")
	ap.add_argument("--image")
	ap.add_argument("--pifork-image")
	ap.add_argument("--harness", action="append", choices=HARNESSES)
	ap.add_argument("--mode", default="text", choices=("fail", "text", "tool"))
	ap.add_argument("--inner", action="store_true")
	ap.add_argument("--turn", choices=TURNS, default="review")
	ap.add_argument("--timeout", type=int, default=TIMEOUT_S)
	ap.add_argument("--keep", action="store_true")
	ap.add_argument("--result", help="result file (default: <scratch>/result.json)")
	ap.add_argument("--write-fixture", metavar="PATH", help="rewrite the instruction_paths.json at PATH from the result")
	ap.add_argument("--from-result", metavar="PATH", help="rebuild the fixture from this saved result instead of running the probe")
	args = ap.parse_args()
	if args.inner:
		args.harness = args.harness[0]
		return inner(args)

	table = parse_table(GO_TABLE.read_text())
	candidates = build_candidates(table)
	if args.list:
		for c in candidates:
			print(f'{c["path"]:58} {c["kind"]:5} {c["table_path"] or "NOT IN TABLE"}')
		print(f'{len(candidates)} candidates, {sum(1 for c in candidates if not c["table_path"])} outside the table', file=sys.stderr)
		return 0

	if args.from_result:
		if not args.write_fixture:
			raise SystemExit("--from-result needs --write-fixture")
		result = recover_candidates(json.loads(pathlib.Path(args.from_result).expanduser().read_text()), table)
		write_fixture(args.write_fixture, result, table)
		return 0

	if not str(SCRATCH).startswith(str(pathlib.Path("~/buildgate").expanduser())):
		raise SystemExit("scratch must be under ~/buildgate")
	SCRATCH.mkdir(parents=True, exist_ok=True)
	image = args.image or default_image()
	versions = image_versions(image)
	result = {"image": image, "candidates": [{k: c[k] for k in ("slug", "path", "kind", "table_path")} for c in candidates], "harnesses": {}}
	for harness in args.harness or HARNESSES:
		h_image = args.pifork_image if harness == "pifork" else image
		entry = {"version": versions.get(harness), "turns": {}}
		result["harnesses"][harness] = entry
		if h_image is None or not has_launcher(h_image, harness):
			entry["skipped"] = "no image carrying the `pifork` launcher" if harness == "pifork" else f"no {harness} launcher in the image"
			print(f"{harness}: skipped ({entry['skipped']})", file=sys.stderr)
			continue
		for turn in TURNS:
			mode = "tool" if turn == "lazy" else args.mode
			obs = run_one(h_image, harness, turn, candidates, mode, SCRATCH)
			entry["turns"][turn] = obs
			summary = {v: (r["exit"], r["model_requests"]) for v, r in obs["runs"].items()}
			print(f"{harness}/{turn}: (exit, model requests) per variant {summary}", file=sys.stderr)
	result["table_observed"] = {h: table_observation(candidates, {t: v["outcomes"] for t, v in e["turns"].items() if t in REAL_TURNS}) for h, e in result["harnesses"].items() if e["turns"]}
	out_file = pathlib.Path(args.result).expanduser() if args.result else SCRATCH / "result.json"
	out_file.write_text(json.dumps(result, indent=1) + "\n")
	print_table(candidates, result)
	if args.write_fixture:
		write_fixture(args.write_fixture, result, table)
	print(f"result: {out_file}", file=sys.stderr)
	return 0


if __name__ == "__main__":
	sys.exit(main())
