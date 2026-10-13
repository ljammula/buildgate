package main

import "buildgate/internal/languageprofile"

// suggestedProfileVerifyCommand is used only by onboarding/scaffolding when
// creating a new .factory.yml. An ambiguous or malformed project receives no
// suggestion; no language profile is consulted by the run acceptance path.
func suggestedProfileVerifyCommand(root string) (command, source string) {
	matches, err := languageprofile.Detect(root)
	if err != nil || len(matches) != 1 {
		return "", ""
	}
	profile := matches[0].Profile
	if profile.VerifyCommand == "" {
		return "", ""
	}
	return profile.VerifyCommand, "profile " + profile.ID
}
