# Ticket: Implement RFC 7396 JSON Merge Patch

This is a new, greenfield proving-ground repo, not an existing one.

## Why this ticket exists

This ticket is a small greenfield live-smoke case with an independent,
pre-existing, externally-published conformance suite (RFC 7396's own
Appendix A test cases) that the Verify-Command runs.

## Goal

Target repo: the json-merge-patch-rfc7396 fixture
(`testdata/fixtures/json-merge-patch-rfc7396`) (a minimal Go
module: `merge.go` at the repo root, `package mergepatch`, one
unimplemented stub function; `verify/rfc7396_oracle_test.go`, a checked-in
conformance test -- NOT part of this ticket's scope, see below).

Implement RFC 7396 JSON Merge Patch
(https://www.rfc-editor.org/rfc/rfc7396) in `merge.go`:

```
func MergePatch(target, patch []byte) ([]byte, error)
```

Algorithm (RFC 7396 section 2, recursive):

```
define MergePatch(Target, Patch):
  if Patch is an Object:
    if Target is not an Object:
      Target = {}
    for each Name/Value pair in Patch:
      if Value is null:
        if Name exists in Target:
          remove the Name/Value pair from Target
      else:
        Target[Name] = MergePatch(Target[Name], Value)
    return Target
  else:
    return Patch
```

`target` and `patch` are both arbitrary JSON documents (not necessarily
objects -- either can be a string, number, array, object, or null).
Return the merged JSON document, marshaled back to `[]byte`.

## Required changes

**`merge.go`**:

1. Replace the `panic("not implemented")` stub with a real implementation
   of the algorithm above.
2. Handle every JSON value kind for both `target` and `patch`, not just
   objects -- the algorithm's `else: return Patch` branch is not a rare
   case, several of RFC 7396's own test cases exercise it directly (a
   patch that is itself a string, an array, or `null` entirely replaces
   the target).

**`merge_test.go`** (new file): add your own tests. These are informational
only for this ticket's `tests_added` gate -- the RFC conformance suite
under `verify/` is what determines whether this run is accepted. See
"Acceptance / verification" below.

## Out of scope

- `verify/rfc7396_oracle_test.go` and `.factory.yml` are both **out of
  scope and must not be touched**. `verify/rfc7396_oracle_test.go` in
  particular is the conformance check -- editing it (even to "fix" an
  apparent test failure) will be rejected by the `diff_scope` gate
  regardless.
- No new dependency of any kind -- `encoding/json` from the standard
  library is sufficient.
- No CLI, no HTTP layer, no `main` package -- `merge.go` is a library
  function only.

Allowed-Files: merge.go, merge_test.go

Required-Changed-Files: merge.go

Required-Content: encoding/json
<!-- required_content_present gates on text that NEWLY appears (base vs.
     final), not merely present in the final file -- "func MergePatch"
     was tried first and rejected every real implementation, because that
     signature is already in the unimplemented stub verbatim. "encoding/json"
     is not in the stub's import block (the stub has none at all), so it
     can only appear if a real implementation actually parses JSON. Found
     live on this ticket's first real run (quarantined on
     required_content_present despite a correct implementation and a
     passing conformance suite) -- keep this in mind for any future
     ticket whose stub already contains the function signature it's
     asking the agent to fill in. -->


## Acceptance / verification

Verify-Command: go build ./... && go vet ./... && go test ./...

- `go build ./...`, `go vet ./...`, and `go test ./...` all succeed. The
  last runs `verify/rfc7396_oracle_test.go` against your `MergePatch`,
  checking all 15 of RFC 7396 Appendix A's published test cases against
  their independently-published expected results. It is not agent-authored
  and cannot be satisfied by tests that assert against your own
  implementation's output -- only by an implementation that actually
  produces RFC 7396's specified results.
- Commit message subject should start with
  `ticket(json-merge-patch-rfc7396):`.

## Known limitation (accepted, not fixed by this ticket)

`verify/rfc7396_oracle_test.go` is checked into this repo and therefore
visible to you (the agent) during your own build phase. `diff_scope` stops
you from *editing* it, but nothing stops you from *reading* it and
special-casing its exact 15 inputs instead of implementing the general
algorithm. Do not do that -- implement the actual algorithm above.
