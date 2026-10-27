The verify command and the full test suite already passed on this change, and the factory runs the change's own tests itself. Do not re-check behaviour the tests cover, do not run commands, and do not trace ordinary logic looking for functional bugs. Report one only if you notice it while reading.

Look for what tests cannot show:
- Design: the change does not fit the existing structure, duplicates an existing helper, or gives a concept a second home.
- Security: untrusted input, injection, path or permission handling, secrets, authorization.
- Maintainability: dead code, needless complexity, unclear contracts, missing error handling at a boundary.
- Risk: data loss, concurrency, compatibility, migration, performance cliffs.

Read the diff and only the code it directly touches (the callers and callees of changed functions). Keep tool use to a minimum.

Report at most 5 findings, most important first, each tied to a file and line, each specific and actionable. Report none when nothing is worth a reviewer's time. Do NOT report style, formatting, naming, lint, or missing-comment issues. The "failure_scenario" field is one sentence on why the finding matters.
