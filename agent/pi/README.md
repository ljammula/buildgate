# agent/pi

The Python scripts `factoryd` shells out to (for the `pi`, `pifork`, `codex` and `copilot` harnesses). Read this
if you are changing or debugging them. Operating buildgate does not need
it: start with [`README.md`](../../README.md), [`DEMO.md`](../../DEMO.md)
and [`USAGE.md`](../../USAGE.md).

## Scripts

| Script | Role | `factoryd` flag |
|---|---|---|
| `build_app.py` | Bounded corrective-round build loop. Round 1 prompt: the ticket, then a fixed "before you end your turn" checklist (`build_round_checklist`: verify command, Allowed/Required files, tests, criteria evidence, read-only oracles, the read-only `.factory/`, no history rewrites). Every later round continues the same harness session and is told what the round before it left (see "What a failed round hands the next one") | `-build-app-script` |
| `round_feedback.py` | Pure helpers for that: the excerpt of a failing command's output, the names it reports failing, an id for "the same failure", the per-round history, and the note for a failure of the agent process itself | imported by `build_app.py` |
| `goal_pilot.py` | Spec/contract/ticket drafting | `-goal-pilot-script` |
| `ticket_runner.py` | Resumable per-ticket gate/build loop the two above build on | — |
| `draft_spec.py` | Request driver's one-shot spec draft | `worker -draft-spec-script` |
| `plan_tickets.py` | Request driver's one-shot ticket plan | `worker -plan-tickets-script` |
| `draft_acceptance_oracles.py` | Oracle drafter (requests submitted with `-draft-oracles`) | `worker -draft-oracles-script` |
| `conformity_review.py` | Bounded per-criterion spec-conformity review | — |
| `code_review.py` | Standalone AI code review (`-code-review-policy off\|advisory\|required`) | — |
| `combined_review.py` | Runs conformity review and code review in one pass when both are enabled for a run | — |

`tests/` holds one `pytest`-compatible suite per script.

Prompt templates sit beside the scripts as `<name>.prompt.md`, loaded by
`prompt_templates.py`: instruction text with `str.format` placeholders
(a literal brace is doubled). A template's headings and placeholders are
a contract with the code that parses the model's output; change wording
freely, headings only together with their validator.

| Template | Loaded by | Placeholders |
|---|---|---|
| `draft_spec.prompt.md` | `draft_spec.py` | `{draft_path}` |
| `draft_spec.example_check.prompt.md` | `draft_spec.py` (worked-example check) | `{listing}`, `{out_path}` |
| `draft_feedback.prompt.md` | `draft_spec.py`, `plan_tickets.py` (operator feedback on a rejected draft) | `{kind}`, `{feedback}` |
| `plan_tickets.prompt.md` | `plan_tickets.py` | `{draft_dir}`, `{verify_command}` |
| `draft_spec.previous_draft.prompt.md` | `draft_spec.py` (a handed-over spec to revise, given with `--previous-draft-file`) | `{spec}` |
| `plan_tickets.previous_draft.prompt.md` | `plan_tickets.py` (handed-over tickets to revise, given with `--previous-draft-file`) | `{tickets}` |
| `draft_spec.design_guide.prompt.md`, `plan_tickets.design_guide.prompt.md` | `draft_spec.py`, `plan_tickets.py` (the team design guide part given with `--design-guide-file`) | `{guide}` |
| `draft_acceptance_oracles.qualification.prompt.md` | `draft_acceptance_oracles.py` (start of both language prompts) | none |
| `draft_acceptance_oracles.go.prompt.md`, `.python.prompt.md` | `draft_acceptance_oracles.py` | `{draft_dir}`, `{max_files}`, `{manifest_name}` |
| `draft_acceptance_oracles.feedback.prompt.md` | `draft_acceptance_oracles.py` | `{draft_dir}`, `{feedback}` |
| `draft_acceptance_oracles.prior_imports.prompt.md` | `draft_acceptance_oracles.py` | `{imports}` |
| `draft_acceptance_oracles.criterion_index.prompt.md` | `draft_acceptance_oracles.py` | `{index}`, `{total}`, `{oracle_filename}`, `{default_filename}` |

