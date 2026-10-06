package evidence

import "testing"

func TestAdditionalLockfileDiffs(t *testing.T) {
	tests := []struct {
		name   string
		diff   func([]byte, []byte) ([]DependencyChange, error)
		base   []byte
		result []byte
		want   []struct{ name, base, result string }
	}{
		{"yarn classic", YarnLockDependencyDiff, []byte(`"foo@^1.0.0", "foo@~1.0.0":
  version "1.0.0"
  resolved "https://registry/foo-1.0.0.tgz"
`), []byte(`"foo@^1.0.0", "foo@~1.0.0":
  version "1.1.0"
  resolved "https://registry/foo-1.1.0.tgz"
`), []struct{ name, base, result string }{{"foo", "1.0.0", "1.1.0"}}},
		{"yarn berry", YarnLockDependencyDiff, []byte(`__metadata:
  version: 6
"foo@npm:^1.0.0":
  version: 1.0.0
  resolution: "foo@npm:1.0.0"
`), []byte(`__metadata:
  version: 6
"foo@npm:^1.0.0":
  version: 1.1.0
  resolution: "foo@npm:1.1.0"
`), []struct{ name, base, result string }{{"foo", "1.0.0", "1.1.0"}}},
		{"pnpm scoped peer", PnpmLockDependencyDiff, []byte(`lockfileVersion: '9.0'
packages:
  "@scope/foo@1.0.0_peer@2.0.0":
    resolution:
      integrity: sha512-old
snapshots:
  "@scope/foo@1.0.0_peer@2.0.0": {}
`), []byte(`lockfileVersion: '9.0'
packages:
  "@scope/foo@1.1.0_peer@2.0.0":
    resolution:
      integrity: sha512-new
snapshots:
  "@scope/foo@1.1.0_peer@2.0.0": {}
`), []struct{ name, base, result string }{{"@scope/foo", "", "1.1.0_peer@2.0.0"}, {"@scope/foo", "1.0.0_peer@2.0.0", ""}}},
		{"Gemfile", GemfileLockDependencyDiff, []byte(`GEM
  remote: https://rubygems.org/
  specs:
    rack (3.0.0)
`), []byte(`GEM
  remote: https://rubygems.org/
  specs:
    rack (3.1.0)
	`), []struct{ name, base, result string }{{"rack", "3.0.0", "3.1.0"}}},
		{"poetry", PoetryLockDependencyDiff, []byte(`[[package]]
name = "foo"
version = "1.0.0"
`), []byte(`[[package]]
name = "foo"
version = "1.1.0"
		`), []struct{ name, base, result string }{{"foo", "", "1.1.0"}, {"foo", "1.0.0", ""}}},
		{"cargo", CargoLockDependencyDiff, []byte(`version = 3
[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://example"
checksum = "old"
`), []byte(`version = 3
[[package]]
name = "foo"
version = "1.1.0"
source = "registry+https://example"
checksum = "new"
`), []struct{ name, base, result string }{{"foo", "", "1.1.0"}, {"foo", "1.0.0", ""}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.diff(tc.base, tc.result)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("changes = %+v, want %+v", got, tc.want)
			}
			for i, want := range tc.want {
				if got[i].Name != want.name || got[i].Base != want.base || got[i].Result != want.result {
					t.Errorf("changes[%d] = %+v, want %s %s -> %s", i, got[i], want.name, want.base, want.result)
				}
			}
		})
	}
}

func TestAdditionalLockfileDiffsRejectMalformed(t *testing.T) {
	tests := []struct {
		name      string
		diff      func([]byte, []byte) ([]DependencyChange, error)
		malformed []byte
	}{
		{"yarn", YarnLockDependencyDiff, []byte("foo:\n  version\n")},
		{"pnpm", PnpmLockDependencyDiff, []byte("packages: [broken")},
		{"Gemfile", GemfileLockDependencyDiff, []byte("GEM\n  specs:\n    broken")},
		{"poetry", PoetryLockDependencyDiff, []byte("[[package]]\nname = \"foo\"\n")},
		{"cargo", CargoLockDependencyDiff, []byte("[[package]]\nversion = \"1.0\"\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.diff(tc.malformed, nil); err == nil {
				t.Fatal("malformed lockfile accepted")
			}
		})
	}
}

