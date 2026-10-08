package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

const (
	// goModulesTimeout bounds one fetch of a repository's modules on the host.
	goModulesTimeout = 20 * time.Minute
	// goModulesPerCall bounds one go command's arguments, well under the
	// operating system's limit on a command line.
	goModulesPerCall = 400
	// goModulesPartialMaxAge is how long a view that lacks a module is kept:
	// longer than any one run that may still have it mounted.
	goModulesPartialMaxAge = 48 * time.Hour
	// goDefaultProxy is GOPROXY when the operator set none.
	goDefaultProxy = "https://proxy.golang.org,direct"
)

// goModule is one module version a go.sum names, with the hashes go.sum
// gives it: ZipSum of its source, "" when go.sum has only its go.mod line
// (the module is in the graph and no build reads its source), and ModSum of
// its go.mod.
type goModule struct {
	Path, Version  string
	ZipSum, ModSum string
}

// goModulePathPattern and goModuleVersionPattern are what a go.sum line may
// name. Both are passed to the go command on the host, so anything else (a
// leading "-", a space, a version query such as "v1", a control character) is
// dropped, not escaped.
var (
	goModulePathPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~/-]*$`)
	goModuleVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.+-]*)?$`)
	goModuleHashPattern    = regexp.MustCompile(`^h1:[A-Za-z0-9+/]+=*$`)
)

// parseGoSums reads go.sum files, "<module> <version>[/go.mod] <hash>" per
// line, into one sorted list with each module version once.
func parseGoSums(sums ...[]byte) []goModule {
	found := map[[2]string]*goModule{}
	for _, data := range sums {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			version, modOnly := strings.CutSuffix(fields[1], "/go.mod")
			if !goModulePathPattern.MatchString(fields[0]) || strings.Contains(fields[0], "..") ||
				!goModuleVersionPattern.MatchString(version) || !goModuleHashPattern.MatchString(fields[2]) {
				continue
			}
			key := [2]string{fields[0], version}
			if found[key] == nil {
				found[key] = &goModule{Path: fields[0], Version: version}
			}
			if modOnly {
				found[key].ModSum = fields[2]
			} else {
				found[key].ZipSum = fields[2]
			}
		}
	}
	modules := make([]goModule, 0, len(found))
	for _, m := range found {
		modules = append(modules, *m)
	}
	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Path != modules[j].Path {
			return modules[i].Path < modules[j].Path
		}
		return modules[i].Version < modules[j].Version
	})
	return modules
}

// repositoryGoSums returns the content of every go.sum in commit sha of repo,
// but those under a vendor or testdata directory: a monorepo's modules each
// have their own. The commit is the one the build starts from, not the
// checkout's working tree, which may be another branch or carry an edit.
func repositoryGoSums(repo, sha string) [][]byte {
	paths, err := runner.GitTreeFilesNamed(repo, sha, "go.sum")
	if err != nil {
		return nil
	}
	var sums [][]byte
	for _, name := range paths {
		if strings.Contains("/"+name, "/vendor/") || strings.Contains("/"+name, "/testdata/") {
			continue
		}
		if content, existed, err := runner.GitShowFile(repo, sha, name); err == nil && existed {
			sums = append(sums, []byte(content))
		}
	}
	return sums
}

// goSettings are the operator's Go settings that decide which modules only
// this machine can fetch.
type goSettings struct {
	GOPRIVATE, GONOPROXY, GONOSUMDB, GOPROXY, GOFLAGS string
}

// readGoSettings asks the operator's go command for them (`go env`, which
// reads the environment and what `go env -w` wrote).
func readGoSettings(dp *deps, ctx context.Context) (goSettings, error) {
	out, err := dp.host.goCommand(ctx, os.TempDir(), nil, "env", "-json", "GOPRIVATE", "GONOPROXY", "GONOSUMDB", "GOPROXY", "GOFLAGS")
	if err != nil {
		return goSettings{}, fmt.Errorf("go env: %v: %s", err, hostcontrol.LastLine(string(out)))
	}
	var settings goSettings
	if err := json.Unmarshal(out, &settings); err != nil {
		return goSettings{}, errors.New("go env printed no settings")
	}
	return settings, nil
}

