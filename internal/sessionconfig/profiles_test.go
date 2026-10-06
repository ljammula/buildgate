package sessionconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// isolateProfiles points the config directory at a fresh temp dir and
// clears FACTORYD_PROFILE, returning the config directory.
func isolateProfiles(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	t.Setenv(ProfileEnv, "")
	dir := ConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeProfile(t *testing.T, dir, file, dataDir string) string {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolvePathOrderEnvThenActiveFileThenDefaults(t *testing.T) {
	dir := isolateProfiles(t)
	def := writeProfile(t, dir, "config.yml", "/d/default")
	work := writeProfile(t, dir, "work.yml", "/d/work")
	other := writeProfile(t, dir, "other.yml", "/d/other")

	if got, found, err := ResolvePath(); err != nil || !found || got != def {
		t.Fatalf("no active profile: got %q found=%v err=%v, want %s", got, found, err, def)
	}
	if err := SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	if got, _, err := ResolvePath(); err != nil || got != work {
		t.Fatalf("active-profile file: got %q err=%v, want %s", got, err, work)
	}
	t.Setenv(ProfileEnv, "other")
	if got, _, err := ResolvePath(); err != nil || got != other {
		t.Fatalf("env beats the file: got %q err=%v, want %s", got, err, other)
	}
	cfg, path, found, err := LoadDefault()
	if err != nil || !found || path != other || cfg.DataDir == nil || *cfg.DataDir != "/d/other" {
		t.Fatalf("LoadDefault follows the resolution: cfg=%v path=%q found=%v err=%v", cfg, path, found, err)
	}
}

func TestResolvePathMissingActiveFileFallsThrough(t *testing.T) {
	dir := isolateProfiles(t)
	def := writeProfile(t, dir, "config.yml", "/d/default")
	got, found, err := ResolvePath()
	if err != nil || !found || got != def {
		t.Fatalf("got %q found=%v err=%v, want default %s", got, found, err, def)
	}
	if err := os.WriteFile(filepath.Join(dir, ActiveProfileFile), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, found, err = ResolvePath(); err != nil || !found || got != def {
		t.Fatalf("empty active-profile file: got %q found=%v err=%v, want default %s", got, found, err, def)
	}
}

func TestResolvePathNoConfigAtAll(t *testing.T) {
	isolateProfiles(t)
	if got, found, err := ResolvePath(); err != nil || found {
		t.Fatalf("got %q found=%v err=%v, want not found and no error", got, found, err)
	}
}

func TestResolvePathActiveProfileWithoutFileIsAnError(t *testing.T) {
	dir := isolateProfiles(t)
	writeProfile(t, dir, "config.yml", "/d/default")
	if err := SetActiveProfile("ghost"); err != nil {
		t.Fatal(err)
	}
	_, found, err := ResolvePath()
	if err == nil || found {
		t.Fatalf("want an error, never a silent fallback to config.yml; got found=%v err=%v", found, err)
	}
	for _, want := range []string{`"ghost"`, "ghost.yml", "factoryd use"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, _, _, err := LoadDefault(); err == nil {
		t.Error("LoadDefault must surface the same error")
	}

	t.Setenv(ProfileEnv, "ghost2")
	if _, _, err := ResolvePath(); err == nil || !strings.Contains(err.Error(), "unset $"+ProfileEnv) {
		t.Errorf("env profile without a file: error %v should say to unset $%s", err, ProfileEnv)
	}
}

func TestResolveArgTurnsProfileNamesIntoPaths(t *testing.T) {
	dir := isolateProfiles(t)
	for in, want := range map[string]string{
		"work":                   filepath.Join(dir, "work.yml"),
		"default":                filepath.Join(dir, "config.yml"),
		"":                       "",
		"work.yml":               "work.yml",
		"./work":                 "./work",
		"sub/work":               "sub/work",
		"/abs/path/config.yml":   "/abs/path/config.yml",
		"/abs/path/noextension":  "/abs/path/noextension",
		"../up":                  "../up",
		"name.with.dots":         filepath.Join(dir, "name.with.dots.yml"),
		"bak-x.yml.bak-20260930": filepath.Join(dir, "bak-x.yml.bak-20260930.yml"),
	} {
		if got := ResolveArg(in); got != want {
			t.Errorf("ResolveArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadAcceptsProfileNameAndNamesMissingProfile(t *testing.T) {
	dir := isolateProfiles(t)
	writeProfile(t, dir, "work.yml", "/d/work")
	cfg, err := Load("work")
	if err != nil || cfg.DataDir == nil || *cfg.DataDir != "/d/work" {
		t.Fatalf("Load(work) = %v, %v", cfg, err)
	}
	_, err = Load("nope")
	if err == nil || !strings.Contains(err.Error(), "nope.yml") || !strings.Contains(err.Error(), "factoryd use") {
		t.Fatalf("Load(nope) error = %v, want the profile file and the fix named", err)
	}
}

func TestProfilesListsOnlyYmlFiles(t *testing.T) {
	dir := isolateProfiles(t)
	writeProfile(t, dir, "config.yml", "/d")
	writeProfile(t, dir, "zeta.yml", "/d")
	writeProfile(t, dir, "alpha.yml", "/d")
	writeProfile(t, dir, "alpha.yml.bak-20260930", "/d")
	writeProfile(t, dir, "notes.txt", "/d")
	got, err := Profiles()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"default", "alpha", "zeta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Profiles() = %v, want %v", got, want)
	}
}

func TestSetActiveProfileIsAtomicAndLeavesNoTempFile(t *testing.T) {
	dir := isolateProfiles(t)
	if err := SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	if err := SetActiveProfile("other"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ActiveProfileFile))
	if err != nil || string(b) != "other\n" {
		t.Fatalf("active-profile = %q, %v", b, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("config dir holds %d entries, want only active-profile", len(entries))
	}
}

func TestProfileNameOfPath(t *testing.T) {
	dir := isolateProfiles(t)
	for path, want := range map[string]string{
		filepath.Join(dir, "config.yml"):   "default",
		filepath.Join(dir, "work.yml"):     "work",
		filepath.Join(dir, "sub", "x.yml"): "",
		"/elsewhere/work.yml":              "",
	} {
		if got := ProfileNameOfPath(path); got != want {
			t.Errorf("ProfileNameOfPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestDefaultDataDirForKeepsNonProfileConfigsApart(t *testing.T) {
	dir := isolateProfiles(t)
	home := os.Getenv("HOME")
	if got, want := DefaultDataDirFor(filepath.Join(dir, "work.yml")), filepath.Join(home, "buildgate", "data"); got != want {
		t.Errorf("profile: DefaultDataDirFor = %q, want %q", got, want)
	}
	other := filepath.Join(t.TempDir(), "throwaway.yml")
	if got, want := DefaultDataDirFor(other), filepath.Join(filepath.Dir(other), "data"); got != want {
		t.Errorf("config outside ConfigDir: DefaultDataDirFor = %q, want %q", got, want)
	}
}
