package evidence

import (
	"testing"
)

func TestPackageLockDependencyDiff(t *testing.T) {
	base := []byte(`{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.0.0"},"node_modules/b":{"name":"b","version":"2.0.0"}}}`)
	result := []byte(`{"packages":{"":{"name":"app","version":"1.0.0"},"node_modules/a":{"name":"a","version":"1.1.0"},"node_modules/c":{"name":"c","version":"3.0.0"}}}`)
	got, err := PackageLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{Name: "a", Base: "1.0.0", Result: "1.1.0"}, {Name: "b", Base: "2.0.0"}, {Name: "c", Result: "3.0.0"}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestPackageLockDependencyDiffKeysByPathNotName is the regression test for
// a real P2 finding from codex review: an earlier version keyed the parsed
// version map by package name alone, collapsing multiple installed copies
// of the same name at different paths (npm's own nested-dependency
// resolution, not a malformed lockfile) into one entry via Go's unordered
// map iteration — non-deterministically selecting which copy survived.
// Two paths both named "a": one path's version is genuinely unchanged
// between base and result, the other path is a new addition. A name-keyed
// diff could report a false change on the unchanged path or miss the real
// addition depending on iteration order; a path-keyed diff must report
// both changes correctly and deterministically every time.
func TestPackageLockDependencyDiffKeysByPathNotName(t *testing.T) {
	base := []byte(`{"packages":{
		"": {"name":"app","version":"1.0.0"},
		"node_modules/a": {"name":"a","version":"1.0.0"}
	}}`)
	result := []byte(`{"packages":{
		"": {"name":"app","version":"1.0.0"},
		"node_modules/a": {"name":"a","version":"1.0.0"},
		"node_modules/nested/a": {"name":"a","version":"2.0.0"}
	}}`)
	// Run many times: a name-keyed implementation's bug depended on Go's
	// randomized map iteration order, so a single run could pass by luck.
	for i := 0; i < 20; i++ {
		got, err := PackageLockDependencyDiff(base, result)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		want := []DependencyChange{{Name: "a", Result: "2.0.0"}}
		if len(got) != len(want) {
			t.Fatalf("run %d: changes = %+v, want %+v (the existing node_modules/a copy is unchanged; only the new nested copy is an addition)", i, got, want)
		}
		if got[0] != want[0] {
			t.Errorf("run %d: changes[0] = %+v, want %+v", i, got[0], want[0])
		}
	}
}

func TestPackageLockDependencyDiffRejectsMalformedJSON(t *testing.T) {
	if _, err := PackageLockDependencyDiff([]byte("{"), []byte(`{"packages":{}}`)); err == nil {
		t.Fatal("malformed base accepted")
	}
}

func TestPackageLockDependencyDiffDerivesNameFromInstallPath(t *testing.T) {
	base := []byte(`{"packages":{"node_modules/foo":{"version":"1.0.0"},"node_modules/@scope/bar":{"version":"1.0.0"}}}`)
	result := []byte(`{"packages":{"node_modules/foo":{"version":"1.1.0"},"node_modules/@scope/bar":{"version":"2.0.0"}}}`)
	got, err := PackageLockDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("PackageLockDependencyDiff: %v", err)
	}
	if len(got) != 2 || got[0].Name != "@scope/bar" || got[1].Name != "foo" {
		t.Fatalf("changes = %+v", got)
	}
}