// private returns the modules only this machine's settings can fetch. With
// the default GOPROXY those are the ones GOPRIVATE, GONOPROXY or GONOSUMDB
// name: every other module is on the public proxy, which the build's own
// registry proxy reaches. With a GOPROXY of the operator's own (a company
// proxy), any module may be on it alone, so all are.
func (s goSettings) private(modules []goModule) []goModule {
	if s.GOPROXY != "" && s.GOPROXY != goDefaultProxy {
		return modules
	}
	var patterns []string
	for _, list := range []string{s.GOPRIVATE, s.GONOPROXY, s.GONOSUMDB} {
		for _, pattern := range strings.Split(list, ",") {
			if pattern = strings.TrimSpace(pattern); pattern != "" && pattern != "none" {
				patterns = append(patterns, pattern)
			}
		}
	}
	var private []goModule
	for _, m := range modules {
		if matchesGoPathPrefix(patterns, m.Path) {
			private = append(private, m)
		}
	}
	return private
}

// matchesGoPathPrefix is the go command's rule for GOPRIVATE and its
// relatives: a pattern matches when it matches, as a glob, a leading run of
// the module path's elements.
func matchesGoPathPrefix(patterns []string, modulePath string) bool {
	elements := strings.Split(modulePath, "/")
	for _, pattern := range patterns {
		n := strings.Count(pattern, "/") + 1
		if n > len(elements) {
			continue
		}
		if ok, err := path.Match(pattern, strings.Join(elements[:n], "/")); err == nil && ok {
			return true
		}
	}
	return false
}

// escapeGoModulePath is the Go module proxy protocol's escaping of a module
// path or version: an upper-case letter is "!" and its lower case.
func escapeGoModulePath(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// goModulesRoot is where fetched modules live: under the data root, the one
// directory the Docker VM shares read-write, so the proxy container can mount
// a view of it.
func goModulesRoot() string {
	return filepath.Join(sessionconfig.DataRoot(), "gomodules")
}

// repositoryGoModuleDir returns a directory holding the private modules the
// go.sum files of repo's commit sha name, laid out as Go module proxy paths,
// for the registry proxy to answer the build's module requests from
// (sandbox.RegistryProxySpec.GoModuleDir); "" when there is nothing to serve.
//
// The modules are fetched here, on the host, by the operator's own go command
// with the operator's own Go settings and credentials (GOPROXY, GOPRIVATE,
// git's credential helper): whatever lets the operator build the repository
// lets a build of it have its private modules, and no credential enters a
// container. The go command is given module paths and versions read from
// go.sum and nothing else of the repository: it runs in an empty directory
// and never reads a file the repository holds. What it fetched is served only
// when it has the hash go.sum gives it.
//
// A module that cannot be fetched, or whose hash differs, is named and left
// out, and the next run tries again; a build that needs it fails as it would
// have. Nothing is fetched under FACTORYD_AUTOSTART=0 or for a run whose
// registry proxy has no Go route.
func repositoryGoModuleDir(dp *deps, ctx context.Context, w io.Writer, repo, sha string, policy *sandbox.RegistryProxyPolicy) string {
	if !hostcontrol.AutostartEnabled() || policy == nil || !policy.ServesGoModules() {
		return ""
	}
	listed := parseGoSums(repositoryGoSums(repo, sha)...)
	if len(listed) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, goModulesTimeout)
	defer cancel()
	settings, err := readGoSettings(dp, ctx)
	if err != nil {
		fmt.Fprintf(w, "go modules: none fetched for the build: %s. A private module go.sum lists will not resolve in the sandbox\n", sanitize.Line(err.Error()))
		return ""
	}
	modules := settings.private(listed)
	if len(modules) == 0 {
		return ""
	}
	views := filepath.Join(goModulesRoot(), "views")
	removeOldPartialGoModuleViews(views, time.Now())
	complete := filepath.Join(views, goModulesKey(modules))
	if info, err := os.Stat(complete); err == nil && info.IsDir() {
		return complete
	}
	fmt.Fprintf(w, "go modules: fetching %d private module version(s) go.sum lists, with this machine's Go settings\n", len(modules))
	cache := filepath.Join(goModulesRoot(), "cache")
	failed, err := fetchGoModules(dp, ctx, cache, settings, modules)
	if err != nil {
		fmt.Fprintf(w, "go modules: none fetched for the build: %s\n", sanitize.Line(err.Error()))
		return ""
	}
	view, missing, err := buildGoModuleView(filepath.Join(cache, "cache", "download"), views, complete, modules)
	if err != nil {
		fmt.Fprintf(w, "go modules: none served to the build: %s\n", sanitize.Line(err.Error()))
		return ""
	}
	if left := mergeSorted(failed, missing); len(left) > 0 {
		fmt.Fprintf(w, "go modules: %d left out, and a build that needs one fails on it (fix this machine's access, then retry): %s\n", len(left), sanitize.Line(strings.Join(left, ", ")))
	}
	fmt.Fprintf(w, "go modules: %d private module version(s) served to the build\n", len(modules)-len(mergeSorted(failed, missing)))
	return view
}

