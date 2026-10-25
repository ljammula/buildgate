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
	ID                      string
	Name                    string
	Markers                 []string
	RequiredBinaries        []string
	VersionSources          []string
	ToolchainProvisioning   string
	DependencyManifests     []string
	ProjectImageManifests   []string
	RegistryProxyEcosystems []string
	VerifyCommand           string
	FullSuiteCommand        string
	TestCommand             string
	LintCommand             string
	// InstallCommand is the reviewed candidate for a `.factory.yml` sandbox
	// block's install, with CachePaths the paths it writes. InstallRequires
	// is the root file that must be present for the candidate to apply (a
	// lockfile). Empty InstallCommand means no candidate.
	InstallCommand        string
	CachePaths            []string
	InstallRequires       string
	TestPatterns          []string
	BaselineFailureFormat string
}

const (
	ToolchainAutomatic       = "automatic"
	ToolchainConfiguredImage = "configured-image"
	ToolchainImageOnly       = "image-only"
	BaselineGoTest           = "go-test"
	BaselinePytest           = "pytest"
	BaselineUnparsed         = "unparsed"
)

// Match reports a profile and the exact marker files found at the repository
// root. Nested-workspace discovery is deliberately left to a later protocol.
type Match struct {
	Profile Profile
	Markers []string
}

