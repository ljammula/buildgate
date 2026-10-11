// Package release decides whether an accepted run may be offered for release
// and records that decision: the merge policy check (MergePolicyCheck), the
// durable Decision, the per-project kill switch, and the rollback of a run's
// worktree and branch. It performs no merge and no deploy: both stay human
// actions (safety-contract.md, SC-001), and a Decision must never be wired to
// either.
package release
