package main

import "buildgate/internal/workspace"

// The real round-ref and round-branch methods of the forge boundary. The refs
// live in the target repository's local .git and are never pushed.

func (impl realForge) updateRef(dir, ref, sha string) error {
	return workspace.UpdateRef(dir, ref, sha)
}
func (impl realForge) createBranchAt(dir, branch, sha string) error {
	return workspace.CreateBranchAt(dir, branch, sha)
}
