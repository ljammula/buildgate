# AGENTS.md

A Node package with one dependency, `is-number` (`package.json`,
`package-lock.json`). It is the fixture of buildgate's
`make live-install-cache`.

## Commands

- Install: `npm ci --ignore-scripts` (the `.factory.yml` `sandbox:` block;
  buildgate runs it once and restores `node_modules` into each step).
- Test: `npm test` (`node --test`).

Do not commit `node_modules` or change the dependency.
