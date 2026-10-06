package request

import "testing"

const validSpec = `# Spec

## Problem

Something is broken.

## Scope

Just this service.

## Non-goals

Not that.

## Affected services and packages

internal/foo

## Acceptance criteria

1. It works.

## Risks

None.

## Open questions

None.
`

func TestValidateSpecSkeletonAcceptsAValidSpec(t *testing.T) {
	if err := ValidateSpecSkeleton(validSpec); err != nil {
		t.Errorf("ValidateSpecSkeleton(valid) = %v, want nil", err)
	}
}

func TestValidateSpecSkeletonRejectsAMissingHeading(t *testing.T) {
	// Drops "## Non-goals" entirely.
	spec := `# Spec

## Problem

x

## Scope

x

## Affected services and packages

x

## Acceptance criteria

1. x

## Risks

x

## Open questions

x
`
	err := ValidateSpecSkeleton(spec)
	if err == nil {
		t.Fatal("ValidateSpecSkeleton(missing heading) = nil, want an error")
	}
	if got := err.Error(); !contains(got, "## Non-goals") {
		t.Errorf("error = %q, want it to name the missing heading %q", got, "## Non-goals")
	}
}

func TestValidateSpecSkeletonRejectsHeadingsOutOfOrder(t *testing.T) {
	// "## Scope" appears before "## Problem".
	spec := `# Spec

## Scope

x

## Problem

x

## Non-goals

x

## Affected services and packages

x

## Acceptance criteria

1. x

## Risks

x

## Open questions

x
`
	err := ValidateSpecSkeleton(spec)
	if err == nil {
		t.Fatal("ValidateSpecSkeleton(out of order) = nil, want an error")
	}
	// "## Problem" is found first (search starts from the top), which
	// advances the search position past "## Scope"'s own, earlier line --
	// so "## Scope" is what's reported missing, not "## Problem". See
	// ValidateSpecSkeleton's own doc comment on this wording.
	if got := err.Error(); !contains(got, "## Scope") {
		t.Errorf("error = %q, want it to name %q (the heading search has already moved past)", got, "## Scope")
	}
}

func TestValidateSpecSkeletonRejectsEmptyAcceptanceCriteria(t *testing.T) {
	spec := `# Spec

## Problem

x

## Scope

x

## Non-goals

x

## Affected services and packages

x

## Acceptance criteria

## Risks

x

## Open questions

x
`
	err := ValidateSpecSkeleton(spec)
	if err == nil {
		t.Fatal("ValidateSpecSkeleton(empty acceptance criteria) = nil, want an error")
	}
	if got := err.Error(); !contains(got, "Acceptance criteria") {
		t.Errorf("error = %q, want it to name the empty section", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
