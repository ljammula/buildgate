package main

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"buildgate/internal/languageprofile"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
)

const testPatternWalkLimit = 20000

var skippedWalkDirs = map[string]bool{"node_modules": true, "vendor": true, "dist": true, "build": true, "target": true}

// deriveTestPatterns returns patterns followed by the test directories
// (<name>/, one of testDirs) that hold files of the patterns' languages which
// patterns miss. No patterns or no testDirs derives nothing.
func deriveTestPatterns(root string, profilePatterns, testDirs []string) []string {
	if len(profilePatterns) == 0 || len(testDirs) == 0 {
		return profilePatterns
	}
	testDirNames := map[string]bool{}
	for _, d := range testDirs {
		testDirNames[d] = true
	}
	exts := map[string]bool{}
	for _, p := range profilePatterns {
		if e := path.Ext(p); e != "" && !strings.HasSuffix(p, "/") {
			exts[e] = true
		}
	}
	result := append([]string(nil), profilePatterns...)
	seen := map[string]bool{}
	for _, p := range result {
		seen[p] = true
	}
	files := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if p != root && (strings.HasPrefix(name, ".") || skippedWalkDirs[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		if files++; files > testPatternWalkLimit {
			return filepath.SkipAll
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil || !exts[path.Ext(d.Name())] {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if policy.IsTestFile(rel, profilePatterns) {
			return nil
		}
		for _, seg := range strings.Split(path.Dir(rel), "/") {
			if testDirNames[seg] && !seen[seg+"/"] {
				seen[seg+"/"] = true
				result = append(result, seg+"/")
			}
		}
		return nil
	})
	return result
}

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

// doctorCheckLanguageProfile reports the best matching root-level profile
// and a candidate .factory.yml block. The profile is advisory: it neither
// writes the repository nor changes the commands or gates a run uses.
func doctorCheckLanguageProfile(root string) doctorCheck {
	matches, err := languageprofile.Detect(root)
	if err != nil {
		return doctorCheck{Name: "language profile for " + root, Err: err, Advisory: true,
			Fix: "inspect the named marker and pass the repository or module root that contains its real manifest"}
	}
	if len(matches) == 0 {
		return doctorCheck{Name: "language profile for " + root,
			Err: fmt.Errorf("no built-in root-level language profile matched"), Advisory: true,
			Fix: "choose an operator-approved worker image, then commit an explicit verify_command and required_binaries in .factory.yml; add test_patterns when the test naming differs from the defaults"}
	}
	if len(matches) > 1 {
		var candidates []string
		for _, match := range matches {
			candidates = append(candidates, match.Profile.ID+" ("+strings.Join(match.Markers, ", ")+")")
		}
		return doctorCheck{Name: "language profile for " + root,
			Err:      fmt.Errorf("multiple root-level profiles matched: %s", strings.Join(candidates, "; ")),
			Advisory: true,
			Fix:      "choose the module/profile whose commands describe this workspace; detection is root-level only and does not resolve nested or multi-module layouts"}
	}
	profile := matches[0].Profile
	loadedConfig, found, configErr := projectconfig.Load(root)
	if configErr != nil {
		return doctorCheck{Name: "language profile " + profile.Name + " for " + root, Err: configErr,
			Fix: "fix the repository's .factory.yml before using its suggestions"}
	}
	var config projectconfig.Config
	if found && loadedConfig != nil {
		config = *loadedConfig
	}
	profile.TestPatterns = deriveTestPatterns(root, profile.TestPatterns, profile.TestDirectories)
	name := "language profile " + profile.Name + " for " + root
	detail := profileDoctorDetail(profile, matches[0].Markers, config)
	if profile.VerifyCommand != "" && config.VerifyCommand == "" {
		return doctorCheck{Name: name, Err: fmt.Errorf(".factory.yml has no verify_command; the profile has a reviewed candidate"), Advisory: true,
			Fix: "profile metadata: " + detail + "\n" + profileFactoryYMLFix(root, profile, config) + "; review the command and commit .factory.yml"}
	}
	missing := missingProfileBinaries(profile.RequiredBinaries, config.RequiredBinaries)
	if len(missing) > 0 {
		return doctorCheck{Name: name, Err: fmt.Errorf(".factory.yml required_binaries does not name: %s", strings.Join(missing, ", ")), Advisory: true,
			Fix: "profile metadata: " + detail + "\n" + profileFactoryYMLFix(root, profile, config) + "; review the fields and commit .factory.yml so each run checks these executables in its final image"}
	}
	if check, missed := doctorCheckCommittedTestPatterns(root, name, config.TestPatterns, profile.TestDirectories); missed {
		return check
	}
	if !found {
		return doctorCheck{Name: name, Err: fmt.Errorf("no committed .factory.yml describes this project"), Advisory: true,
			Fix: "profile metadata: " + detail + "\n" + profileFactoryYMLFix(root, profile, config) + "; review and commit .factory.yml"}
	}
	return doctorCheck{Name: name, Detail: detail}
}

// doctorCheckCommittedTestPatterns reports a committed test_patterns that
// misses test files under the repository's own test directories: the
// tests_added gate and plan drafting would not count those files as tests.
func doctorCheckCommittedTestPatterns(root, name string, committed, testDirs []string) (doctorCheck, bool) {
	if len(committed) == 0 {
		return doctorCheck{}, false
	}
	missed := deriveTestPatterns(root, committed, testDirs)[len(committed):]
	if len(missed) == 0 {
		return doctorCheck{}, false
	}
	return doctorCheck{Name: name, Err: fmt.Errorf(".factory.yml test_patterns match no file under the test directories %s", strings.Join(missed, ", ")), Advisory: true,
		Fix: "add [" + quotedYAMLList(missed) + "] to test_patterns and commit .factory.yml, so the tests_added gate and plan drafting count the repository's own test files"}, true
}

func profileDoctorDetail(profile languageprofile.Profile, markers []string, config projectconfig.Config) string {
	verify := config.VerifyCommand
	if verify == "" {
		verify = profile.VerifyCommand
	}
	if verify == "" {
		verify = "not suggested; set verify_command explicitly"
	}
	fullSuite := config.FullSuiteCommand
	if fullSuite == "" && config.VerifyCommand != "" {
		fullSuite = "verify_command fallback (" + config.VerifyCommand + ")"
	} else if fullSuite == "" {
		fullSuite = profile.FullSuiteCommand
	}
	if fullSuite == "" && verify != "not suggested; set verify_command explicitly" {
		fullSuite = "verify_command (default)"
	} else if fullSuite == "" {
		fullSuite = "not configured"
	} else if fullSuite == verify {
		fullSuite = "verify_command (same command)"
	}
	testPatterns := profile.TestPatterns
	if len(config.TestPatterns) > 0 {
		testPatterns = config.TestPatterns
	}
	parts := []string{
		"working-tree root markers: " + strings.Join(markers, ", "),
		"verify command: " + strconv.Quote(verify),
		"full-suite command: " + strconv.Quote(fullSuite),
		"required_binaries: " + strings.Join(profile.RequiredBinaries, ", "),
		"test_patterns: " + strings.Join(testPatterns, ", "),
		"version sources to inspect: " + strings.Join(profile.VersionSources, ", "),
		"toolchain: " + profile.ToolchainProvisioning,
		"dependency manifests: " + strings.Join(profile.DependencyManifests, ", "),
		"project-sandbox-image supports: " + profileSupportList(profile.ProjectImageManifests),
		"registry proxy ecosystems: " + profileEcosystemList(profile.RegistryProxyEcosystems),
		"baseline named-failure parser: " + profile.BaselineFailureFormat,
	}
	return strings.Join(parts, "; ")
}

func profileFactoryYMLFix(root string, profile languageprofile.Profile, config projectconfig.Config) string {
	var lines []string
	if config.VerifyCommand == "" && profile.VerifyCommand != "" {
		lines = append(lines, "verify_command: "+strconv.Quote(profile.VerifyCommand))
	} else if config.VerifyCommand == "" {
		lines = append(lines, "# verify_command: choose the repository's real verification command")
	}
	specPath, contractPath, architecturePath := projectBootstrapArtifactPaths(root)
	if config.PreflightProfile == "" && onboardingNotDone(specPath, contractPath, architecturePath) {
		lines = append(lines, "preflight_profile: "+projectconfig.PreflightProfileBrownfield)
	}
	if config.FullSuiteCommand == "" && profile.FullSuiteCommand != "" && (config.VerifyCommand == "" || config.VerifyCommand == profile.VerifyCommand) {
		lines = append(lines, "full_suite_command: "+strconv.Quote(profile.FullSuiteCommand))
	}
	if missing := missingProfileBinaries(profile.RequiredBinaries, config.RequiredBinaries); len(missing) > 0 {
		lines = append(lines, "required_binaries: ["+quotedYAMLList(mergeProfileBinaries(config.RequiredBinaries, profile.RequiredBinaries))+"]")
	}
	if len(config.TestPatterns) == 0 && len(profile.TestPatterns) > 0 {
		lines = append(lines, "test_patterns: ["+quotedYAMLList(profile.TestPatterns)+"]")
	}
	if len(lines) == 0 {
		return "choose and commit .factory.yml values for verify_command, required_binaries, and test_patterns"
	}
	return "candidate .factory.yml fields (review before commit):\n" + strings.Join(lines, "\n")
}

func mergeProfileBinaries(configured, required []string) []string {
	merged := append([]string(nil), configured...)
	merged = append(merged, required...)
	return uniqueProfileBinaries(merged)
}

func missingProfileBinaries(required, configured []string) []string {
	set := make(map[string]bool, len(configured))
	for _, name := range configured {
		set[name] = true
	}
	var missing []string
	for _, name := range required {
		if !set[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

func quotedYAMLList(values []string) string {
	quoted := make([]string, len(values))
	for i, value := range values {
		quoted[i] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}

func profileSupportList(items []string) string {
	if len(items) == 0 {
		return "none; use an operator-built worker image"
	}
	return strings.Join(items, ", ")
}

func profileEcosystemList(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
