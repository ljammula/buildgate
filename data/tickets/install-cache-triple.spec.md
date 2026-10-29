# Ticket: add triple to the install-cache fixture

This is an existing repo. Deliberately tiny: this ticket exists as the fixture
of `make live-install-cache` (see AGENTS.md's "Live validation" section), not
to ship a product feature. Target repo: the install-cache-node fixture
(`testdata/fixtures/install-cache-node`), a Node package whose one dependency,
`is-number`, its `sandbox:` install puts in `node_modules`.

## Goal

`index.js` declares `triple(value)`, which throws. Implement it as `double`
is implemented, and test it.

## Required changes

**`index.js`**:

1. `triple(value)` returns three times the number, using `is-number` as
   `double` does, and throws a `TypeError` for a value that is not a number.
2. Remove the "not yet implemented" comment and the throw.

**`triple.test.js`** (new file):

1. A `node:test` test of `triple` covering `2`, `'3'` and a non-number.

## Out of scope

- No change to `package.json`, `package-lock.json`, `.factory.yml` or
  `index.test.js`.
- No new dependency of any kind.

Allowed-Files: index.js, triple.test.js



## Acceptance / verification

Verify-Command: npm test

- `npm test` passes, including the new `triple` cases and the existing
  `double` test.

Success criterion: `triple(2) === 6`, verified by a real test.
