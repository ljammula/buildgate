package request

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"buildgate/internal/conformity"
	"buildgate/internal/oraclecanary"
)

// maxOracleManifestBytes bounds the MANIFEST.json read at validation.
const maxOracleManifestBytes = 1 << 20

// maxGoOracleBytes bounds one oracle file read for the Go static check.
const maxGoOracleBytes = 1 << 20

// ValidateRequestOracleDir returns EVERY reason the request-level oracle/
// directory would be refused at oracle_review approval, or nil when it is
// absent, empty (a skip) or fine. It is the one definition of "plan-independent
// validity", shared by requestOracleRelPaths (approval), the oracle API's
// listing and, through validateOracleManifest, the planning materializer, so a
// refusal that planning could raise is raised at oracle_review, where the
// operator can still edit oracle/. Only plan-dependent refusals (per-ticket
// caps, command scope across a ticket split) are left to planning.
//
// Checks: flat directory of regular, non-stray files; RUN_COMMAND.txt present
// and passing ValidateOracleRunCommand; MANIFEST.json present and parseable;
// every manifest entry with an oracle_file names a bare file that exists, has
// an in-range criterion_index whose text matches that criterion of the approved
// spec (the cross-check's normalisation), and no oracle file is left without an
// entry (it would never be materialized); and oraclecanary.CheckDir.
func ValidateRequestOracleDir(dataDir, id string) []string {
	dir := oracleDirPath(dataDir, id)
	info, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return []string{fmt.Sprintf("stat %s: %v", RequestOracleDirName, err)}
	}
	if !info.IsDir() {
		return []string{RequestOracleDirName + " is not a directory"}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []string{fmt.Sprintf("read %s: %v", RequestOracleDirName, err)}
	}
	if len(entries) == 0 {
		return nil
	}
	var problems []string
	files := map[string]bool{}
	hasRunCommand := false
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case entry.IsDir():
			problems = append(problems, fmt.Sprintf("%s is a subdirectory -- the oracle directory must be flat, one file per entry, no nesting", name))
		case strayOracleFileName(name):
			problems = append(problems, fmt.Sprintf("%s looks like an editor backup, swap or hidden file -- remove it before approving", name))
		case !entry.Type().IsRegular():
			problems = append(problems, fmt.Sprintf("%s is not a regular file (symlink or special file)", name))
		default:
			files[name] = true
			if name == TicketOracleRunCommandFilename {
				hasRunCommand = true
				command, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					problems = append(problems, fmt.Sprintf("read %s: %v", name, err))
				} else if err := ValidateOracleRunCommand(string(command)); err != nil {
					problems = append(problems, fmt.Sprintf("%s: %v", name, err))
				}
			}
		}
	}
	if !hasRunCommand {
		problems = append(problems, MissingRunCommandProblem(dataDir, id))
	}
	problems = append(problems, validateManifestFile(dataDir, id, dir, files)...)
	if len(problems) == 0 {
		if err := oraclecanary.CheckDir(dir); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

// validateManifestFile reads MANIFEST.json and the approved spec and applies
// validateOracleManifest, or reports what is missing.
func validateManifestFile(dataDir, id, dir string, files map[string]bool) []string {
	data, err := readRegularNoFollow(filepath.Join(dir, ManifestFileName), maxOracleManifestBytes)
	if err != nil {
		if errors.Is(err, ErrOracleFileNotFound) {
			return []string{fmt.Sprintf("no %s -- add %s mapping each oracle file to a spec criterion (criterion, oracle_file, criterion_index), so each ticket can be given the oracles for the criteria it owns", ManifestFileName, ManifestFileName)}
		}
		return []string{fmt.Sprintf("%s: %v", ManifestFileName, err)}
	}
	spec, err := os.ReadFile(filepath.Join(Dir(dataDir, id), specFileName))
	if err != nil {
		return []string{fmt.Sprintf("read approved spec: %v", err)}
	}
	criteria, err := SpecAcceptanceCriteria(string(spec))
	if err != nil {
		return []string{fmt.Sprintf("approved spec.md: %v", err)}
	}
	problems := validateOracleManifest(data, criteria, files)
	problems = append(problems, goOracleProblems(dataDir, id, dir, data, files)...)
	problems = append(problems, pythonOracleProblems(dir, data, files)...)
	problems = append(problems, pythonImportConsistencyProblems(dir, data, files)...)
	return append(problems, pythonModuleResolutionProblems(dataDir, id, dir, data, files, string(spec))...)
}

// goOracleProblems runs oraclecanary.CheckGoOracle over every manifest-named
// .go oracle: it must parse, and its package clause must fit the directory its
// target_path names in the request's workspace. Parse-only, so no model-written
// code runs on the host; an unloadable request record just skips the package
// half (workspace ""). Each file is checked once per distinct target_path.
func goOracleProblems(dataDir, id, dir string, manifest []byte, files map[string]bool) []string {
	entries, err := parseAllManifestEntries(manifest)
	if err != nil {
		return nil
	}
	workspace := ""
	if r, loadErr := Load(dataDir, id); loadErr == nil {
		workspace = r.Workspace
	}
	var problems []string
	checked := map[string]bool{}
	for _, e := range entries {
		name := e.oracleFile
		if !strings.HasSuffix(name, ".go") || !ValidOracleFileName(name) || !files[name] {
			continue
		}
		target := ""
		if v, ok := e.raw["target_path"]; ok && string(v) != "null" {
			_ = json.Unmarshal(v, &target)
		}
		key := name + "\x00" + target
		if checked[key] {
			continue
		}
		checked[key] = true
		src, readErr := readRegularNoFollow(filepath.Join(dir, name), maxGoOracleBytes)
		if readErr != nil {
			if errors.Is(readErr, ErrOracleFileTooLarge) {
				problems = append(problems, fmt.Sprintf("%s: too large to check (limit %d bytes)", name, maxGoOracleBytes))
			}
			continue
		}
		if err := oraclecanary.CheckGoOracle(workspace, target, src); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return problems
}

// pythonOracleProblems runs oraclecanary.CheckPythonOracle over every
// manifest-named Python oracle: it must parse, define at least one top-level
// test-named function, and import/call nothing forbidden. Parse-only (see
// CheckPythonOracle's own doc comment on why running python3 -I on the
// source's text is not executing the oracle); an oracle file named by no
// manifest entry is not checked here (validateOracleManifest already refuses
// it as unreferenced).
func pythonOracleProblems(dir string, manifest []byte, files map[string]bool) []string {
	entries, err := parseAllManifestEntries(manifest)
	if err != nil {
		return nil
	}
	var problems []string
	checked := map[string]bool{}
	for _, e := range entries {
		name := e.oracleFile
		if !strings.HasSuffix(name, ".py") || !ValidOracleFileName(name) || !files[name] || checked[name] {
			continue
		}
		checked[name] = true
		src, readErr := readRegularNoFollow(filepath.Join(dir, name), maxGoOracleBytes)
		if readErr != nil {
			if errors.Is(readErr, ErrOracleFileTooLarge) {
				problems = append(problems, fmt.Sprintf("%s: too large to check (limit %d bytes)", name, maxGoOracleBytes))
			}
			continue
		}
		if err := oraclecanary.CheckPythonOracle(src); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	return problems
}

// pythonImportConsistencyProblems refuses approval when the same imported
// name comes from two different modules across the manifest's own Python
// oracles -- oraclecanary.PythonImportConflicts's own doc comment has the
// live defect (2026-09-24) this closes. Same static check cmd/factoryd's
// own oracle-drafting job already runs advisory at draft time
// (pythonOracleImportConsistencyProblems); this is the real refusal, mirroring
// how pythonOracleProblems itself is advisory at draft time and a refusal here.
func pythonImportConsistencyProblems(dir string, manifest []byte, files map[string]bool) []string {
	entries, err := parseAllManifestEntries(manifest)
	if err != nil {
		return nil
	}
	imports := map[string]map[string]string{}
	for _, e := range entries {
		name := e.oracleFile
		if !strings.HasSuffix(name, ".py") || !ValidOracleFileName(name) || !files[name] {
			continue
		}
		if _, checked := imports[name]; checked {
			continue
		}
		src, readErr := readRegularNoFollow(filepath.Join(dir, name), maxGoOracleBytes)
		if readErr != nil {
			continue // reported by pythonOracleProblems
		}
		bindings, parseErr := oraclecanary.PythonOracleFromImports(src)
		if parseErr != nil {
			continue // reported by pythonOracleProblems
		}
		imports[name] = bindings
	}
	return oraclecanary.PythonImportConflicts(imports)
}

// pythonModuleResolutionProblems refuses approval when a manifest's own
// Python oracle imports a module that is neither a real module in the
// request's workspace nor named anywhere in the approved spec text passed as
// specText -- an invented module the drafter guessed rather than one
// grounded in the real repository or the human-approved spec (see
// oraclecanary.PythonUnresolvedModules's own doc comment). Same static check
// cmd/factoryd's own oracle-drafting job already runs advisory at draft time
// (pythonOracleUnresolvedImportProblems); this is the real refusal, mirroring
// how pythonImportConsistencyProblems itself is advisory at draft time and a
// refusal here.
func pythonModuleResolutionProblems(dataDir, id, dir string, manifest []byte, files map[string]bool, specText string) []string {
	entries, err := parseAllManifestEntries(manifest)
	if err != nil {
		return nil
	}
	workspace := ""
	if r, loadErr := Load(dataDir, id); loadErr == nil {
		workspace = r.Workspace
	}
	var problems []string
	checked := map[string]bool{}
	for _, e := range entries {
		name := e.oracleFile
		if !strings.HasSuffix(name, ".py") || !ValidOracleFileName(name) || !files[name] || checked[name] {
			continue
		}
		checked[name] = true
		src, readErr := readRegularNoFollow(filepath.Join(dir, name), maxGoOracleBytes)
		if readErr != nil {
			continue // reported by pythonOracleProblems
		}
		bindings, parseErr := oraclecanary.PythonOracleImports(src)
		if parseErr != nil {
			continue // reported by pythonOracleProblems
		}
		for _, module := range oraclecanary.PythonUnresolvedModules(bindings, workspace, specText) {
			problems = append(problems, fmt.Sprintf("oracle %s imports module %q, which neither exists in the repository nor is named in the approved spec -- reject with feedback to redraft", name, module))
		}
	}
	return problems
}

// validateOracleManifest returns every plan-independent problem of a
// request-level MANIFEST.json against the spec's criteria and the set of oracle
// file names present (pinned, at planning). MANIFEST.json and RUN_COMMAND.txt in
// files are not oracle files.
func validateOracleManifest(data []byte, criteria []string, files map[string]bool) []string {
	entries, err := parseAllManifestEntries(data)
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	referenced := map[string]bool{}
	for i, e := range entries {
		if e.oracleFile == "" {
			continue
		}
		label := fmt.Sprintf("%s entry %d (%q)", ManifestFileName, i, e.oracleFile)
		referenced[e.oracleFile] = true
		if !ValidOracleFileName(e.oracleFile) || e.oracleFile == ManifestFileName || e.oracleFile == TicketOracleRunCommandFilename {
			problems = append(problems, fmt.Sprintf("%s: oracle_file must be a bare, non-hidden file name other than %s and %s", label, ManifestFileName, TicketOracleRunCommandFilename))
			continue
		}
		if !files[e.oracleFile] {
			problems = append(problems, fmt.Sprintf("%s: oracle_file is not a file in the oracle directory", label))
		}
		switch {
		case !e.hasIndex:
			problems = append(problems, fmt.Sprintf("%s: no criterion_index -- an entry must say which spec criterion (1-based) it checks so it can be assigned to a ticket", label))
		case e.index < 1 || e.index > len(criteria):
			problems = append(problems, fmt.Sprintf("%s: criterion_index %d is outside the spec's %d acceptance criteria", label, e.index, len(criteria)))
		case conformity.NormalizeCriterion(e.criterion) != conformity.NormalizeCriterion(criteria[e.index-1]):
			problems = append(problems, fmt.Sprintf("%s: criterion text does not match spec criterion %d (compared exactly, whitespace included) -- %s; fix criterion_index if the oracle was drafted against different numbering, or fix the text", label, e.index, criterionDiff(e.criterion, criteria[e.index-1])))
		}
	}
	oracleFiles := 0
	for name := range files {
		if name != ManifestFileName && name != TicketOracleRunCommandFilename {
			oracleFiles++
		}
	}
	if oracleFiles > MaxTicketOracleFiles {
		problems = append(problems, fmt.Sprintf("the oracle directory holds %d oracle files, over the %d file cap: remove some (or re-draft) before approving", oracleFiles, MaxTicketOracleFiles))
	}
	var unreferenced []string
	for name := range files {
		if name != ManifestFileName && name != TicketOracleRunCommandFilename && !referenced[name] {
			unreferenced = append(unreferenced, name)
		}
	}
	sort.Strings(unreferenced)
	if len(unreferenced) > 0 {
		problems = append(problems, fmt.Sprintf("oracle file(s) %s are named by no %s entry, so no ticket would ever receive them -- add an entry or remove the file", strings.Join(unreferenced, ", "), ManifestFileName))
	}
	return problems
}

// criterionDiff renders a compact, exact difference between a MANIFEST.json
// criterion text and the spec's: the first differing position of the two
// whitespace-normalised strings and a short %q snippet of each around it, so
// a whitespace or backtick-only difference is visible.
func criterionDiff(manifest, spec string) string {
	a := []rune(conformity.NormalizeCriterion(manifest))
	b := []rune(conformity.NormalizeCriterion(spec))
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return fmt.Sprintf("first difference at character %d: manifest has %s, spec has %s", i+1, diffSnippet(a, i), diffSnippet(b, i))
}

func diffSnippet(r []rune, at int) string {
	const before, after = 12, 24
	start := at - before
	if start < 0 {
		start = 0
	}
	if at >= len(r) {
		return fmt.Sprintf("%q (ends here)", string(r[start:]))
	}
	end := at + after
	if end > len(r) {
		end = len(r)
	}
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "..."
	}
	if end < len(r) {
		suffix = "..."
	}
	return fmt.Sprintf("%q", prefix+string(r[start:end])+suffix)
}