// mergeSorted is the sorted union of a and b.
func mergeSorted(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string(nil), a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// goModulesKey names the view of exactly these module versions and hashes.
func goModulesKey(modules []goModule) string {
	sum := sha256.New()
	for _, m := range modules {
		fmt.Fprintf(sum, "%s %s %s %s\n", m.Path, m.Version, m.ZipSum, m.ModSum)
	}
	return fmt.Sprintf("%x", sum.Sum(nil)[:8])
}

// fetchGoModules downloads modules into the module cache at cache: the zip
// of each one a build reads, and only go.mod of the others. It returns the
// module versions the go command could not fetch.
func fetchGoModules(dp *deps, ctx context.Context, cache string, settings goSettings, modules []goModule) (failed []string, err error) {
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, err
	}
	// An empty directory: the go command resolves the named versions and
	// reads no go.mod, go.work or vendor directory of anyone's.
	empty, err := os.MkdirTemp("", "buildgate-gomodules-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(empty)
	// The operator's GOFLAGS stay; -modcacherw keeps the cache removable.
	env := []string{"GOMODCACHE=" + cache, "GOFLAGS=" + strings.TrimSpace(settings.GOFLAGS+" -modcacherw"), "GOWORK=off", "GO111MODULE=on", "GIT_TERMINAL_PROMPT=0"}
	var zips, mods []string
	for _, m := range modules {
		if m.ZipSum != "" {
			zips = append(zips, m.Path+"@"+m.Version)
		} else {
			mods = append(mods, m.Path+"@"+m.Version)
		}
	}
	for _, call := range []struct{ args, targets []string }{
		{[]string{"mod", "download", "-json"}, zips},
		{[]string{"list", "-m", "-e", "-json"}, mods},
	} {
		for start := 0; start < len(call.targets); start += goModulesPerCall {
			batch := call.targets[start:min(start+goModulesPerCall, len(call.targets))]
			out, runErr := dp.host.goCommand(ctx, empty, env, append(append([]string(nil), call.args...), batch...)...)
			reported, parseErr := goModuleErrors(out)
			if parseErr != nil {
				if runErr != nil {
					return nil, fmt.Errorf("go %s: %v: %s", call.args[0], runErr, hostcontrol.LastLine(string(out)))
				}
				return nil, parseErr
			}
			failed = append(failed, reported...)
		}
	}
	sort.Strings(failed)
	return failed, nil
}

// goModuleErrors reads the go command's -json stream of module objects and
// returns "path@version" of each one that carries an error.
func goModuleErrors(out []byte) ([]string, error) {
	var failed []string
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var module struct {
			Path, Version string
			Error         json.RawMessage
		}
		if err := dec.Decode(&module); err != nil {
			return nil, errors.New("the go command's output is not the module list it was asked for")
		}
		if len(module.Error) > 0 && string(module.Error) != "null" && string(module.Error) != `""` {
			failed = append(failed, module.Path+"@"+module.Version)
		}
	}
	return failed, nil
}

// goModH1 is the go.sum hash of a go.mod file's content.
func goModH1(content []byte) string {
	file := sha256.Sum256(content)
	summary := sha256.Sum256([]byte(fmt.Sprintf("%x  go.mod\n", file)))
	return "h1:" + base64.StdEncoding.EncodeToString(summary[:])
}

