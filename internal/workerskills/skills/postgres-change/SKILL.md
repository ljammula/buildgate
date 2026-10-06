---
name: postgres-change
description: PostgreSQL schema, migration, index or query changes in a buildgate build round. Use when the repository has PostgreSQL drivers and migration or schema files and the ticket touches them.
---

# PostgreSQL change (buildgate)


- When the round names a Postgres sidecar (`BG_SERVICE_*` variables), apply the migrations to it starting from the previous schema, and check the resulting constraints, indexes and the queries the ticket changes. Do not try to start a database yourself otherwise; rely on the repository's own tests.
- Use explicit transactions where PostgreSQL supports them.
- For long-running or backwards-incompatible changes, note the lock-duration and compatibility risk in your final message, with the expand/contract or forward-fix plan.
