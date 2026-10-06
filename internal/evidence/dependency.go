package evidence

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DependencyChange describes a dependency identity and its base and result
// versions. Empty sides represent additions/removals. Composer changes also
// retain the production/development scope and locked source/artifact
// references when they are known.
type DependencyChange struct {
	Name                  string `json:"name"`
	Base                  string `json:"base,omitempty"`
	Result                string `json:"result,omitempty"`
	BaseScope             string `json:"base_scope,omitempty"`
	ResultScope           string `json:"result_scope,omitempty"`
	BaseSourceReference   string `json:"base_source_reference,omitempty"`
	ResultSourceReference string `json:"result_source_reference,omitempty"`
	BaseDistReference     string `json:"base_dist_reference,omitempty"`
	ResultDistReference   string `json:"result_dist_reference,omitempty"`
	BaseDistURL           string `json:"base_dist_url,omitempty"`
	ResultDistURL         string `json:"result_dist_url,omitempty"`
	BaseDistShasum        string `json:"base_dist_shasum,omitempty"`
	ResultDistShasum      string `json:"result_dist_shasum,omitempty"`
	BaseDistType          string `json:"base_dist_type,omitempty"`
	ResultDistType        string `json:"result_dist_type,omitempty"`
	BaseSourceURL         string `json:"base_source_url,omitempty"`
	ResultSourceURL       string `json:"result_source_url,omitempty"`
	BaseSourceType        string `json:"base_source_type,omitempty"`
	ResultSourceType      string `json:"result_source_type,omitempty"`
	BaseAlias             string `json:"base_alias,omitempty"`
	ResultAlias           string `json:"result_alias,omitempty"`
	BaseAliasNormalized   string `json:"base_alias_normalized,omitempty"`
	ResultAliasNormalized string `json:"result_alias_normalized,omitempty"`
}

// PackageLockDependencyDiff computes semantic dependency changes from npm
// lockfile JSON. It supports both lockfile v2/v3 packages maps and fails
// closed on malformed input rather than silently treating it as unchanged.
//
// Diffed by installation path, not package name — found via review: an
// earlier version collapsed every installed copy of the same package name
// into one name-keyed entry, overwriting whichever one the (unordered) map
// iteration happened to visit last. A lockfile with multiple installed
// copies of the same dependency at different paths and versions (npm's own
// nested-dependency-resolution output, not a malformed or unusual
// lockfile) could then report a false change on an unrelated path, miss a
// real change to a non-surviving copy, or — since base and result are
// parsed independently, each with its own random map order — report
// different results for the exact same pair of lockfiles from one call to
// the next.
func PackageLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := packageLockVersions(base)
	if err != nil {
		return nil, fmt.Errorf("parse base package-lock: %w", err)
	}
	now, err := packageLockVersions(result)
	if err != nil {
		return nil, fmt.Errorf("parse result package-lock: %w", err)
	}
	paths := map[string]bool{}
	for p := range old {
		paths[p] = true
	}
	for p := range now {
		paths[p] = true
	}
	changes := make([]DependencyChange, 0)
	for path := range paths {
		oldEntry, now2 := old[path], now[path]
		if oldEntry.Version == now2.Version {
			continue
		}
		name := now2.Name
		if name == "" {
			name = oldEntry.Name
		}
		changes = append(changes, DependencyChange{Name: name, Base: oldEntry.Version, Result: now2.Version})
	}
	// Name is not unique across entries by design (see doc comment above),
	// so Base/Result break ties deterministically rather than leaving
	// same-name entries in map-iteration (random) relative order.
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Base != b.Base {
			return a.Base < b.Base
		}
		return a.Result < b.Result
	})
	return changes, nil
}

// packageLockEntry is one lockfile "packages" map entry's identity —
// package name and installed version — keyed by installation path by
// packageLockVersions below, never by name alone (see
// PackageLockDependencyDiff's doc comment for why).
type packageLockEntry struct {
	Name    string
	Version string
}