// verifiedGoModuleFiles returns the proxy-protocol files of m in the download
// cache, or nil when one is missing or does not have the hash go.sum gives
// it: what a build is served is what its go.sum names, whatever the host
// fetched under that name.
func verifiedGoModuleFiles(download string, m goModule) []string {
	base := filepath.Join(download, escapeGoModulePath(m.Path), "@v", escapeGoModulePath(m.Version))
	files := []string{base + ".info", base + ".mod"}
	mod, err := os.ReadFile(base + ".mod")
	if err != nil || (m.ModSum != "" && goModH1(mod) != m.ModSum) {
		return nil
	}
	if m.ZipSum != "" {
		recorded, err := os.ReadFile(base + ".ziphash")
		if err != nil || strings.TrimSpace(string(recorded)) != m.ZipSum {
			return nil
		}
		files = append(files, base+".zip", base+".ziphash")
	}
	for _, file := range files {
		if info, err := os.Lstat(file); err != nil || !info.Mode().IsRegular() {
			return nil
		}
	}
	return files
}

// buildGoModuleView makes the directory a run's registry proxy serves: for
// each module version, its verified files from the download cache,
// hard-linked (copied when a link cannot be made). It holds these modules and
// no others, so a build is served what its own go.sum lists and nothing
// another repository fetched.
//
// A view with every module is renamed to complete, where later runs with the
// same list find it; nothing ever changes a directory a proxy may have
// mounted. A view that lacks a module (missing names them) is this run's
// alone, so the next run fetches again.
func buildGoModuleView(download, views, complete string, modules []goModule) (view string, missing []string, err error) {
	if err := os.MkdirAll(views, 0o755); err != nil {
		return "", nil, err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", nil, err
	}
	building := filepath.Join(views, fmt.Sprintf("partial-%s-%x", filepath.Base(complete), suffix))
	for _, m := range modules {
		files := verifiedGoModuleFiles(download, m)
		if files == nil {
			missing = append(missing, m.Path+"@"+m.Version)
			continue
		}
		dir := filepath.Join(building, escapeGoModulePath(m.Path), "@v")
		if err := mkdirAllReadable(building, dir); err != nil {
			return "", nil, err
		}
		for _, file := range files {
			if err := linkOrCopy(file, filepath.Join(dir, filepath.Base(file))); err != nil {
				return "", nil, err
			}
		}
	}
	if len(missing) == len(modules) {
		_ = os.RemoveAll(building)
		return "", nil, fmt.Errorf("none of the %d could be fetched with the hash go.sum gives it", len(modules))
	}
	if len(missing) > 0 {
		return building, missing, nil
	}
	if err := os.Rename(building, complete); err != nil {
		// Another run completed the same view first: it is the same content.
		_ = os.RemoveAll(building)
		if info, statErr := os.Stat(complete); statErr != nil || !info.IsDir() {
			return "", nil, err
		}
	}
	return complete, nil, nil
}

// mkdirAllReadable creates dir under root with every directory from root
// down readable by the proxy container's user, whatever the umask.
func mkdirAllReadable(root, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for d := dir; strings.HasPrefix(d, root); d = filepath.Dir(d) {
		if err := os.Chmod(d, 0o755); err != nil {
			return err
		}
		if d == root {
			break
		}
	}
	return nil
}

// linkOrCopy makes target the same file as source, readable by the proxy
// container's user.
func linkOrCopy(source, target string) error {
	if err := os.Chmod(source, 0o644); err != nil {
		return err
	}
	if os.Link(source, target) == nil {
		return nil
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	return os.Chmod(target, 0o644)
}

// removeOldPartialGoModuleViews deletes the views that lack a module and are
// older than any run that could still have them mounted.
func removeOldPartialGoModuleViews(views string, now time.Time) {
	entries, err := os.ReadDir(views)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "partial-") {
			continue
		}
		if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > goModulesPartialMaxAge {
			_ = os.RemoveAll(filepath.Join(views, entry.Name()))
		}
	}
}

// goCommandPaths are where a go command is looked for when PATH has none: a
// worker under launchd has a short PATH, and the official installer's
// directory is not on it.
var goCommandPaths = []string{"/usr/local/go/bin/go", "/opt/homebrew/bin/go", "/usr/local/bin/go"}

// goCommand is the real hostBoundary go command: the operator's own, with
// the operator's environment plus env, run in dir.
func (impl realHost) goCommand(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	binary, err := exec.LookPath("go")
	for _, candidate := range goCommandPaths {
		if err == nil {
			break
		}
		if _, statErr := os.Stat(candidate); statErr == nil {
			binary, err = candidate, nil
		}
	}
	if err != nil {
		return nil, errors.New("no go command on this machine's PATH or in /usr/local/go/bin")
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return stderr.Bytes(), err
	}
	return out, err
}
