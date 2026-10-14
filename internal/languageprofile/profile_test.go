package languageprofile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectReturnsSingleLanguageProfileAndAdvisoryCommand(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Cargo.toml"), []byte("[package]\nname='sample'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Profile.ID != "rust" || matches[0].Profile.VerifyCommand != "cargo test --workspace" {
		t.Fatalf("matches = %+v", matches)
	}
}

func TestDetectReportsAllMatchesInsteadOfChoosing(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"Cargo.toml", "Gemfile"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	matches, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 || matches[0].Profile.ID != "ruby" || matches[1].Profile.ID != "rust" {
		t.Fatalf("ambiguous matches = %+v", matches)
	}
}

func TestDetectDoesNotFollowSymlinkedMarker(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "Cargo.toml")
	if err := os.WriteFile(target, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "Cargo.toml")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Detect(root); err == nil {
		t.Fatal("Detect followed symlinked marker")
	}
}

func TestCatalogReturnsIndependentCopies(t *testing.T) {
	catalog := Catalog()
	catalog[0].Markers[0] = "changed"
	catalog[0].VersionSources = append(catalog[0].VersionSources, "changed")
	catalog[0].TestPatterns = append(catalog[0].TestPatterns, "changed")
	if Catalog()[0].Markers[0] == "changed" || len(Catalog()[0].VersionSources) > 0 && Catalog()[0].VersionSources[len(Catalog()[0].VersionSources)-1] == "changed" || len(Catalog()[0].TestPatterns) > 0 && Catalog()[0].TestPatterns[len(Catalog()[0].TestPatterns)-1] == "changed" {
		t.Fatal("Catalog exposed mutable backing data")
	}
}

func TestCatalogDescribesAgentSetupContract(t *testing.T) {
	byID := map[string]Profile{}
	for _, profile := range Catalog() {
		byID[profile.ID] = profile
		if profile.ToolchainProvisioning == "" || profile.BaselineFailureFormat == "" {
			t.Errorf("%s is missing toolchain or result-reporting metadata: %+v", profile.ID, profile)
		}
		if len(profile.Markers) == 0 || len(profile.RequiredBinaries) == 0 {
			t.Errorf("%s is missing markers or binaries: %+v", profile.ID, profile)
		}
	}
	for _, id := range []string{"go", "python", "node", "rust", "ruby", "php-composer", "java-maven", "java-gradle", "dotnet", "dart"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("catalog is missing %q", id)
		}
	}
	if got := byID["dotnet"].ProjectImageManifests; len(got) != 0 {
		t.Errorf(".NET ProjectImageManifests = %v, want no unsupported helper manifests", got)
	}
	if got := byID["go"].RegistryProxyEcosystems; len(got) != 1 || got[0] != "Go modules" {
		t.Errorf("Go RegistryProxyEcosystems = %v, want Go modules", got)
	}
	if len(byID["python"].VerifyCommand) != 0 || len(byID["python"].FullSuiteCommand) != 0 {
		t.Errorf("Python command candidates = %q / %q, want no runner assumed from manifest markers", byID["python"].VerifyCommand, byID["python"].FullSuiteCommand)
	}
	if len(byID["rust"].TestPatterns) != 0 {
		t.Errorf("Rust TestPatterns = %v, want no potentially incomplete pattern suggestion", byID["rust"].TestPatterns)
	}
	if byID["go"].BaselineFailureFormat != BaselineGoTest || byID["python"].BaselineFailureFormat != BaselinePytest || byID["java-maven"].BaselineFailureFormat != BaselineUnparsed {
		t.Error("catalog does not match implemented baseline failure parsers")
	}
}
