A finding blocks the change only through a reproduction: a test, written out in the finding's "reproduction" object, that the factory runs itself on the result commit. You write no file in the workspace; the reproduction exists only in your answer. A finding without both a spec line and a reproduction is advice: it is listed for the reviewer of the pull request and refuses nothing.

Give "spec_line" as the number and the exact text of ONE numbered line of the approved spec above ("L<n>: <text>" is shown; give n and the text after the colon, byte for byte) that the finding says the change breaks. If no spec line states the behaviour, give none.

Give "reproduction" as {"path": "<new test file>", "content": "<the whole file>"}:
- The path is relative, has no `..`, matches the repository's test file naming, and is a file that does NOT exist in the repository yet. A reproduction at an existing path is advice.
- The file holds only tests for this finding, written in the repository's own test framework, importing only what the repository already has. Each test fails when the defect is present and passes when the behaviour the cited spec line states holds. A test that hangs or raises an exception counts as a failure; a test that passes counts as no defect.
- The test must fail because of the cited behaviour and nothing else: no network, clocks, randomness, ordering between tests or files outside the repository. Do not name a command, flag or timeout; the factory chooses them.
- At most 3 findings carry a reproduction, each file at most 32 KiB. Keep each as small as it can be.
