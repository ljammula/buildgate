Request data directories as v0.0.13 wrote them, one per state a request can be in
at an upgrade. Each was produced by running v0.0.13's own fixtures (its
`requestdrivertest` helpers, the request and run stores) against a checkout of the
tag and copying the data directory out.

`@DATA@` stands for the data directory and `@WORKSPACE@` for the repository the
request names; a test replaces both with its own temporary directories before
loading. Do not regenerate these with a newer build: the point is that the files
are what the earlier release wrote.