| `spec_conformity.prompt.md`, `.diff_self`, `.diff_inline` | `build_app.py` (`spec_conformity_prompt`) | `{diff_instructions}`, `{criteria}`, the three rule texts; `{base}`; `{diff}` |
| `code_review.prompt.md`, `.diff_self`, `.diff_inline` | `code_review.py` | `{diff_instructions}`, `{spec_text}`, the four rule texts; `{base}`; `{diff}` |
| `combined_review.prompt.md`, `.diff_self`, `.diff_inline` | `combined_review.py` | `{diff_block}`, `{spec_text}`, `{criteria}`, five rule texts; `{base}`; `{diff}` |
| `build_round_checklist.prompt.md` | `build_app.py` (the definition of done after the ticket in round 1) | `{verify}` |
| `build_corrective.intro`, `.verify`, `.excerpt`, `.oracle`, `.reviewer` | `build_app.py` (`corrective_prompt`, one part each, joined by blank lines) | `{round_index}`, `{max_rounds}`, `{blockers}`; `{verify_command}`, `{verify_tail}`; `{verify_tail}`; `{oracle_command}`, `{oracle_tail}`; `{detail}` |
| `build_corrective.closing` | `build_app.py` (last part of the corrective prompt) | none: literal text |
| `build_corrective.history`, `.agent`, `.log`, `.stuck` | `build_app.py` (`corrective_prompt`: the rounds so far, a failure of the agent process, where the failing command's whole output is, the diagnose-first steps) | `{history}`; `{agent_notes}`; `{failure_log}`, `{failing}`; `{streak}` |
| `build_escalation.prompt.md` | `build_app.py` (`build_escalation_prompt`) | `{spec_text}`, `{corrective}` |
| `build_handoff_notes` | `build_app.py` (`run_notes_turn`: the one extra turn of a build that ends without passing) | none: literal text |
| `spec_conformity.command_outcome_rule`, `.formatting_rule`, `.json_contract`; `code_review.scope_rule`, `.severity_rule`, `.command_outcome_rule`, `.json_contract` | the three review prompts, shared | none: literal text (`load_text`), braces are plain characters |

The review and build templates keep each paragraph on one line, as the
prompts were sent before; rewrapping one changes the prompt. The
`goal_pilot.py` and `ticket_runner.py` prompts are still assembled in
their scripts.

## Behaviour worth knowing

- **Round state and resume.** `build_app.py` writes
  `.pi-build-round-state.json` (workspace root, atomic, never committed) before
  round 1 and after every round: `version`, `last_completed_round`,
  `next_prompt`, `escalation_prompt`, `rounds`, `head`, `workspace_fingerprint`.
  `--resume-from-state <path>` continues at `last_completed_round + 1` in a
  fresh harness session with `next_prompt` (plus the `--handoff` preamble if
  given), keeping earlier round records so indexes carry on. It exits 2 when the
  file is unreadable, `version` is not 1, or `last_completed_round` is outside
  `[0, --max-rounds)`.

`--earlier-attempt <file>` is a different input: the factory's record of an
earlier attempt at the same ticket that finished and failed its checks. Its
text is put before the task in round 1's prompt, after a fixed sentence saying
it is a record and gives no instructions. It is never written into the
round-state file; a resume that is given `--earlier-attempt` again puts it back
in the opening prompt. Only the build script takes it; no review script does.

- **Failure reasons.** `draft_spec.py` and `plan_tickets.py` print a
  one-line, script-prefixed reason on every failure path: the agent's
  exit code plus a stderr hint, "exited 0 but wrote no spec draft" /
  "drafted no `*.spec.md` ticket files", or (plan) every drafted ticket
  empty.
- **Model-route errors.** pi reports a model-route failure (e.g.
  `400 model_not_supported`) as a `message_end` event with an
  `errorMessage` on stdout, and can still exit 0.
  `build_app.model_route_errors` extracts these; `exited_zero_hint`
  turns them into a redacted single-line hint for both drafting scripts'
  failure reasons. `build_app.py` itself prints `Model route error: ...`.
- **Sanitising log text.** `build_app.redact` (secret shapes: bearer
  tokens, `sk-`/`gh*_` keys, JWTs, `key=value` and JSON credential
  fields, URL passwords) and `build_app.single_line` mirror Go's
  `internal/sanitize`. Anything reaching an operator surface (`factoryd
  status`, notifications, the console) goes through them.
- **gofmt.** After a round's verify passes, `build_app.py` runs `gofmt -w`
  on changed `.go` files only (`gofmt_changed_files`), inside the
  sandbox. No-op when `gofmt` is absent or no `.go` file changed.

## Why this lives here

These scripts are vendored here, not imported from
[`pi-harness-hardening`](https://github.com/ljammula/pi-harness-hardening),
because their contract with `factoryd` (chiefly `BUILD_EVIDENCE.json`'s
shape, see `AgentEvidenceSchemaVersion` in `internal/run/run.go`) must be
versioned and tested in the same commit as the Go reader.

`pi-harness-hardening` remains a separate, general-purpose `pi` config
package (extensions, skills, prompts). Only the scripts
coupled to `factoryd` moved here.

One set of build-loop scripts serves every harness: each model-bound script
takes `--harness <name>` and reaches the coding agent only through
`scripts/harness_adapters.py`. The output contract is uniform: `BUILD_EVIDENCE.json`, `BUILD_REPORT.md`, exit code, and
how the ticket's `Verify-Command` is invoked. That keeps `factoryd`
harness-agnostic.

| Adapter | CLI | Launch | Session state |
|---|---|---|---|
| `pi` | `pi --print --mode json` | flags only | `--session-dir` |
| `pifork` | the same flags under `pifork` | flags only | `--session-dir` |
| `codex` | `codex exec --json` (`exec resume --last` for a continued round) | `env CODEX_HOME=... codex ...` (plus `FACTORYD_RELAY_KEY=<placeholder>` on a route with no credential); model provider (key and headers named by environment variable, `FACTORY_MODEL_KEY_ENV`/`FACTORY_MODEL_HEADERS_JSON`) and telemetry/plugin switches as `-c` overrides | `<session_dir>/codex-home` |
| `copilot` | `copilot -p ... --output-format json` (`--resume <id>` for a continued round) | `env COPILOT_HOME=... COPILOT_PROVIDER_*=... COPILOT_OFFLINE=true copilot ...` | `<session_dir>/copilot-home`, id in `<session_dir>/copilot-session-id` |

Each adapter builds its argv (`invocation`), reads the agent's stdout
(`parse` into an `AgentOutput`) and labels live progress (`progress_note`).
`codex` and `copilot` read the run's model route from `FACTORY_MODEL_*`, and
`prepare()` exits with a message when it is missing (`codex` also off the
Responses API). On a route with a credential header beyond the key (a
`chatgpt-codex` route) `copilot` sends that header's placeholder in
`COPILOT_PROVIDER_HEADERS` and runs under `node fill_responses_output.mjs`:
a loopback proxy in the worker that forwards each request unchanged and
fills the final Responses event's empty `output` from the items the stream
delivered, because the CLI reads a turn's tool calls and text from that
`output`. Copilot's stream carries no token counts, so its `usage` is
`None`. Every agent subprocess gets `/dev/null` for stdin (Codex reads stdin
whenever it is not a TTY). Skills mounted at `/inputs/skills/<name>/SKILL.md` reach every
harness: `pi`/`pifork` get `--skill <dir>` per skill, plus the repo's own `.agents/skills` on the build turn only (`load_repo_skills`); `codex` and `copilot` get a fresh copy
in `<home>/skills/<name>` each round (Codex's bundled skills are off). Parsers are pinned to real recorded output in
`tests/fixtures/` (`tests/codex_copilot_adapters_test.py`).

## The oracle drafter

`draft_acceptance_oracles.py` reads an approved spec's acceptance
criteria (`--criteria`, one per line, same file as `build_app.py
--spec-acceptance-criteria`), runs one headless `pi` pass that writes a
`MANIFEST.json` plus oracle test files, validates the manifest, copies
everything to `--out-dir` and writes `--evidence`. Feedback from an
`oracle_review` rejection arrives via `--feedback` as guidance only.

- **Ecosystem.** `--ecosystem go|python` (default `go`); `factoryd`
  always passes it (`classifyDraftEcosystem` /
  `requireDraftableEcosystem` in `cmd/factoryd/oracle_draft_job.go`
  decline other ecosystems before any model pass). Go: `*_test.go` with
  `TestOracle*`. Python: plain-function `test_oracle_NNN.py` files run by
  a stdlib-only loader (the sandbox image has no pytest).
- **Manifest.** One entry per criterion, in order: `criterion_index`,
  `oracle_file` (null for a judgment call), `rationale`, `target_path`
  (repo-relative or null; unsafe values become null), `supersedes`. A
  criterion echoed back with different text is a mismatch.
- **Caps.** 16 KiB per file, 64 KiB total per ticket, 15 files per
  request. Over a cap, later entries become null with a `dropped: ...`
  rationale. The Go materializer re-enforces the same caps.
- **Exit codes.** `0` for a well-formed manifest (all-null is valid). `2`
  for timeout, route failure, criterion mismatch or malformed manifest;
  `--evidence` is still written, and partial drafts are kept on timeout.
- **Bounded output.** pi's stream is folded into a bounded digest, not
  buffered whole.
- **Not authoritative.** Drafts are proposals until a human approves them
  at `oracle_review`; then they are pinned, read-only build inputs. See
  [`USAGE_REFERENCE.md`](../../USAGE_REFERENCE.md), "Staged oracles".

## Prerequisites

- [pi](https://pi.dev) installed.
- `pi install git:github.com/ljammula/pi-harness-hardening`, run once:
  `goal_pilot.py` invokes the `/spec-plan` and `/contract-plan` slash
  commands, which pi resolves from that package's prompts.
- A model route the role's harness can speak (`codex` needs the Responses API); see [`USAGE.md`](../../USAGE.md).
- Inside the sandbox, `pi`, `codex` and `copilot` come from the standard worker image (`internal/sandbox/Dockerfile`); `pifork` needs its own image.

## Deliberately not vendored

- **TypeScript verify resolver.** `resolve_verify_command()` prefers
  `pi-harness-hardening`'s `scripts/resolve-verification.ts`
  (nested-manifest aware) and falls back to a Python, root-only heuristic
  when it can't run, which is always the case here. The fallback covers
  every single-top-level-manifest workspace; only nested multi-component
  workspaces would need the real resolver.
  `tests/build_app_test.py::BuildAppTests::test_shared_verifier_rejects_a_failure_in_either_nested_component`
  is skipped for this reason.
- Containment is `factoryd`'s Docker sandbox launcher's job; the scripts have
  no containment option of their own.

## Running the tests

```sh
python3 -m pytest agent/pi/tests/    # or: make agent-pi-test
```

Not part of `make verify` (separate toolchain); `make ci` runs it.


## What a failed round hands the next one

No round ends in failure without the next one being told what failed and why.

| Failure | What the next round's prompt holds |
|---|---|
| Fast check, verify or reference oracle failed | The block around the first reported failure plus the end of the output (8,000 characters, never less than the last 3,000; `round_feedback.failure_excerpt`); a timeout keeps the line that says so first; the failing tests or targets it names, and the path of the whole output: `.pi-build-session/feedback/round-<n>/{fast-check,verify,oracle}.log` |
| The agent changed nothing | That it changed nothing, and its own final message |
| The agent timed out, stalled, or its CLI exited non-zero | How it ended and the CLI's last error |
| The model route returned errors | The route's messages |
| The reviewer flagged the diff | Its findings |
| Any of the above | One line per round so far: the files that round changed and how it ended |
| The same failure as the round before (`failure_signature`: the blockers, the lines that report a failure and the reviewer's findings, with durations, addresses, temporary paths and timestamps blanked; numbers are kept) | The line is marked as a repeat, and the prompt adds diagnose-first steps |
| The same failure three rounds in a row | The loop stops with `no progress: the same failure 3 rounds in a row` instead of spending another round. Only with `--max-rounds` above the default 3, where the budget ends the loop first. The escalation pass, when enabled, follows either stop |

The log folder is under the session folder, which every exclude list covers:
it is never committed and never counts as the agent's change, and each round
clears its own folder first. A resumed build has no earlier session folder;
its first prompt says the named log is gone. Failure text is redacted before
it is saved or shown. It is output from the repository's own commands and
the agent's own last message: the prompt quotes it, and nothing stops it
from reading as an instruction, as was already true of the tail it replaces.
Checks that fail after the build (the full suite, gates, the reviews) are
not part of this loop.