func TestComposerLockDependencyDiffIncludesProductionAndDevelopmentPackages(t *testing.T) {
	base := []byte(`{"packages":[{"name":"vendor/a","version":"1.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`)
	result := []byte(`{"packages":[{"name":"vendor/a","version":"1.1.0"},{"name":"vendor/b","version":"3.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.1.0"}]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{
		{Name: "vendor/a", Base: "1.0.0", Result: "1.1.0", BaseScope: "packages", ResultScope: "packages"},
		{Name: "vendor/b", Result: "3.0.0", ResultScope: "packages"},
		{Name: "vendor/test", Base: "2.0.0", Result: "2.1.0", BaseScope: "packages-dev", ResultScope: "packages-dev"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsScopeChanges(t *testing.T) {
	base := []byte(`{"packages":[{"name":"vendor/a","version":"1.0.0"}],"packages-dev":[{"name":"vendor/test","version":"2.0.0"}]}`)
	result := []byte(`{"packages":[{"name":"vendor/test","version":"2.0.0"}],"packages-dev":[{"name":"vendor/a","version":"1.0.0"}]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{
		{Name: "vendor/a", Base: "1.0.0", Result: "1.0.0", BaseScope: "packages", ResultScope: "packages-dev"},
		{Name: "vendor/test", Base: "2.0.0", Result: "2.0.0", BaseScope: "packages-dev", ResultScope: "packages"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsPlatformRequirementChanges(t *testing.T) {
	base := []byte(`{"packages":[],"packages-dev":[],"platform":{"php":">=8.1"},"platform-dev":{"ext-json":"*"}}`)
	result := []byte(`{"packages":[],"packages-dev":[],"platform":{"php":">=8.2"},"platform-dev":{"ext-json":"*","ext-mbstring":"*"}}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{
		{Name: "ext-mbstring", Result: "*", ResultScope: "platform-dev"},
		{Name: "php", Base: ">=8.1", Result: ">=8.2", BaseScope: "platform", ResultScope: "platform"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffKeepsSamePlatformRequirementScopesDistinct(t *testing.T) {
	base := []byte(`{"packages":[],"packages-dev":[],"platform":{"php":">=8.1"},"platform-dev":{"php":">=8.1"}}`)
	result := []byte(`{"packages":[],"packages-dev":[],"platform":{"php":">=8.2"},"platform-dev":{"php":">=8.1"}}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{Name: "php", Base: ">=8.1", Result: ">=8.2", BaseScope: "platform", ResultScope: "platform"}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsDisabledPlatformOverride(t *testing.T) {
	base := []byte(`{"packages":[],"packages-dev":[],"platform":{},"platform-dev":{},"platform-overrides":{}}`)
	result := []byte(`{"packages":[],"packages-dev":[],"platform":{},"platform-dev":{},"platform-overrides":{"ext-foo":false}}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{Name: "ext-foo", Result: "false", ResultScope: "platform-overrides"}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsAliasChangesAndDeduplicatesIdenticalEntries(t *testing.T) {
	base := []byte(`{"packages":[],"packages-dev":[],"aliases":[{"package":"vendor/a","version":"dev-main","alias":"1.0.x-dev","alias_normalized":"1.0.9999999.9999999-dev"}]}`)
	result := []byte(`{"packages":[],"packages-dev":[],"aliases":[{"package":"vendor/a","version":"dev-main","alias":"2.0.x-dev","alias_normalized":"2.0.9999999.9999999-dev"},{"package":"vendor/a","version":"dev-main","alias":"2.0.x-dev","alias_normalized":"2.0.9999999.9999999-dev"}]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{
		{
			Name:                  "vendor/a",
			Result:                "dev-main",
			ResultScope:           "aliases",
			ResultAlias:           "2.0.x-dev",
			ResultAliasNormalized: "2.0.9999999.9999999-dev",
		},
		{
			Name:                "vendor/a",
			Base:                "dev-main",
			BaseScope:           "aliases",
			BaseAlias:           "1.0.x-dev",
			BaseAliasNormalized: "1.0.9999999.9999999-dev",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsLockedReferenceChanges(t *testing.T) {
	base := []byte(`{"packages":[{"name":"vendor/source","version":"dev-main","source":{"reference":"abc123"},"dist":{"reference":"same-dist"}},{"name":"vendor/dist","version":"dev-main","source":{"reference":"same-source"},"dist":{"reference":"abc123"}}],"packages-dev":[]}`)
	result := []byte(`{"packages":[{"name":"vendor/source","version":"dev-main","source":{"reference":"def456"},"dist":{"reference":"same-dist"}},{"name":"vendor/dist","version":"dev-main","source":{"reference":"same-source"},"dist":{"reference":"def456"}}],"packages-dev":[]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{
		{
			Name:                  "vendor/dist",
			Base:                  "dev-main",
			Result:                "dev-main",
			BaseScope:             "packages",
			ResultScope:           "packages",
			BaseSourceReference:   "same-source",
			ResultSourceReference: "same-source",
			BaseDistReference:     "abc123",
			ResultDistReference:   "def456",
		},
		{
			Name:                  "vendor/source",
			Base:                  "dev-main",
			Result:                "dev-main",
			BaseScope:             "packages",
			ResultScope:           "packages",
			BaseSourceReference:   "abc123",
			ResultSourceReference: "def456",
			BaseDistReference:     "same-dist",
			ResultDistReference:   "same-dist",
		},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsDistributionURLAndChecksumChanges(t *testing.T) {
	base := []byte(`{"packages":[{"name":"vendor/a","version":"1.0.0","dist":{"type":"zip","url":"https://example.test/a-old.zip","shasum":"oldsum","reference":"same-ref"}}],"packages-dev":[]}`)
	result := []byte(`{"packages":[{"name":"vendor/a","version":"1.0.0","dist":{"type":"zip","url":"https://example.test/a-new.zip","shasum":"newsum","reference":"same-ref"}}],"packages-dev":[]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{
		Name:                "vendor/a",
		Base:                "1.0.0",
		Result:              "1.0.0",
		BaseScope:           "packages",
		ResultScope:         "packages",
		BaseDistReference:   "same-ref",
		ResultDistReference: "same-ref",
		BaseDistURL:         "https://example.test/a-old.zip",
		ResultDistURL:       "https://example.test/a-new.zip",
		BaseDistShasum:      "oldsum",
		ResultDistShasum:    "newsum",
		BaseDistType:        "zip",
		ResultDistType:      "zip",
	}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsDistributionTypeChanges(t *testing.T) {
	base := []byte(`{"packages":[{"name":"vendor/a","version":"1.0.0","dist":{"type":"zip","url":"https://example.test/a.zip","shasum":"same-sum","reference":"same-ref"}}],"packages-dev":[]}`)
	result := []byte(`{"packages":[{"name":"vendor/a","version":"1.0.0","dist":{"type":"tar","url":"https://example.test/a.zip","shasum":"same-sum","reference":"same-ref"}}],"packages-dev":[]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{
		Name:                "vendor/a",
		Base:                "1.0.0",
		Result:              "1.0.0",
		BaseScope:           "packages",
		ResultScope:         "packages",
		BaseDistReference:   "same-ref",
		ResultDistReference: "same-ref",
		BaseDistURL:         "https://example.test/a.zip",
		ResultDistURL:       "https://example.test/a.zip",
		BaseDistShasum:      "same-sum",
		ResultDistShasum:    "same-sum",
		BaseDistType:        "zip",
		ResultDistType:      "tar",
	}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffReportsSourceLocationChanges(t *testing.T) {
	base := []byte(`{"packages":[{"name":"vendor/a","version":"dev-main","source":{"type":"git","url":"https://example.test/old.git","reference":"same-ref"}}],"packages-dev":[]}`)
	result := []byte(`{"packages":[{"name":"vendor/a","version":"dev-main","source":{"type":"hg","url":"https://example.test/new.hg","reference":"same-ref"}}],"packages-dev":[]}`)
	got, err := ComposerLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{
		Name:                  "vendor/a",
		Base:                  "dev-main",
		Result:                "dev-main",
		BaseScope:             "packages",
		ResultScope:           "packages",
		BaseSourceReference:   "same-ref",
		ResultSourceReference: "same-ref",
		BaseSourceURL:         "https://example.test/old.git",
		ResultSourceURL:       "https://example.test/new.hg",
		BaseSourceType:        "git",
		ResultSourceType:      "hg",
	}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestComposerLockDependencyDiffRejectsMalformedJSON(t *testing.T) {
	if _, err := ComposerLockDependencyDiff([]byte("{"), []byte(`{"packages":[]}`)); err == nil {
		t.Fatal("malformed base accepted")
	}
}

func TestComposerLockDependencyDiffRejectsMalformedPackageEntries(t *testing.T) {
	valid := `{"packages":[],"packages-dev":[]}`
	tests := []struct {
		name   string
		result string
	}{
		{"missing name", `{"packages":[{"version":"1.0.0"}],"packages-dev":[]}`},
		{"missing version", `{"packages":[{"name":"vendor/a"}],"packages-dev":[]}`},
		{"null version", `{"packages":[{"name":"vendor/a","version":null}],"packages-dev":[]}`},
		{"missing packages array", `{"packages-dev":[]}`},
		{"packages is not an array", `{"packages":{},"packages-dev":[]}`},
		{"missing packages-dev array", `{"packages":[]}`},
		{"duplicate package name", `{"packages":[{"name":"vendor/a","version":"1.0.0"},{"name":"vendor/a","version":"1.1.0"}],"packages-dev":[]}`},
		{"duplicate across production and development", `{"packages":[{"name":"vendor/a","version":"1.0.0"}],"packages-dev":[{"name":"vendor/a","version":"1.1.0"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ComposerLockDependencyDiff([]byte(valid), []byte(tt.result)); err == nil {
				t.Fatal("malformed Composer lockfile accepted")
			}
		})
	}
}

func TestPubspecLockDependencyDiff(t *testing.T) {
	base := []byte(`packages:
  a:
    version: "1.0.0"
  b:
    version: "2.0.0"`)
	result := []byte(`packages:
  a:
    version: "1.1.0"
  c:
    version: "3.0.0"`)
	got, err := PubspecLockDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{{Name: "a", Base: "1.0.0", Result: "1.1.0"}, {Name: "b", Base: "2.0.0"}, {Name: "c", Result: "3.0.0"}}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPubspecLockDependencyDiffRejectsMalformedYAML(t *testing.T) {
	if _, err := PubspecLockDependencyDiff([]byte("{"), []byte("packages: {}")); err == nil {
		t.Fatal("malformed base accepted")
	}
}

// TestPubspecLockDependencyDiffRejectsTrailingYAMLDocument is the
// regression test for a real finding from review: yaml.Unmarshal only
// decodes the first "---"-delimited document and silently ignores
// anything after it, unlike encoding/json.Unmarshal (used for
// package-lock.json), which errors on trailing bytes after a valid
// value. A pubspec.lock with a well-formed first document followed by
// malformed trailing content must still fail closed rather than
// silently diffing only the valid part.
func TestPubspecLockDependencyDiffRejectsTrailingYAMLDocument(t *testing.T) {
	corrupted := []byte("packages: {}\n---\n[\n")
	if _, err := PubspecLockDependencyDiff(corrupted, []byte("packages: {}")); err == nil {
		t.Fatal("base with trailing corrupted YAML document accepted")
	}
	if _, err := PubspecLockDependencyDiff([]byte("packages: {}"), corrupted); err == nil {
		t.Fatal("result with trailing corrupted YAML document accepted")
	}
}

func TestGoSumDependencyDiff(t *testing.T) {
	base := []byte(`github.com/a/a v1.0.0 h1:abc=
github.com/a/a v1.0.0/go.mod h1:def=
github.com/b/b v2.0.0 h1:ghi=
github.com/b/b v2.0.0/go.mod h1:jkl=`)
	result := []byte(`github.com/a/a v1.1.0 h1:mno=
github.com/a/a v1.1.0/go.mod h1:pqr=
github.com/c/c v3.0.0 h1:stu=
github.com/c/c v3.0.0/go.mod h1:vwx=`)
	got, err := GoSumDependencyDiff(base, result)
	if err != nil {
		t.Fatal(err)
	}
	want := []DependencyChange{
		{Name: "github.com/a/a", Base: "v1.0.0", Result: "v1.1.0"},
		{Name: "github.com/b/b", Base: "v2.0.0"},
		{Name: "github.com/c/c", Result: "v3.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestGoSumDependencyDiffRejectsMalformedInput(t *testing.T) {
	// "only two" splits into 2 fields, which is less than required 3
	if _, err := GoSumDependencyDiff([]byte("only two"), []byte(`github.com/a/a v1.0.0 h1:abc=`)); err == nil {
		t.Fatal("malformed base (too few fields) accepted")
	}
	// "a b c d" splits into 4 fields, which is more than required 3
	if _, err := GoSumDependencyDiff([]byte(`github.com/a/a v1.0.0 h1:abc=`), []byte("a b c d")); err == nil {
		t.Fatal("malformed result (too many fields) accepted")
	}
}

// TestGoSumDependencyDiffMultipleVersionsReportsFullMembershipDiff is the
// regression test for a real P2 finding from GitHub's automated Codex PR
// reviewer (independently corroborating an earlier internal review round
// on the same code): go.sum records every checksum the toolchain has
// encountered, not just the version Minimal Version Selection currently
// builds with, so collapsing a module's version set down to "the highest
// version" can silently discard evidence — a *stale* higher checksum can
// remain in the file while the actually-used lower version changes
// underneath it, and comparing only the highest would report no change at
// all. A module with multiple versions on either side must report the
// full set-membership diff instead: one entry per version added, one per
// version removed.
func TestGoSumDependencyDiffMultipleVersionsReportsFullMembershipDiff(t *testing.T) {
	// Module "app" has v1.0.0 (a stale, no-longer-selected entry) and
	// v1.9.0 in base. In result, the stale v1.0.0 entry is gone and a new
	// v1.3.0 was added, while v1.9.0's checksum is untouched — exactly
	// the scenario where a highest-only diff would report nothing.
	base := []byte(`github.com/app/app v1.0.0 h1:abc=
github.com/app/app v1.0.0/go.mod h1:def=
github.com/app/app v1.9.0 h1:ghi=
github.com/app/app v1.9.0/go.mod h1:jkl=`)
	result := []byte(`github.com/app/app v1.3.0 h1:mno=
github.com/app/app v1.3.0/go.mod h1:pqr=
github.com/app/app v1.9.0 h1:ghi=
github.com/app/app v1.9.0/go.mod h1:jkl=`)
	got, err := GoSumDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("GoSumDependencyDiff: %v", err)
	}
	// Sorted by Name, then Base, then Result: an empty Base ("" from the
	// added-version entry) sorts before a non-empty one.
	want := []DependencyChange{
		{Name: "github.com/app/app", Result: "v1.3.0"},
		{Name: "github.com/app/app", Base: "v1.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestGoSumDependencyDiffMultipleVersionsNoChange proves that a module
// whose multi-version set is byte-for-byte identical between base and
// result reports no changes at all.
func TestGoSumDependencyDiffMultipleVersionsNoChange(t *testing.T) {
	sum := []byte(`github.com/app/app v1.0.0 h1:abc=
github.com/app/app v1.0.0/go.mod h1:def=
github.com/app/app v1.9.0 h1:ghi=
github.com/app/app v1.9.0/go.mod h1:jkl=`)
	got, err := GoSumDependencyDiff(sum, sum)
	if err != nil {
		t.Fatalf("GoSumDependencyDiff: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("changes = %+v, want none (identical multi-version sets)", got)
	}
}

// TestGoSumDependencyDiffSingleVersionUnaffectedByMultiVersionPath proves
// that the common single-version-per-module case still reports a clean
// paired Base/Result entry (matching every other DependencyDiff
// function's version-bump shape), not a two-entry membership diff, even
// though the underlying set representation could support either.
func TestGoSumDependencyDiffSingleVersionUnaffectedByMultiVersionPath(t *testing.T) {
	base := []byte(`github.com/app/app v1.0.0 h1:abc=
github.com/app/app v1.0.0/go.mod h1:def=`)
	result := []byte(`github.com/app/app v1.1.0 h1:ghi=
github.com/app/app v1.1.0/go.mod h1:jkl=`)
	got, err := GoSumDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("GoSumDependencyDiff: %v", err)
	}
	want := []DependencyChange{{Name: "github.com/app/app", Base: "v1.0.0", Result: "v1.1.0"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
}

// TestGoSumDependencyDiffSkipsBlankLines proves that empty and
// whitespace-only lines in go.sum are safely ignored.
func TestGoSumDependencyDiffSkipsBlankLines(t *testing.T) {
	base := []byte(`github.com/a/a v1.0.0 h1:abc=
github.com/a/a v1.0.0/go.mod h1:def=


github.com/b/b v2.0.0 h1:ghi=`)
	result := []byte(`github.com/a/a v1.1.0 h1:mno=
github.com/a/a v1.1.0/go.mod h1:pqr=`)
	got, err := GoSumDependencyDiff(base, result)
	if err != nil {
		t.Fatalf("GoSumDependencyDiff: %v", err)
	}
	want := []DependencyChange{
		{Name: "github.com/a/a", Base: "v1.0.0", Result: "v1.1.0"},
		{Name: "github.com/b/b", Base: "v2.0.0"},
	}
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
