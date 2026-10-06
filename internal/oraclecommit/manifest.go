// Package oraclecommit lets the factory HOST (never the sandboxed agent)
// commit accepted acceptance oracles into the target repository as part of
// a run's result commit, so the pull request carries them.
//
// The whole mechanism is inert unless an approved oracle MANIFEST.json
// entry declares a target_path: no existing flow produces one, so a
// manifest without it yields no plan, no commit and no run-record change.
//
// Trust model: MANIFEST.json and the oracle files it names are read from
// the hash-verified, host-owned snapshot that was actually mounted for the
// reference_oracle gate -- never from a path the agent can write. Every
// path that will be written into the repository is validated here, and
// re-checked against the real working tree at write time (symlinks).
package oraclecommit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/evidence"
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
)

const (
	// ManifestName is the manifest file inside an oracle directory.
	ManifestName = "MANIFEST.json"
	// IndexPath is the fixed repository path of the committed oracle index.
	IndexPath = ".buildgate/oracles.json"
	// IndexDir is the directory that is protected unconditionally.
	IndexDir = ".buildgate"

	maxOracleFileBytes = 1 << 20
	maxManifestBytes   = 1 << 20
	maxEntries         = 256
)

// ErrMalformedManifest means MANIFEST.json exists but is not a JSON array
// of objects. Such a manifest cannot declare a target_path, so callers
// treat it as "inert" (the existing oracle-coverage loader already warns
// about it); it is distinct from every other error, which callers must
// treat as fatal.
var ErrMalformedManifest = errors.New("oracle manifest is not a JSON array of objects")

// Entry is one MANIFEST.json entry, decoded into oraclecommit's own struct
// (internal/conformity reads the same file for coverage only).
type Entry struct {
	Criterion      string
	OracleFile     string
	TargetPath     string
	Supersedes     []string
	CriterionIndex int
}

type rawEntry struct {
	Criterion      *string  `json:"criterion"`
	OracleFile     *string  `json:"oracle_file"`
	TargetPath     *string  `json:"target_path"`
	Supersedes     []string `json:"supersedes"`
	CriterionIndex *int     `json:"criterion_index"`
}

// File is one oracle to commit: validated target path, the exact bytes read
// from the verified snapshot, and their SHA-256.
type File struct {
	TargetPath     string
	Bytes          []byte
	SHA256         string
	CriterionIndex int
}

// Plan is the validated commit set derived from a manifest. nil means the
// manifest declares no target_path anywhere (inert).
type Plan struct {
	Files      []File
	Supersedes []string
}

// ParseManifest returns the entries that declare a target_path, validated.
// It returns (nil, nil) when no entry declares one. Entries without a
// target_path are ignored entirely (including any supersedes on them).
func ParseManifest(data []byte, extraProtected []string) ([]Entry, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedManifest, err)
	}
	if len(raws) > maxEntries {
		return nil, fmt.Errorf("oracle manifest has %d entries, limit is %d", len(raws), maxEntries)
	}
	var out []Entry
	for i, raw := range raws {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("%w: entry %d: %v", ErrMalformedManifest, i, err)
		}
		if _, ok := probe["target_path"]; !ok {
			continue
		}
		var re rawEntry
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&re); err != nil {
			return nil, fmt.Errorf("oracle manifest entry %d: %w", i, err)
		}
		if re.TargetPath == nil || *re.TargetPath == "" {
			// A present-but-null/empty target_path is not "absent": refuse
			// rather than silently skip an entry someone tried to set.
			if string(probe["target_path"]) == "null" {
				continue
			}
			return nil, fmt.Errorf("oracle manifest entry %d: empty target_path", i)
		}
		if err := ValidateTargetPath(*re.TargetPath, extraProtected); err != nil {
			return nil, fmt.Errorf("oracle manifest entry %d: target_path: %w", i, err)
		}
		if re.OracleFile == nil || *re.OracleFile == "" {
			return nil, fmt.Errorf("oracle manifest entry %d: target_path %q declared without an oracle_file", i, *re.TargetPath)
		}
		if err := validateOracleFileName(*re.OracleFile); err != nil {
			return nil, fmt.Errorf("oracle manifest entry %d: oracle_file: %w", i, err)
		}
		e := Entry{OracleFile: *re.OracleFile, TargetPath: *re.TargetPath, CriterionIndex: i}
		if re.Criterion != nil {
			e.Criterion = *re.Criterion
		}
		if re.CriterionIndex != nil {
			if *re.CriterionIndex < 0 {
				return nil, fmt.Errorf("oracle manifest entry %d: negative criterion_index", i)
			}
			e.CriterionIndex = *re.CriterionIndex
		}
		for _, s := range re.Supersedes {
			if err := ValidateTargetPath(s, extraProtected); err != nil {
				return nil, fmt.Errorf("oracle manifest entry %d: supersedes %q: %w", i, s, err)
			}
			e.Supersedes = append(e.Supersedes, s)
		}
		out = append(out, e)
	}
	return out, nil
}