func TestYarnBerryDescriptorSetsAndSingleDocument(t *testing.T) {
	base := []byte(`__metadata:
  version: 6
"@scope/foo@npm:^1.0.0, @scope/foo@npm:~1.0.0":
  version: 1.0.0
  resolution: "@scope/foo@npm:1.0.0"
`)
	result := []byte(`__metadata:
  version: 6
"@scope/foo@npm:~1.0.0, @scope/foo@npm:^1.0.0":
  version: 1.1.0
  resolution: "@scope/foo@npm:1.1.0"
`)
	got, err := YarnLockDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("YarnLockDependencyDiff: %v", err)
	}
	if len(got) != 1 || got[0].Name != "@scope/foo" || got[0].Base != "1.0.0" || got[0].Result != "1.1.0" {
		t.Fatalf("changes = %+v, want one normalized scoped descriptor update", got)
	}
	if _, err := YarnLockDependencyDiff(base, append(result, []byte("---\nother: value\n")...)); err == nil {
		t.Fatal("Yarn Berry trailing document accepted")
	}
	if _, err := YarnLockDependencyDiff([]byte(`__metadata:
  version: 6
"foo@npm:^1.0.0, bar@npm:^1.0.0":
  version: 1.0.0
`), base); err == nil {
		t.Fatal("Yarn Berry descriptor set mixing package names accepted")
	}
}

// TestYarnDescriptorNameSplitsOnFirstAt is the regression test for a real
// finding from adversarial review (2026-09-01): yarnDescriptorName split
// on the LAST '@' in a descriptor, which mis-parses an npm-alias
// descriptor that legitimately embeds a second '@' in its range half.
func TestYarnDescriptorNameSplitsOnFirstAt(t *testing.T) {
	tests := []struct {
		descriptor string
		want       string
	}{
		{"foo@^1.0.0", "foo"},
		{"@scope/foo@^1.0.0", "@scope/foo"},
		{"my-alias@npm:real-package@^1.0.0", "my-alias"},
		{"@scope/name@npm:@scope/other@^1.0.0", "@scope/name"},
	}
	for _, tc := range tests {
		got, err := yarnDescriptorName(tc.descriptor)
		if err != nil {
			t.Errorf("yarnDescriptorName(%q): %v", tc.descriptor, err)
			continue
		}
		if got != tc.want {
			t.Errorf("yarnDescriptorName(%q) = %q, want %q", tc.descriptor, got, tc.want)
		}
	}
}

func TestPnpmSectionsMergeRegardlessOfDocumentOrderAndRejectUnsupported(t *testing.T) {
	base := []byte(`lockfileVersion: '9.0'
snapshots:
  foo@1.0.0: {}
packages:
  foo@1.0.0:
    resolution:
      integrity: sha512-old
`)
	result := []byte(`lockfileVersion: '9.0'
packages:
  foo@1.0.0:
    resolution:
      integrity: sha512-new
snapshots:
  foo@1.0.0: {}
`)
	got, err := PnpmLockDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("PnpmLockDependencyDiff: %v", err)
	}
	if len(got) != 1 || got[0].Name != "foo" || got[0].Base != "1.0.0" || got[0].Result != "1.0.0" || got[0].BaseSourceReference == got[0].ResultSourceReference {
		t.Fatalf("changes = %+v, want one source-only change using packages resolution", got)
	}
	if _, err := PnpmLockDependencyDiff([]byte("lockfileVersion: '9.0'\nmetadata: {}\n"), []byte("")); err == nil {
		t.Fatal("unsupported pnpm document accepted")
	}
	if _, err := PnpmLockDependencyDiff(base, append(result, []byte("---\nother: value\n")...)); err == nil {
		t.Fatal("pnpm trailing document accepted")
	}
}

