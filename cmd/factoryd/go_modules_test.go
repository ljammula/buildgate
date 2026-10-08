package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/sandbox"
)

func TestParseGoSumsNamesEachModuleVersionOnceAndDropsWhatIsNotOne(t *testing.T) {
	got := parseGoSums([]byte(`example.com/public/lib v1.2.3 h1:AAAA=
example.com/public/lib v1.2.3/go.mod h1:BBBB=
example.com/graph/only v0.9.0/go.mod h1:CCCC=
github.com/Corp/Private v2.0.0+incompatible h1:DDDD=
-bad/flag v1.0.0 h1:FFFF=
example.com/x v1.0.0;rm h1:GGGG=
example.com/../escape v1.0.0 h1:HHHH=
example.com/query v1 h1:IIII=
example.com/latest latest h1:JJJJ=
example.com/nohash v1.0.0 sha1:KKKK
not a go.sum line
`), []byte("example.com/public/lib v1.2.3 h1:AAAA=\nexample.com/second/file v0.1.0 h1:LLLL=\n"))
	want := []goModule{
		{Path: "example.com/graph/only", Version: "v0.9.0", ModSum: "h1:CCCC="},
		{Path: "example.com/public/lib", Version: "v1.2.3", ZipSum: "h1:AAAA=", ModSum: "h1:BBBB="},
		{Path: "example.com/second/file", Version: "v0.1.0", ZipSum: "h1:LLLL="},
		{Path: "github.com/Corp/Private", Version: "v2.0.0+incompatible", ZipSum: "h1:DDDD="},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGoSums = %+v, want %+v", got, want)
	}
	if got := escapeGoModulePath("github.com/Corp/Private"); got != "github.com/!corp/!private" {
		t.Errorf("escapeGoModulePath = %q", got)
	}
}