// LoadPlan reads MANIFEST.json and the oracle files it names from
// snapshotDir (the immutable, hash-verified copy that was mounted) and
// returns the commit set. (nil, nil) when there is no manifest or no entry
// declares a target_path -- the inert case.
func LoadPlan(snapshotDir string, extraProtected []string) (*Plan, error) {
	manifest, err := evidence.ReadHostileFile(filepath.Join(snapshotDir, ManifestName), maxManifestBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read oracle manifest: %w", err)
	}
	entries, err := ParseManifest(manifest, extraProtected)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, nil
	}
	plan := &Plan{}
	targets := map[string]bool{}
	superseded := map[string]bool{}
	// Several criteria may be covered by one oracle file, each with its own
	// entry naming the same oracle_file and target_path: that is ONE file to
	// commit (the first entry's criterion_index). The same target_path for a
	// different oracle_file is still refused.
	entries = dedupeSharedFileEntries(entries)
	for _, e := range entries {
		if targets[e.TargetPath] {
			return nil, fmt.Errorf("oracle manifest declares target_path %q more than once", e.TargetPath)
		}
		targets[e.TargetPath] = true
		for _, s := range e.Supersedes {
			superseded[s] = true
		}
	}
	for s := range superseded {
		if targets[s] {
			return nil, fmt.Errorf("oracle manifest both supersedes and writes %q", s)
		}
		plan.Supersedes = append(plan.Supersedes, s)
	}
	sort.Strings(plan.Supersedes)
	for _, e := range entries {
		data, err := readSnapshotFile(snapshotDir, e.OracleFile)
		if err != nil {
			return nil, fmt.Errorf("oracle file %q for target_path %q: %w", e.OracleFile, e.TargetPath, err)
		}
		plan.Files = append(plan.Files, File{TargetPath: e.TargetPath, Bytes: data, SHA256: HashBytes(data), CriterionIndex: e.CriterionIndex})
	}
	sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].TargetPath < plan.Files[j].TargetPath })
	return plan, nil
}

// dedupeSharedFileEntries merges entries with the same oracle_file AND
// target_path (their supersedes are unioned); entries that share only a
// target_path are left for the caller's duplicate check to refuse.
func dedupeSharedFileEntries(entries []Entry) []Entry {
	type key struct{ file, target string }
	at := map[key]int{}
	var out []Entry
	for _, e := range entries {
		k := key{e.OracleFile, e.TargetPath}
		if i, ok := at[k]; ok {
			out[i].Supersedes = append(out[i].Supersedes, e.Supersedes...)
			continue
		}
		at[k] = len(out)
		out = append(out, e)
	}
	return out
}

func readSnapshotFile(snapshotDir, rel string) ([]byte, error) {
	full := filepath.Join(snapshotDir, filepath.FromSlash(rel))
	// Walk every component with Lstat: the snapshot has no symlinks
	// (evidence.SnapshotTree refuses them) but this reader must not rely on
	// that for its own safety.
	cur := snapshotDir
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink", cur)
		}
	}
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", full)
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxOracleFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxOracleFileBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxOracleFileBytes)
	}
	return data, nil
}

func validateOracleFileName(rel string) error {
	if rel == "" || strings.ContainsAny(rel, "\x00\\") || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("invalid oracle file name %q", rel)
	}
	// A leading ':' is pathspec magic (":!x" matches everything but x) and a
	// leading '-' reads as an option.
	if rel[0] == ':' || rel[0] == '-' {
		return fmt.Errorf("oracle file name %q must not start with ':' or '-'", rel)
	}
	if path.Clean(rel) != rel || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("oracle file name %q must be a clean relative path inside the oracle directory", rel)
	}
	if rel == ManifestName {
		return fmt.Errorf("oracle file name must not be %s", ManifestName)
	}
	return nil
}

// ValidateTargetPath refuses any repository path an oracle may not be
// written to (or supersede): absolute, unclean, traversing, anything under
// or named like .git*, .buildgate, .oracle (matched case-insensitively, at
// any depth, because a case-folding filesystem or a nested checkout would
// otherwise alias them), the project config file, harness byproducts,
// dependency lockfiles, and every path in extraProtected (matched exactly
// or, for a trailing "/", as a directory prefix).
func ValidateTargetPath(p string, extraProtected []string) error {
	if p == "" {
		return errors.New("empty path")
	}
	if p[0] == ':' || p[0] == '-' {
		return fmt.Errorf("path %q must not start with ':' (pathspec magic) or '-'", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("path %q contains a control or backslash character", p)
		}
		// Glob metacharacters would turn an index row into a broad
		// path.Match pattern in the protected-path check.
		if r == '*' || r == '?' || r == '[' {
			return fmt.Errorf("path %q contains a glob character", p)
		}
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) || (len(p) >= 2 && p[1] == ':') {
		return fmt.Errorf("path %q must be relative", p)
	}
	if path.Clean(p) != p || p == "." {
		return fmt.Errorf("path %q is not a clean relative path (no ./, //, trailing / or ..)", p)
	}
	segs := strings.Split(p, "/")
	for _, seg := range segs {
		if seg == ".." || seg == "." || seg == "" {
			return fmt.Errorf("path %q traverses", p)
		}
		low := strings.ToLower(seg)
		switch {
		case strings.HasPrefix(low, ".git"):
			return fmt.Errorf("path %q is under a .git* name", p)
		case low == ".buildgate":
			return fmt.Errorf("path %q is under .buildgate", p)
		case low == ".oracle":
			return fmt.Errorf("path %q is under .oracle", p)
		}
	}
	if strings.EqualFold(p, projectconfig.FileName) {
		return fmt.Errorf("path %q is the protected project config", p)
	}
	for _, prot := range extraProtected {
		if prot == "" {
			continue
		}
		if strings.HasSuffix(prot, "/") {
			if strings.HasPrefix(strings.ToLower(p), strings.ToLower(prot)) {
				return fmt.Errorf("path %q is under protected path %q", p, prot)
			}
			continue
		}
		if strings.EqualFold(p, prot) {
			return fmt.Errorf("path %q is a protected path", p)
		}
	}
	if len(policy.ExcludeHarnessByproducts([]string{p})) == 0 {
		return fmt.Errorf("path %q is a harness byproduct path", p)
	}
	if len(evidence.DependencyLockfilesTouched([]string{p})) > 0 {
		return fmt.Errorf("path %q is a dependency lockfile", p)
	}
	return nil
}

// HashBytes is the lowercase hex SHA-256 used everywhere in this package.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
