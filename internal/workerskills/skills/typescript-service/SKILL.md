---
name: typescript-service
description: TypeScript or JavaScript implementation in a buildgate build round. Use when the repository has package.json and the ticket changes a Node, Deno or Bun service, an npm package or a web frontend.
---

# TypeScript / JavaScript service (buildgate)


Read `package.json`, `tsconfig.json` (if present) and the repository's instructions before editing. Use its own `scripts` for format, lint, type-check and test.

- Add or update tests for the behaviour the ticket changes, and run them after each coherent edit.
- If `tsconfig.json` exists and the verify command named in the build prompt's checklist does not already type-check, run the repository's type-check on your change before finishing; passing tests do not catch type errors.
- Do not add dependencies, formatters or linters the repository has not installed; the sandbox has no general network access.
- The verify command named in the build prompt's checklist is the broad check; do not invent another.