// goModH1 is the hash the go command writes in go.sum: checked against the
// line it wrote for the private-module fixture's dependency.
func TestGoModH1IsTheGoCommandsHash(t *testing.T) {
	content, err := os.ReadFile("../../testdata/fixtures/private-module-go-dep/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile("../../testdata/fixtures/private-module-go/go.sum")
	if err != nil {
		t.Fatal(err)
	}
	if want := goModH1(content); !strings.Contains(string(sum), "private.example/acme/shout v1.0.0/go.mod "+want) {
		t.Errorf("goModH1 = %s, which the fixture's go.sum does not have:\n%s", want, sum)
	}
}

func TestGoSettingsPrivateIsWhatOnlyThisMachineCanFetch(t *testing.T) {
	modules := []goModule{{Path: "github.com/corp/secret"}, {Path: "github.com/corp-other/x"}, {Path: "golang.org/x/text"}, {Path: "git.corp.example/team/lib/v2"}}
	paths := func(settings goSettings) []string {
		var out []string
		for _, m := range settings.private(modules) {
			out = append(out, m.Path)
		}
		return out
	}
	cases := []struct {
		name     string
		settings goSettings
		want     []string
	}{
		{"nothing private configured", goSettings{GOPROXY: goDefaultProxy}, nil},
		{"GOPRIVATE prefixes and globs", goSettings{GOPROXY: goDefaultProxy, GOPRIVATE: "github.com/corp,*.corp.example"}, []string{"github.com/corp/secret", "git.corp.example/team/lib/v2"}},
		{"GONOPROXY and GONOSUMDB count too", goSettings{GONOPROXY: "github.com/corp-other/*", GONOSUMDB: "none"}, []string{"github.com/corp-other/x"}},
		{"a GOPROXY of the operator's own may hold any of them", goSettings{GOPROXY: "https://artifactory.corp.example/go,direct"}, []string{"github.com/corp/secret", "github.com/corp-other/x", "golang.org/x/text", "git.corp.example/team/lib/v2"}},
	}
	for _, tc := range cases {
		if got := paths(tc.settings); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: private = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// goSumRepo is a git repository whose one commit holds the given files; it
// returns the repository and the commit.
func goSumRepo(t *testing.T, files map[string]string) (repo, sha string) {
	t.Helper()
	repo = t.TempDir()
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("add", "-A")
	git("commit", "-q", "-m", "fixture")
	return repo, git("rev-parse", "HEAD")
}

// fakeGo stands in for the operator's go command. It answers `go env` with
// settings, "fetches" each module it is asked for into GOMODCACHE with the
// content in served (but those in unreachable), and records its calls.
type fakeGo struct {
	t           *testing.T
	settings    goSettings
	served      map[string]fakeModule // "path@version"
	unreachable map[string]bool
	fetches     [][]string
	dirs        []string
}

type fakeModule struct{ mod, zipSum string }

func (f *fakeGo) run(_ context.Context, dir string, env []string, args ...string) ([]byte, error) {
	if args[0] == "env" {
		return json.Marshal(f.settings)
	}
	f.fetches = append(f.fetches, args)
	f.dirs = append(f.dirs, dir)
	cache := ""
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GOMODCACHE="); ok {
			cache = v
		}
	}
	var out bytes.Buffer
	for _, target := range args {
		path, version, ok := strings.Cut(target, "@")
		if !ok {
			continue
		}
		module := map[string]any{"Path": path, "Version": version}
		served, known := f.served[target]
		if f.unreachable[target] || !known {
			module["Error"] = target + ": 404 Not Found"
		} else {
			base := filepath.Join(cache, "cache", "download", escapeGoModulePath(path), "@v", escapeGoModulePath(version))
			if err := os.MkdirAll(filepath.Dir(base), 0o755); err != nil {
				f.t.Fatal(err)
			}
			files := map[string]string{".info": `{"Version":"` + version + `"}`, ".mod": served.mod}
			if args[0] == "mod" {
				files[".zip"], files[".ziphash"] = "zip of "+target, served.zipSum
			}
			for suffix, content := range files {
				if err := os.WriteFile(base+suffix, []byte(content), 0o644); err != nil {
					f.t.Fatal(err)
				}
			}
		}
		encoded, _ := json.Marshal(module)
		out.Write(encoded)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// goSumLines is the go.sum a repository has for these modules.
func goSumLines(modules map[string]fakeModule, modOnly ...string) string {
	only := map[string]bool{}
	for _, target := range modOnly {
		only[target] = true
	}
	var b strings.Builder
	for target, m := range modules {
		path, version, _ := strings.Cut(target, "@")
		if !only[target] {
			fmt.Fprintf(&b, "%s %s %s\n", path, version, m.zipSum)
		}
		fmt.Fprintf(&b, "%s %s/go.mod %s\n", path, version, goModH1([]byte(m.mod)))
	}
	return b.String()
}

var goProxyPolicy = &sandbox.RegistryProxyPolicy{Routes: sandbox.DefaultRegistryProxyRoutes()}

func TestRepositoryGoModuleDirServesThePrivateModulesOfTheBuildsCommit(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	t.Setenv("HOME", t.TempDir())
	dp := newTestDeps(t)
	private := map[string]fakeModule{
		"corp.example/team/lib@v1.2.3":     {mod: "module corp.example/team/lib\n", zipSum: "h1:AAAA="},
		"corp.example/Graph/only@v0.9.0":   {mod: "module corp.example/Graph/only\n", zipSum: "h1:BBBB="},
		"corp.example/services/api@v0.1.0": {mod: "module corp.example/services/api\n", zipSum: "h1:CCCC="},
	}
	fake := &fakeGo{t: t, settings: goSettings{GOPROXY: goDefaultProxy, GOPRIVATE: "corp.example"}, served: private}
	fakeHostOf(dp).goCommandFn = fake.run
	repo, sha := goSumRepo(t, map[string]string{
		"go.sum": goSumLines(map[string]fakeModule{
			"corp.example/team/lib@v1.2.3":   private["corp.example/team/lib@v1.2.3"],
			"corp.example/Graph/only@v0.9.0": private["corp.example/Graph/only@v0.9.0"],
		}, "corp.example/Graph/only@v0.9.0") + "golang.org/x/text v0.14.0 h1:PUBLIC=\n",
		"services/api/go.sum":      goSumLines(map[string]fakeModule{"corp.example/services/api@v0.1.0": private["corp.example/services/api@v0.1.0"]}),
		"vendor/x/go.sum":          "corp.example/vendored/away v1.0.0 h1:DDDD=\n",
		"internal/testdata/go.sum": "corp.example/test/data v1.0.0 h1:EEEE=\n",
	})
	// The working tree moves on after the commit the build starts from.
	if err := os.WriteFile(filepath.Join(repo, "go.sum"), []byte("corp.example/uncommitted/edit v9.9.9 h1:FFFF=\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Another repository's private module is already in the shared download cache.
	other := filepath.Join(goModulesRoot(), "cache", "cache", "download", "corp.example", "another", "repo", "@v")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "v1.0.0.zip"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	view := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy)
	if view == "" {
		t.Fatalf("no module directory: %s", out.String())
	}
	for file, present := range map[string]bool{
		"corp.example/team/lib/@v/v1.2.3.zip":         true,
		"corp.example/team/lib/@v/v1.2.3.mod":         true,
		"corp.example/!graph/only/@v/v0.9.0.mod":      true,
		"corp.example/!graph/only/@v/v0.9.0.zip":      false, // go.sum has only its go.mod
		"corp.example/services/api/@v/v0.1.0.zip":     true,
		"golang.org/x/text/@v/v0.14.0.zip":            false, // public: the proxy's upstream has it
		"corp.example/vendored/away/@v/v1.0.0.zip":    false, // under vendor/
		"corp.example/test/data/@v/v1.0.0.zip":        false, // under testdata/
		"corp.example/uncommitted/edit/@v/v9.9.9.zip": false, // not in the commit
		"corp.example/another/repo/@v/v1.0.0.zip":     false, // another repository's
	} {
		_, err := os.Stat(filepath.Join(view, filepath.FromSlash(file)))
		if (err == nil) != present {
			t.Errorf("%s present = %v, want %v", file, err == nil, present)
		}
	}
	if !strings.Contains(out.String(), "3 private module version(s) served") || strings.Contains(out.String(), "left out") {
		t.Errorf("output %q, want three served and none left out", out.String())
	}
	// The go command ran in an empty directory, never in the repository, and
	// was asked for the private module versions only.
	for i, dir := range fake.dirs {
		entries, _ := os.ReadDir(dir)
		if strings.HasPrefix(dir, repo) || len(entries) != 0 {
			t.Errorf("go call %d ran in %s", i, dir)
		}
	}
	asked := strings.Join(append(append([]string(nil), fake.fetches[0]...), fake.fetches[1]...), " ")
	if len(fake.fetches) != 2 || strings.Contains(asked, "golang.org/x/text") || strings.Contains(asked, "uncommitted") {
		t.Fatalf("go fetches = %v, want one download and one list of the private modules", fake.fetches)
	}
	// The same list again fetches nothing, and nothing changes the directory.
	out.Reset()
	if again := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy); again != view || len(fake.fetches) != 2 || out.Len() != 0 {
		t.Errorf("second call = %q, %d fetches, output %q; want the same view and no fetch", again, len(fake.fetches), out.String())
	}
}

// A module that could not be fetched is left out of this run's view only:
// the next run fetches again, and once it succeeds the view is the complete,
// reused one. A module the host fetched under a go.sum name with another
// hash is never served.
func TestRepositoryGoModuleDirRetriesAFailedFetchAndRefusesAWrongHash(t *testing.T) {
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	t.Setenv("HOME", t.TempDir())
	dp := newTestDeps(t)
	modules := map[string]fakeModule{
		"corp.example/a@v1.0.0": {mod: "module corp.example/a\n", zipSum: "h1:AAAA="},
		"corp.example/b@v1.0.0": {mod: "module corp.example/b\n", zipSum: "h1:BBBB="},
	}
	fake := &fakeGo{t: t, settings: goSettings{GOPRIVATE: "corp.example"}, served: modules, unreachable: map[string]bool{"corp.example/b@v1.0.0": true}}
	fakeHostOf(dp).goCommandFn = fake.run
	repo, sha := goSumRepo(t, map[string]string{"go.sum": goSumLines(modules)})

	var out bytes.Buffer
	partial := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy)
	if partial == "" || !strings.Contains(filepath.Base(partial), "partial-") || !strings.Contains(out.String(), "1 left out") || !strings.Contains(out.String(), "corp.example/b@v1.0.0") {
		t.Fatalf("with one module unreachable: view %q, output %q", partial, out.String())
	}
	if _, err := os.Stat(filepath.Join(partial, "corp.example", "a", "@v", "v1.0.0.zip")); err != nil {
		t.Errorf("the module that was fetched is not served: %v", err)
	}

	// The operator fixes access; the next run fetches again and completes.
	fake.unreachable = nil
	out.Reset()
	complete := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy)
	if complete == partial || strings.Contains(filepath.Base(complete), "partial-") || !strings.Contains(out.String(), "2 private module version(s) served") {
		t.Fatalf("after the fix: view %q, output %q; want a complete view", complete, out.String())
	}
	if _, err := os.Stat(filepath.Join(partial, "corp.example", "a", "@v", "v1.0.0.zip")); err != nil {
		t.Errorf("the earlier run's view was changed under it: %v", err)
	}

	// Another repository lists the same name with another hash: what the host
	// has under that name is not what its go.sum means.
	wrong := map[string]fakeModule{"corp.example/a@v1.0.0": {mod: "module corp.example/a\n", zipSum: "h1:ZZZZ="}}
	wrongRepo, wrongSHA := goSumRepo(t, map[string]string{"go.sum": goSumLines(wrong)})
	out.Reset()
	if view := repositoryGoModuleDir(dp, context.Background(), &out, wrongRepo, wrongSHA, goProxyPolicy); view != "" || !strings.Contains(out.String(), "none served") {
		t.Errorf("a module with another hash: view %q, output %q; want nothing served", view, out.String())
	}
}

func TestRepositoryGoModuleDirIsNothingWhenThereIsNothingToServe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dp := newTestDeps(t)
	var out bytes.Buffer
	modules := map[string]fakeModule{"corp.example/a@v1.0.0": {mod: "module corp.example/a\n", zipSum: "h1:AAAA="}}
	repo, sha := goSumRepo(t, map[string]string{"go.sum": goSumLines(modules)})
	fake := &fakeGo{t: t, settings: goSettings{GOPRIVATE: "corp.example"}, served: modules}

	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	fakeHostOf(dp).goCommandFn = func(context.Context, string, []string, ...string) ([]byte, error) {
		t.Error("the go command ran when nothing was to be fetched")
		return nil, nil
	}
	if got := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy); got != "" {
		t.Errorf("under FACTORYD_AUTOSTART=0 = %q", got)
	}
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	noGo, noGoSHA := goSumRepo(t, map[string]string{"README.md": "no modules\n"})
	for name, call := range map[string]func() string{
		"a repository with no go.sum": func() string {
			return repositoryGoModuleDir(dp, context.Background(), &out, noGo, noGoSHA, goProxyPolicy)
		},
		"a run with no registry proxy": func() string { return repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, nil) },
		"a registry proxy with no Go route": func() string {
			return repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, &sandbox.RegistryProxyPolicy{})
		},
	} {
		if got := call(); got != "" {
			t.Errorf("%s = %q", name, got)
		}
	}
	// Go settings that name nothing private: nothing to fetch, nothing said.
	fake.settings = goSettings{GOPROXY: goDefaultProxy}
	fakeHostOf(dp).goCommandFn = fake.run
	out.Reset()
	if got := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy); got != "" || len(fake.fetches) != 0 || out.Len() != 0 {
		t.Errorf("with nothing private configured = %q, %d fetches, output %q", got, len(fake.fetches), out.String())
	}
	// A machine with no usable go command: the run goes on without, and says so.
	fakeHostOf(dp).goCommandFn = func(context.Context, string, []string, ...string) ([]byte, error) {
		return nil, errors.New("no go command on this machine's PATH")
	}
	if got := repositoryGoModuleDir(dp, context.Background(), &out, repo, sha, goProxyPolicy); got != "" || !strings.Contains(out.String(), "go modules: none fetched") {
		t.Errorf("with no go command = %q, output %q", got, out.String())
	}
}