func TestGemfileSourcesAndTOMLDuplicateVersions(t *testing.T) {
	gemBase := []byte(`GIT
  remote: https://example.test/repo.git
  revision: old
  specs:
    foo (1.0.0)
PATH
  remote: ../local
  specs:
    foo (1.0.0)
`)
	gemResult := []byte(`GIT
  remote: https://example.test/repo.git
  revision: new
  specs:
    foo (1.0.0)
PATH
  remote: ../local
  specs:
    foo (1.0.0)
`)
	changes, err := GemfileLockDependencyDiff(gemBase, gemResult)
	if err != nil {
		t.Fatalf("GemfileLockDependencyDiff: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want lossless GIT revision removal/addition with PATH duplicate preserved", changes)
	}
	if changes[0].Base != "" || changes[0].Result != "1.0.0" || changes[1].Base != "1.0.0" || changes[1].Result != "" {
		t.Fatalf("changes = %+v, want deterministic GIT revision removal/addition", changes)
	}
	if changes[0].ResultSourceReference == changes[1].BaseSourceReference {
		t.Fatalf("changes = %+v, want distinct GIT revisions", changes)
	}

	toml := []byte(`[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://example"
[[package]]
name = "foo"
version = "2.0.0"
source = "registry+https://example"
`)
	for name, diff := range map[string]func([]byte, []byte) ([]DependencyChange, error){"Cargo": CargoLockDependencyDiff, "Poetry": PoetryLockDependencyDiff} {
		got, err := diff(toml, toml)
		if err != nil {
			t.Fatalf("%s duplicate versions: %v", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s duplicate versions unchanged produced %+v", name, got)
		}
		oneVersion := []byte(`[[package]]
name = "foo"
version = "2.0.0"
source = "registry+https://example"
`)
		got, err = diff(toml, oneVersion)
		if err != nil || len(got) != 1 || got[0].Base != "1.0.0" || got[0].Result != "" {
			t.Errorf("%s duplicate membership change = %+v, %v; want one 1.0.0 removal", name, got, err)
		}
	}
}

func TestTOMLIntegrityEvidenceAndConflictingDuplicate(t *testing.T) {
	base := []byte(`[[package]]
name = "foo"
version = "1.0.0"
source = { type = "registry", url = "https://example" }
files = [{file = "foo.whl", hash = "sha256:old"}]
`)
	result := []byte(`[[package]]
name = "foo"
version = "1.0.0"
source = { url = "https://example", type = "registry" }
files = [{hash = "sha256:new", file = "foo.whl"}]
`)
	got, err := PoetryLockDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("PoetryLockDependencyDiff: %v", err)
	}
	if len(got) != 1 || got[0].Base != "1.0.0" || got[0].Result != "1.0.0" || got[0].BaseSourceReference == got[0].ResultSourceReference {
		t.Fatalf("changes = %+v, want Poetry files integrity evidence", got)
	}
	cargoBase := []byte(`[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://example"
checksum = "old"
`)
	cargoResult := []byte(`[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://example"
checksum = "new"
`)
	got, err = CargoLockDependencyDiff(cargoBase, cargoResult)
	if err != nil || len(got) != 1 || got[0].Base != "1.0.0" || got[0].Result != "1.0.0" || got[0].BaseSourceReference == got[0].ResultSourceReference {
		t.Fatalf("Cargo checksum-only changes = %+v, %v; want one source-evidence update", got, err)
	}
	conflict := append(append([]byte{}, base...), []byte(`
[[package]]
name = "foo"
version = "1.0.0"
source = { type = "registry", url = "https://example" }
files = [{file = "other.whl", hash = "sha256:other"}]
`)...)
	if _, err := PoetryLockDependencyDiff(conflict, base); err == nil {
		t.Fatal("conflicting duplicate Poetry identity accepted")
	}
}
