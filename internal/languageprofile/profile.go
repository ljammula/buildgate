// Package languageprofile provides reviewed, declarative language hints for
// repositories. Profiles help operators inspect and configure projects; they
// do not execute commands or decide whether a run passed.
package languageprofile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Profile is a compiled-in, data-only description of a project family.
// Commands are suggestions for a human-authored .factory.yml, never run by
// the profile resolver.
type Profile struct {
	ID               string
	Name             string
	Markers          []string
	RequiredBinaries []string
	VerifyCommand    string
	TestCommand      string
	LintCommand      string
}

// Match reports a profile and the exact marker files found at the repository
// root. Nested-workspace discovery is deliberately left to a later protocol.
type Match struct {
	Profile Profile
	Markers []string
}

var profiles = []Profile{
	{ID: "rust", Name: "Rust", Markers: []string{"Cargo.toml"}, RequiredBinaries: []string{"cargo", "rustc"}, VerifyCommand: "cargo test --workspace", TestCommand: "cargo test --workspace", LintCommand: "cargo clippy --workspace --all-targets -- -D warnings"},
	{ID: "ruby", Name: "Ruby", Markers: []string{"Gemfile"}, RequiredBinaries: []string{"ruby", "bundle"}},
	{ID: "php-composer", Name: "PHP / Composer", Markers: []string{"composer.json"}, RequiredBinaries: []string{"php", "composer"}},
	{ID: "java-maven", Name: "Java / Maven", Markers: []string{"pom.xml"}, RequiredBinaries: []string{"java", "mvn"}, VerifyCommand: "mvn test", TestCommand: "mvn test"},
	{ID: "java-gradle", Name: "Java / Gradle", Markers: []string{"build.gradle", "build.gradle.kts"}, RequiredBinaries: []string{"java", "gradle"}},
	{ID: "dotnet", Name: ".NET", Markers: []string{"*.sln", "*.csproj", "*.fsproj"}, RequiredBinaries: []string{"dotnet"}},
	{ID: "dart", Name: "Dart", Markers: []string{"pubspec.yaml"}, RequiredBinaries: []string{"dart"}},
}

// Catalog returns a copy of the profiles in stable ID order.
func Catalog() []Profile {
	out := make([]Profile, 0, len(profiles))
	for _, profile := range profiles {
		profile.Markers = append([]string(nil), profile.Markers...)
		profile.RequiredBinaries = append([]string(nil), profile.RequiredBinaries...)
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Detect returns every compiled profile whose root marker is present. It
// rejects marker symlinks and non-regular files rather than following them.
// Multiple matches are returned so the caller can report ambiguity instead
// of selecting one by precedence.
func Detect(root string) ([]Match, error) {
	var matches []Match
	for _, profile := range profiles {
		var found []string
		for _, marker := range profile.Markers {
			if filepath.Base(marker) != marker || marker == "." || marker == ".." {
				return nil, fmt.Errorf("profile %s has an invalid root marker %q", profile.ID, marker)
			}
			if marker[0] == '*' {
				entries, err := os.ReadDir(root)
				if err != nil && !os.IsNotExist(err) {
					return nil, fmt.Errorf("read project root for profile %s: %w", profile.ID, err)
				}
				pattern := marker
				for _, entry := range entries {
					ok, err := filepath.Match(pattern, entry.Name())
					if err != nil {
						return nil, fmt.Errorf("profile %s has invalid marker pattern %q: %w", profile.ID, marker, err)
					}
					if ok && entry.Type().IsRegular() {
						found = append(found, entry.Name())
					}
				}
				continue
			}
			path := filepath.Join(root, marker)
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("inspect project marker %s: %w", marker, err)
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("project marker %s is not a regular file", marker)
			}
			found = append(found, marker)
		}
		if len(found) > 0 {
			sort.Strings(found)
			matches = append(matches, Match{Profile: profile, Markers: found})
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Profile.ID < matches[j].Profile.ID })
	return matches, nil
}