func TestFetchGoModulesAsksInBatchesAndKeepsTheOperatorsGoFlags(t *testing.T) {
	dp := newTestDeps(t)
	var modules []goModule
	for i := 0; i < goModulesPerCall+5; i++ {
		modules = append(modules, goModule{Path: fmt.Sprintf("corp.example/m%04d", i), Version: "v1.0.0", ZipSum: "h1:AAAA="})
	}
	var sizes []int
	var flags string
	fakeHostOf(dp).goCommandFn = func(_ context.Context, _ string, env []string, args ...string) ([]byte, error) {
		sizes = append(sizes, len(args)-3)
		for _, e := range env {
			if v, ok := strings.CutPrefix(e, "GOFLAGS="); ok {
				flags = v
			}
		}
		return nil, nil
	}
	if _, err := fetchGoModules(dp, context.Background(), t.TempDir(), goSettings{GOFLAGS: "-insecure"}, modules); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sizes, []int{goModulesPerCall, 5}) || flags != "-insecure -modcacherw" {
		t.Errorf("batches = %v, GOFLAGS = %q; want [%d 5] and the operator's flags kept", sizes, flags, goModulesPerCall)
	}
}

func TestOldPartialGoModuleViewsAreRemoved(t *testing.T) {
	views := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{"partial-old-1": 72 * time.Hour, "partial-new-2": time.Hour, "abcdef0123456789": 72 * time.Hour} {
		dir := filepath.Join(views, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(dir, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	removeOldPartialGoModuleViews(views, now)
	for name, kept := range map[string]bool{"partial-old-1": false, "partial-new-2": true, "abcdef0123456789": true} {
		if _, err := os.Stat(filepath.Join(views, name)); (err == nil) != kept {
			t.Errorf("%s kept = %v, want %v", name, err == nil, kept)
		}
	}
}
