Independent combined review: two separate tasks over the SAME diff, in one turn -- task A is a spec-conformity review, task B is a free-form code review. Answer both.

{diff_block}

--- ticket spec (context for task B; task A's own criteria are given in task A below) ---
{spec_text}
--- end ticket spec ---

=== Task A: spec-conformity review ===

Check the diff above against each of the following approved acceptance criteria, in order.

{criteria}

{conformity_command_outcome_rule}

{conformity_formatting_rule}

=== Task B: code review ===

{scope_rule}

{severity_rule}

{command_outcome_rule}

=== Answer format ===

Respond with ONLY a single JSON object of the form {{"criteria": [{{"criterion": "<criterion text, verbatim>", "verdict": "clean"|"flagged", "detail": "<why, if flagged>"}}], "findings": [{{"severity": "high"|"medium"|"low", "file": "<path>", "line": <int>, "summary": "<what is wrong>", "failure_scenario": "<inputs/state that trigger it and the wrong result>"}}]}} -- exactly one criteria entry per acceptance criterion above, in the same order, using each criterion's exact text (task A); findings may be an empty list when there are none (task B). No prose outside the JSON.
