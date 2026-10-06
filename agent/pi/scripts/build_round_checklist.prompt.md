Before you end your turn:
1. Run the narrowest tests that cover your change and read their output; fix and rerun until they pass. You do not need to run the full command yourself: after your turn the harness runs {verify} and decides the round on it. If a test cannot run in this sandbox, say so in one line.
2. If the ticket lists `Required-Changed-Files`, change every one of them; if it lists `Allowed-Files`, change nothing outside them.
3. Unless the ticket says `Tests-Required: no`, add or change a test that exercises the new behaviour.
4. For each acceptance criterion the ticket covers, name the test or code that shows it holds.
5. Never edit read-only reference-oracle tests; when one fails, fix the code it tests.
6. Committing is optional (anything left uncommitted is committed for you), but never rewrite history: no reset, rebase or amend of existing commits, no push, no branch switch.