func packageLockVersions(b []byte) (map[string]packageLockEntry, error) {
	var doc struct {
		Packages map[string]struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	entries := map[string]packageLockEntry{}
	for path, pkg := range doc.Packages {
		if path == "" {
			continue
		}
		name := pkg.Name
		if name == "" {
			name = packageNameFromInstallPath(path)
		}
		if name == "" {
			continue
		}
		entries[path] = packageLockEntry{Name: name, Version: pkg.Version}
	}
	return entries, nil
}

func packageNameFromInstallPath(installPath string) string {
	parts := strings.Split(strings.Trim(installPath, "/"), "/")
	for i := len(parts) - 2; i >= 0; i-- {
		if parts[i] == "node_modules" && i+1 < len(parts) {
			name := parts[i+1]
			if strings.HasPrefix(name, "@") && i+2 < len(parts) {
				return name + "/" + parts[i+2]
			}
			return name
		}
	}
	return ""
}

// ComposerLockDependencyDiff computes semantic dependency changes from
// Composer lockfile JSON. It includes both production and development
// packages and fails closed on malformed input rather than silently treating
// it as unchanged.
func ComposerLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := composerLockVersions(base)
	if err != nil {
		return nil, fmt.Errorf("parse base composer.lock: %w", err)
	}
	now, err := composerLockVersions(result)
	if err != nil {
		return nil, fmt.Errorf("parse result composer.lock: %w", err)
	}
	names := map[string]bool{}
	for name := range old {
		names[name] = true
	}
	for name := range now {
		names[name] = true
	}
	changes := make([]DependencyChange, 0)
	for name := range names {
		oldEntry, nowEntry := old[name], now[name]
		if oldEntry == nowEntry {
			continue
		}
		changeName := nowEntry.Name
		if changeName == "" {
			changeName = oldEntry.Name
		}
		changes = append(changes, DependencyChange{
			Name:                  changeName,
			Base:                  oldEntry.Version,
			Result:                nowEntry.Version,
			BaseScope:             oldEntry.Scope,
			ResultScope:           nowEntry.Scope,
			BaseSourceReference:   oldEntry.SourceReference,
			ResultSourceReference: nowEntry.SourceReference,
			BaseDistReference:     oldEntry.DistReference,
			ResultDistReference:   nowEntry.DistReference,
			BaseDistURL:           oldEntry.DistURL,
			ResultDistURL:         nowEntry.DistURL,
			BaseDistShasum:        oldEntry.DistShasum,
			ResultDistShasum:      nowEntry.DistShasum,
			BaseDistType:          oldEntry.DistType,
			ResultDistType:        nowEntry.DistType,
			BaseSourceURL:         oldEntry.SourceURL,
			ResultSourceURL:       nowEntry.SourceURL,
			BaseSourceType:        oldEntry.SourceType,
			ResultSourceType:      nowEntry.SourceType,
			BaseAlias:             oldEntry.Alias,
			ResultAlias:           nowEntry.Alias,
			BaseAliasNormalized:   oldEntry.AliasNormalized,
			ResultAliasNormalized: nowEntry.AliasNormalized,
		})
	}
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Base != b.Base {
			return a.Base < b.Base
		}
		if a.Result != b.Result {
			return a.Result < b.Result
		}
		if a.BaseScope != b.BaseScope {
			return a.BaseScope < b.BaseScope
		}
		return a.ResultScope < b.ResultScope
	})
	return changes, nil
}

type composerLockEntry struct {
	Name            string
	Version         string
	Scope           string
	SourceReference string
	SourceURL       string
	SourceType      string
	DistReference   string
	DistURL         string
	DistShasum      string
	DistType        string
	Alias           string
	AliasNormalized string
}