var profiles = []Profile{
	{ID: "go", Name: "Go", Markers: []string{"go.mod"}, RequiredBinaries: []string{"go"}, VersionSources: []string{"go.mod"}, ToolchainProvisioning: ToolchainAutomatic, DependencyManifests: []string{"go.mod", "go.sum"}, ProjectImageManifests: []string{"go.mod", "go.sum"}, RegistryProxyEcosystems: []string{"Go modules"}, VerifyCommand: "go build ./... && go test -run '^$' ./...", FullSuiteCommand: "go test ./...", TestCommand: "go test ./...", TestPatterns: []string{"*_test.go"}, BaselineFailureFormat: BaselineGoTest},
	{ID: "python", Name: "Python", Markers: []string{"pyproject.toml", "requirements.txt", "setup.py"}, RequiredBinaries: []string{"python3"}, VersionSources: []string{".python-version", "pyproject.toml"}, ToolchainProvisioning: ToolchainAutomatic, InstallCommand: "python3 -m venv .venv && .venv/bin/pip install -r requirements.txt", CachePaths: []string{".venv"}, InstallRequires: "requirements.txt", DependencyManifests: []string{"pyproject.toml", "requirements.txt", "poetry.lock", "uv.lock"}, ProjectImageManifests: []string{"requirements.txt"}, RegistryProxyEcosystems: []string{"PyPI"}, TestPatterns: []string{"test_*.py", "*_test.py"}, BaselineFailureFormat: BaselinePytest},
	{ID: "node", Name: "Node.js", Markers: []string{"package.json"}, RequiredBinaries: []string{"node", "npm"}, VersionSources: []string{".nvmrc", ".node-version", "package.json"}, ToolchainProvisioning: ToolchainConfiguredImage, InstallCommand: "npm ci", CachePaths: []string{"node_modules"}, InstallRequires: "package-lock.json", DependencyManifests: []string{"package.json", "package-lock.json", "npm-shrinkwrap.json"}, ProjectImageManifests: []string{"package.json", "package-lock.json"}, RegistryProxyEcosystems: []string{"npm"}, TestPatterns: []string{"*.test.js", "*.test.ts", "*.test.tsx", "*.spec.js", "*.spec.ts", "*.spec.tsx"}, BaselineFailureFormat: BaselineUnparsed},
	{ID: "rust", Name: "Rust", Markers: []string{"Cargo.toml"}, RequiredBinaries: []string{"cargo", "rustc"}, VersionSources: []string{"rust-toolchain", "rust-toolchain.toml"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"Cargo.toml", "Cargo.lock"}, VerifyCommand: "cargo test --workspace", FullSuiteCommand: "cargo test --workspace", TestCommand: "cargo test --workspace", LintCommand: "cargo clippy --workspace --all-targets -- -D warnings", BaselineFailureFormat: BaselineUnparsed},
	{ID: "ruby", Name: "Ruby", Markers: []string{"Gemfile"}, RequiredBinaries: []string{"ruby", "bundle"}, VersionSources: []string{".ruby-version", ".tool-versions"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"Gemfile", "Gemfile.lock"}, ProjectImageManifests: []string{"Gemfile", "Gemfile.lock"}, RegistryProxyEcosystems: []string{"RubyGems"}, TestPatterns: []string{"*_spec.rb", "*_test.rb"}, BaselineFailureFormat: BaselineUnparsed},
	{ID: "php-composer", Name: "PHP / Composer", Markers: []string{"composer.json"}, RequiredBinaries: []string{"php", "composer"}, VersionSources: []string{".php-version", "composer.json"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"composer.json", "composer.lock"}, TestPatterns: []string{"test_*.php", "*Test.php"}, BaselineFailureFormat: BaselineUnparsed},
	{ID: "java-maven", Name: "Java / Maven", Markers: []string{"pom.xml"}, RequiredBinaries: []string{"java", "mvn"}, VersionSources: []string{"pom.xml", ".java-version"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"pom.xml"}, ProjectImageManifests: []string{"pom.xml"}, RegistryProxyEcosystems: []string{"Maven Central"}, VerifyCommand: "mvn test", FullSuiteCommand: "mvn test", TestCommand: "mvn test", TestPatterns: []string{"*Test.java"}, BaselineFailureFormat: BaselineUnparsed},
	{ID: "java-gradle", Name: "Java / Gradle", Markers: []string{"build.gradle", "build.gradle.kts"}, RequiredBinaries: []string{"java", "gradle"}, VersionSources: []string{"gradle/wrapper/gradle-wrapper.properties", "build.gradle", "build.gradle.kts", ".java-version"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"build.gradle", "build.gradle.kts", "gradle.lockfile"}, TestPatterns: []string{"*Test.java"}, BaselineFailureFormat: BaselineUnparsed},
	{ID: "dotnet", Name: ".NET", Markers: []string{"*.sln", "*.csproj", "*.fsproj"}, RequiredBinaries: []string{"dotnet"}, VersionSources: []string{"global.json", "*.csproj", "*.fsproj"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"*.csproj", "*.fsproj", "packages.lock.json"}, TestPatterns: []string{"*Test.cs", "*Tests.cs"}, BaselineFailureFormat: BaselineUnparsed},
	{ID: "dart", Name: "Dart", Markers: []string{"pubspec.yaml"}, RequiredBinaries: []string{"dart"}, VersionSources: []string{".tool-versions", "pubspec.yaml"}, ToolchainProvisioning: ToolchainImageOnly, DependencyManifests: []string{"pubspec.yaml", "pubspec.lock"}, TestPatterns: []string{"*_test.dart"}, BaselineFailureFormat: BaselineUnparsed},
}

// Catalog returns a copy of the profiles in stable ID order.
func Catalog() []Profile {
	out := make([]Profile, 0, len(profiles))
	for _, profile := range profiles {
		profile.Markers = append([]string(nil), profile.Markers...)
		profile.RequiredBinaries = append([]string(nil), profile.RequiredBinaries...)
		profile.VersionSources = append([]string(nil), profile.VersionSources...)
		profile.DependencyManifests = append([]string(nil), profile.DependencyManifests...)
		profile.ProjectImageManifests = append([]string(nil), profile.ProjectImageManifests...)
		profile.RegistryProxyEcosystems = append([]string(nil), profile.RegistryProxyEcosystems...)
		profile.CachePaths = append([]string(nil), profile.CachePaths...)
		profile.TestPatterns = append([]string(nil), profile.TestPatterns...)
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
