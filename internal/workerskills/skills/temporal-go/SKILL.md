---
name: temporal-go
description: Temporal Go Workflow and Activity changes in a buildgate build round. Use when go.mod includes the Temporal SDK and the ticket changes workflow or activity code.
---

# Temporal Go (buildgate)


- Keep Workflow code deterministic; put side effects in Activities.
- Test with the Go SDK's test environment: behaviour, failures, cancellation, signals, retries and time skipping for what the ticket changes.
- If the repository keeps replay tests or recorded histories, run them after changing a Workflow definition and treat nondeterminism as a failure. Version any change that cannot replay against existing histories.