func composerLockVersions(b []byte) (map[string]composerLockEntry, error) {
	var doc struct {
		Packages          json.RawMessage `json:"packages"`
		PackagesDev       json.RawMessage `json:"packages-dev"`
		Platform          json.RawMessage `json:"platform"`
		PlatformDev       json.RawMessage `json:"platform-dev"`
		PlatformOverrides json.RawMessage `json:"platform-overrides"`
		Aliases           json.RawMessage `json:"aliases"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	packages, err := decodeComposerLockPackages("packages", doc.Packages)
	if err != nil {
		return nil, err
	}
	packagesDev, err := decodeComposerLockPackages("packages-dev", doc.PackagesDev)
	if err != nil {
		return nil, err
	}
	platform, err := decodeComposerLockPlatform("platform", doc.Platform)
	if err != nil {
		return nil, err
	}
	platformDev, err := decodeComposerLockPlatform("platform-dev", doc.PlatformDev)
	if err != nil {
		return nil, err
	}
	platformOverrides, err := decodeComposerLockOverrides(doc.PlatformOverrides)
	if err != nil {
		return nil, err
	}
	aliases, err := decodeComposerLockAliases(doc.Aliases)
	if err != nil {
		return nil, err
	}
	versions := map[string]composerLockEntry{}
	seenPackages := map[string]bool{}
	for _, group := range []struct {
		field    string
		packages []composerLockPackage
	}{{"packages", packages}, {"packages-dev", packagesDev}} {
		for _, pkg := range group.packages {
			if seenPackages[pkg.Name] {
				return nil, fmt.Errorf("composer.lock package %q appears more than once (in %q)", pkg.Name, group.field)
			}
			seenPackages[pkg.Name] = true
			versions[composerLockIdentityKey("package", pkg.Name)] = composerLockEntry{
				Name:            pkg.Name,
				Version:         pkg.Version,
				Scope:           group.field,
				SourceReference: pkg.Source.Reference,
				SourceURL:       pkg.Source.URL,
				SourceType:      pkg.Source.Type,
				DistReference:   pkg.Dist.Reference,
				DistURL:         pkg.Dist.URL,
				DistShasum:      pkg.Dist.Shasum,
				DistType:        pkg.Dist.Type,
			}
		}
	}
	for _, group := range []struct {
		field        string
		requirements map[string]string
	}{{"platform", platform}, {"platform-dev", platformDev}} {
		for name, requirement := range group.requirements {
			versions[composerLockIdentityKey(group.field, name)] = composerLockEntry{Name: name, Version: requirement, Scope: group.field}
		}
	}
	for name, override := range platformOverrides {
		versions[composerLockIdentityKey("platform-overrides", name)] = composerLockEntry{Name: name, Version: override, Scope: "platform-overrides"}
	}
	seenAliases := map[string]bool{}
	for _, alias := range aliases {
		identity := composerLockIdentityKey("alias", alias.Package+"\x00"+alias.Version+"\x00"+alias.Alias+"\x00"+alias.AliasNormalized)
		if seenAliases[identity] {
			continue
		}
		seenAliases[identity] = true
		versions[identity] = composerLockEntry{
			Name:            alias.Package,
			Version:         alias.Version,
			Scope:           "aliases",
			Alias:           alias.Alias,
			AliasNormalized: alias.AliasNormalized,
		}
	}
	return versions, nil
}

func composerLockIdentityKey(kind, name string) string {
	return kind + "\x00" + name
}

type composerLockPackage struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  struct {
		Reference string `json:"reference"`
		URL       string `json:"url"`
		Type      string `json:"type"`
	} `json:"source"`
	Dist struct {
		Reference string `json:"reference"`
		URL       string `json:"url"`
		Shasum    string `json:"shasum"`
		Type      string `json:"type"`
	} `json:"dist"`
}

type composerLockAlias struct {
	Package         string `json:"package"`
	Version         string `json:"version"`
	Alias           string `json:"alias"`
	AliasNormalized string `json:"alias_normalized"`
}

func decodeComposerLockPackages(field string, raw json.RawMessage) ([]composerLockPackage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("composer.lock field %q must be an array", field)
	}
	var packages []composerLockPackage
	if err := json.Unmarshal(trimmed, &packages); err != nil {
		return nil, fmt.Errorf("parse composer.lock field %q: %w", field, err)
	}
	for i, pkg := range packages {
		if pkg.Name == "" {
			return nil, fmt.Errorf("composer.lock field %q package %d has an empty name", field, i)
		}
		if pkg.Version == "" {
			return nil, fmt.Errorf("composer.lock field %q package %d has an empty version", field, i)
		}
	}
	return packages, nil
}

func decodeComposerLockPlatform(field string, raw json.RawMessage) (map[string]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]string{}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("composer.lock field %q must be an object", field)
	}
	var requirements map[string]string
	if err := json.Unmarshal(trimmed, &requirements); err != nil {
		return nil, fmt.Errorf("parse composer.lock field %q: %w", field, err)
	}
	for name, requirement := range requirements {
		if name == "" {
			return nil, fmt.Errorf("composer.lock field %q contains an empty requirement name", field)
		}
		if requirement == "" {
			return nil, fmt.Errorf("composer.lock field %q requirement %q has an empty constraint", field, name)
		}
	}
	return requirements, nil
}

func decodeComposerLockOverrides(raw json.RawMessage) (map[string]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return map[string]string{}, nil
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("composer.lock field %q must be an object", "platform-overrides")
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return nil, fmt.Errorf("parse composer.lock field %q: %w", "platform-overrides", err)
	}
	overrides := make(map[string]string, len(values))
	for name, rawValue := range values {
		if name == "" {
			return nil, fmt.Errorf("composer.lock field %q contains an empty override name", "platform-overrides")
		}
		value := bytes.TrimSpace(rawValue)
		if bytes.Equal(value, []byte("false")) {
			overrides[name] = "false"
			continue
		}
		var version string
		if err := json.Unmarshal(value, &version); err != nil || version == "" {
			return nil, fmt.Errorf("composer.lock field %q override %q must be a nonempty string or false", "platform-overrides", name)
		}
		overrides[name] = version
	}
	return overrides, nil
}

func decodeComposerLockAliases(raw json.RawMessage) ([]composerLockAlias, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return []composerLockAlias{}, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("composer.lock field %q must be an array", "aliases")
	}
	var aliases []composerLockAlias
	if err := json.Unmarshal(trimmed, &aliases); err != nil {
		return nil, fmt.Errorf("parse composer.lock field %q: %w", "aliases", err)
	}
	for i, alias := range aliases {
		if alias.Package == "" {
			return nil, fmt.Errorf("composer.lock field %q alias %d has an empty package", "aliases", i)
		}
		if alias.Version == "" {
			return nil, fmt.Errorf("composer.lock field %q alias %d has an empty version", "aliases", i)
		}
		if alias.Alias == "" {
			return nil, fmt.Errorf("composer.lock field %q alias %d has an empty alias", "aliases", i)
		}
	}
	return aliases, nil
}

// PubspecLockDependencyDiff computes semantic dependency changes from Dart/Flutter
// lockfile YAML. It fails closed on malformed input rather than silently treating
// it as unchanged.
//
// Unlike PackageLockDependencyDiff, pubspec.lock uses package name directly as the
// key in the packages map, with no installation-path ambiguity — each package name
// appears exactly once. Diffing by name is both sufficient and necessary; there is
// no path-keying complexity.
func PubspecLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := pubspecLockVersions(base)
	if err != nil {
		return nil, fmt.Errorf("parse base pubspec.lock: %w", err)
	}
	now, err := pubspecLockVersions(result)
	if err != nil {
		return nil, fmt.Errorf("parse result pubspec.lock: %w", err)
	}
	names := map[string]bool{}
	for n := range old {
		names[n] = true
	}
	for n := range now {
		names[n] = true
	}
	changes := make([]DependencyChange, 0)
	for name := range names {
		oldVersion, nowVersion := old[name], now[name]
		if oldVersion == nowVersion {
			continue
		}
		changes = append(changes, DependencyChange{Name: name, Base: oldVersion, Result: nowVersion})
	}
	// Sort deterministically: by Name, then Base, then Result, matching
	// PackageLockDependencyDiff's sorting order for consistency.
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Base != b.Base {
			return a.Base < b.Base
		}
		return a.Result < b.Result
	})
	return changes, nil
}

func pubspecLockVersions(b []byte) (map[string]string, error) {
	var doc struct {
		Packages map[string]struct {
			Version string `yaml:"version"`
		} `yaml:"packages"`
	}
	// yaml.Unmarshal only decodes the first "---"-delimited document and
	// silently ignores anything after it, unlike encoding/json.Unmarshal,
	// which errors on trailing bytes after a valid value. Decode via
	// yaml.Decoder and require io.EOF on the next Decode call so trailing
	// malformed content isn't silently dropped as unchanged evidence.
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil && err != io.EOF {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("pubspec.lock contains more than one YAML document")
		}
		return nil, err
	}
	versions := map[string]string{}
	for name, pkg := range doc.Packages {
		if name != "" {
			versions[name] = pkg.Version
		}
	}
	return versions, nil
}

// GoSumDependencyDiff computes semantic dependency changes from Go's
// go.sum lockfile format. It fails closed on malformed input rather than
// silently treating it as unchanged.
//
// go.sum format is plain text with one entry per line: three whitespace-
// separated fields (module name, version, hash). Each module version
// normally produces TWO lines: one for the module zip hash and one for the
// go.mod file hash (with "/go.mod" appended to the version field). A
// module can appear with multiple versions in the same file, because
// go.sum records every checksum the Go toolchain has encountered while
// resolving the module graph — not just the versions Minimal Version
// Selection currently selects for the build.
//
// When a module has exactly one version on both the base and result side
// (by far the common case — a single selected-and-verified version, plus
// its /go.mod hash line), that's reported the same way every other
// DependencyDiff function reports a version bump: one entry with Base set
// to the old version and Result set to the new one, or nothing at all if
// unchanged.
//
// Otherwise — a module with multiple recorded versions on either side,
// which happens because go.sum records every checksum the Go toolchain
// has encountered while resolving the module graph, not just the version
// Minimal Version Selection currently builds with — there is no reliable
// way to collapse that set down to a single "the" version and diff just
// that: found via review (both an internal review round and GitHub's own
// automated PR reviewer, independently), an earlier version of this
// function reported only the highest semver version per module, on the
// reasoning that a lower-priority entry appearing or disappearing
// alongside an unchanged highest version wasn't a real change. That
// reasoning doesn't hold for go.sum: a *stale* higher checksum can remain
// in the file (go.sum is not aggressively pruned) while the actually-used
// lower version changes underneath it, and "highest unchanged" would then
// silently discard real evidence of that change. So a module with a
// multi-version set on either side instead gets the full membership diff:
// one DependencyChange per version that was added to (Result set, Base
// empty) or removed from (Base set, Result empty) its checksum set. This
// needs no semver-precedence comparator, and can't discard real evidence,
// since every set membership change is preserved.
//
// Semantic diffing by module name de-duplicates the /go.mod suffix.
// Malformed lines (wrong field count, non-whitespace-separated) return an
// error rather than silently skipping them. Blank or whitespace-only lines
// are safely skipped as normal go.sum artifacts.
func GoSumDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := goSumVersions(base)
	if err != nil {
		return nil, fmt.Errorf("parse base go.sum: %w", err)
	}
	now, err := goSumVersions(result)
	if err != nil {
		return nil, fmt.Errorf("parse result go.sum: %w", err)
	}
	names := map[string]bool{}
	for n := range old {
		names[n] = true
	}
	for n := range now {
		names[n] = true
	}
	changes := make([]DependencyChange, 0)
	for name := range names {
		oldVersions, nowVersions := old[name], now[name]
		if len(oldVersions) <= 1 && len(nowVersions) <= 1 {
			// The common case: at most one recorded version on each
			// side. Report it the same way every other DependencyDiff
			// function reports a version bump — one paired entry, or
			// nothing if unchanged. singleVersion returns "" for an
			// empty set, matching an addition/removal.
			oldVersion, nowVersion := singleVersion(oldVersions), singleVersion(nowVersions)
			if oldVersion != nowVersion {
				changes = append(changes, DependencyChange{Name: name, Base: oldVersion, Result: nowVersion})
			}
			continue
		}
		// A module with multiple recorded versions on either side: no
		// single-pair summary can represent this without risking lost
		// evidence (see doc comment above), so report the full
		// membership diff instead.
		for v := range oldVersions {
			if !nowVersions[v] {
				changes = append(changes, DependencyChange{Name: name, Base: v})
			}
		}
		for v := range nowVersions {
			if !oldVersions[v] {
				changes = append(changes, DependencyChange{Name: name, Result: v})
			}
		}
	}
	// Sort deterministically: by Name, then Base, then Result, matching
	// other DependencyDiff functions' sorting order for consistency —
	// necessary here since the two membership loops above iterate maps
	// in random order.
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Base != b.Base {
			return a.Base < b.Base
		}
		return a.Result < b.Result
	})
	return changes, nil
}

// singleVersion returns the one version in a set of at most one version,
// or "" for an empty set. Callers must not pass a set with more than one
// element.
func singleVersion(versions map[string]bool) string {
	for v := range versions {
		return v
	}
	return ""
}

// goSumVersions parses go.sum and returns a map of module name to set of
// versions. Each module typically has two lines (one for the module, one for
// its go.mod file), both with the same version. The /go.mod suffix is
// stripped from the version field before use, so both lines are treated as
// duplicates of the same (module, version) pair.
func goSumVersions(b []byte) (map[string]map[string]bool, error) {
	versions := make(map[string]map[string]bool)
	for _, line := range bytes.Split(b, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		fields := bytes.Fields(trimmed)
		if len(fields) != 3 {
			return nil, fmt.Errorf("malformed go.sum line: expected 3 whitespace-separated fields, got %d", len(fields))
		}
		moduleName := string(fields[0])
		version := string(fields[1])
		// Strip /go.mod suffix if present, treating both the module zip
		// entry and its go.mod file entry as the same version pair.
		version = strings.TrimSuffix(version, "/go.mod")
		if moduleName == "" || version == "" {
			continue
		}
		if versions[moduleName] == nil {
			versions[moduleName] = make(map[string]bool)
		}
		versions[moduleName][version] = true
	}
	return versions, nil
}
