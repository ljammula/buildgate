Independent code review.

{diff_block}

--- ticket spec (context for the change's intent) ---
{spec_text}
--- end ticket spec ---

=== Code review ===

{scope_rule}

{severity_rule}

{command_outcome_rule}

=== Answer format ===

Respond with ONLY a single JSON object of the form {{"findings": [{{"severity": "high"|"medium"|"low", "file": "<path>", "line": <int>, "summary": "<what is wrong>", "failure_scenario": "<one sentence on why it matters>"}}]}} -- or {{"findings": []}} when there are none. No prose outside the JSON.
