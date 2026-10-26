Independent code review.

{diff_block}

--- ticket spec (context for the change's intent) ---
{spec_text}
--- end ticket spec ---

{approved_spec}

=== Code review ===

{scope_rule}

{severity_rule}

{reproduction_rule}

{command_outcome_rule}

=== Answer format ===

Respond with ONLY a single JSON object of the form {{"findings": [{{"severity": "high"|"medium"|"low", "file": "<path>", "line": <int>, "summary": "<what is wrong>", "failure_scenario": "<inputs/state that trigger it and the wrong result>", "spec_line": {{"number": <int>, "text": "<the spec line's text>"}}, "reproduction": {{"path": "<new test file>", "content": "<the whole file>"}}}}]}} -- or {{"findings": []}} when there are none. "spec_line" and "reproduction" are optional on each finding; leave them out when the finding has none. No prose outside the JSON.
