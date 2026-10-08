package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/sessionconfig"
)

// profileFixture is an isolated config directory holding profiles, each
// with its own data dir.
type profileFixture struct {
	dir   string
	roots map[string]string // profile name -> data dir
}

const profileRoutesYAML = "roles:\n  execution:\n    model: m\n    harness: pifork\nmodels:\n  m:\n    id: gpt-x\n    routes: [r]\nroutes:\n  r:\n    credential_mode: static\n    upstream: https://r.example.invalid\n    credential_env: PROFILE_TEST_KEY\n"

// newProfileFixture writes one profile per name (config.yml for "default")
// under a fresh temp XDG dir, each with a distinct data dir, and clears
// FACTORYD_PROFILE.
func newProfileFixture(t *testing.T, names ...string) *profileFixture {
	t.Helper()
	isolateSessionConfig(t)
	t.Setenv(sessionconfig.ProfileEnv, "")
	f := &profileFixture{dir: sessionconfig.ConfigDir(), roots: map[string]string{}}
	root := t.TempDir()
	for _, name := range names {
		dataDir := filepath.Join(root, name+"-data")
		f.roots[name] = dataDir
		writeSessionConfig(t, sessionconfig.ProfilePath(name), "data_dir: "+dataDir+"\n"+profileRoutesYAML)
	}
	return f
}

func writeFreshHeartbeatWithoutAddress(t *testing.T, dataDir string, pid int, active string) {
	t.Helper()
	hb := daemonheartbeat.Heartbeat{
		PID:       pid,
		StartedAt: time.Now().Format(time.RFC3339Nano),
		UpdatedAt: time.Now().Format(time.RFC3339Nano),
	}
	if active != "" {
		hb.ActiveRequests = []string{active}
	}
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
}

// writeFreshWorkerHeartbeat is a worker's heartbeat: the worker one plus
// the Temporal address, job slots and every active request.
func writeFreshWorkerHeartbeat(t *testing.T, dataDir string, pid int, address string, slots int, active ...string) {
	t.Helper()
	hb := daemonheartbeat.Heartbeat{
		PID:             pid,
		StartedAt:       time.Now().Format(time.RFC3339Nano),
		UpdatedAt:       time.Now().Format(time.RFC3339Nano),
		ActiveRequests:  active,
		JobSlots:        slots,
		TemporalAddress: address,
	}
	if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), hb); err != nil {
		t.Fatal(err)
	}
}

func TestUseSaysWorkerRunningForAWorkerHeartbeat(t *testing.T) {
	dp := newTestDeps(t)
	f := newProfileFixture(t, "default")
	writeFreshWorkerHeartbeat(t, f.roots["default"], os.Getpid(), "localhost:7233", 3)
	var out bytes.Buffer
	if err := useRun(dp, nil, &out); err != nil {
		t.Fatalf("useRun: %v", err)
	}
	if !strings.Contains(out.String(), "worker running") {
		t.Errorf("want worker running:\n%s", out.String())
	}
}

func TestUseListsProfilesWithActiveMarkerRouteAndLiveness(t *testing.T) {
	dp := newTestDeps(t)
	f := newProfileFixture(t, "default", "work")
	if err := sessionconfig.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	writeFreshHeartbeatWithoutAddress(t, f.roots["work"], os.Getpid(), "")
	// A backup next to the profiles never counts as one.
	writeSessionConfig(t, filepath.Join(f.dir, "work.yml.bak-20260930"), "data_dir: /x\n")

	var out bytes.Buffer
	if err := useRun(dp, nil, &out); err != nil {
		t.Fatalf("useRun: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (default, work), got %d:\n%s", len(lines), out.String())
	}
	if !strings.HasPrefix(lines[0], "  default ") || !strings.HasPrefix(lines[1], "* work ") {
		t.Errorf("active marker wrong:\n%s", out.String())
	}
	for i, name := range []string{"default", "work"} {
		if !strings.Contains(lines[i], f.roots[name]) {
			t.Errorf("line %d lacks data dir %s: %s", i, f.roots[name], lines[i])
		}
		if !strings.Contains(lines[i], "static · gpt-x · pifork") {
			t.Errorf("line %d lacks route · model · harness: %s", i, lines[i])
		}
	}
	if !strings.Contains(lines[1], "worker running") || !strings.Contains(lines[0], "worker stopped") {
		t.Errorf("worker liveness wrong:\n%s", out.String())
	}
	if !strings.Contains(lines[0], "serve stopped") {
		t.Errorf("serve liveness wrong:\n%s", out.String())
	}
}

func TestUseWithoutProfilesSaysSo(t *testing.T) {
	dp := newTestDeps(t)
	newProfileFixture(t)
	var out bytes.Buffer
	if err := useRun(dp, nil, &out); err != nil || !strings.Contains(out.String(), "No profiles") {
		t.Fatalf("out=%q err=%v", out.String(), err)
	}
}

func TestUseSwitchWritesActiveProfileAndPrintsDataDir(t *testing.T) {
	dp := newTestDeps(t)
	f := newProfileFixture(t, "default", "work")
	var out bytes.Buffer
	if err := useRun(dp, []string{"work"}, &out); err != nil {
		t.Fatalf("use work: %v", err)
	}
	if name, _, _ := sessionconfig.ActiveProfile(); name != "work" {
		t.Fatalf("active profile = %q, want work", name)
	}
	if !strings.Contains(out.String(), "active profile: work") || !strings.Contains(out.String(), f.roots["work"]) {
		t.Errorf("output = %q", out.String())
	}
	if p, _, _ := sessionconfig.ResolvePath(); p != sessionconfig.ProfilePath("work") {
		t.Errorf("ResolvePath = %q, want work's file", p)
	}

	out.Reset()
	if err := useRun(dp, []string{"default"}, &out); err != nil {
		t.Fatalf("use default: %v", err)
	}
	if p, _, _ := sessionconfig.ResolvePath(); p != filepath.Join(f.dir, "config.yml") {
		t.Errorf("after `use default` ResolvePath = %q, want config.yml", p)
	}
}

func TestUseSwitchRefusesUnknownOrBrokenProfile(t *testing.T) {
	dp := newTestDeps(t)
	f := newProfileFixture(t, "default")
	writeSessionConfig(t, filepath.Join(f.dir, "broken.yml"), "no_such_key: 1\n")
	for _, name := range []string{"ghost", "broken", "a/b", "x.yml"} {
		var out bytes.Buffer
		if err := useRun(dp, []string{name}, &out); err == nil {
			t.Errorf("use %s succeeded, want an error", name)
		}
	}
	if _, err := os.Stat(filepath.Join(f.dir, sessionconfig.ActiveProfileFile)); err == nil {
		t.Error("a refused switch must not write active-profile")
	}
	if err := useRun(dp, []string{"a", "b"}, &bytes.Buffer{}); err == nil {
		t.Error("two arguments must be refused")
	}
}

func TestConfigResolutionFlagBeatsEnvBeatsActiveFile(t *testing.T) {
	f := newProfileFixture(t, "default", "work", "env", "flag")
	if err := sessionconfig.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	pathOf := func(flag string) string {
		t.Helper()
		_, path, found, err := loadConfigForPath(flag)
		if err != nil || !found {
			t.Fatalf("loadConfigForPath(%q): found=%v err=%v", flag, found, err)
		}
		return path
	}
	if got := pathOf(""); got != sessionconfig.ProfilePath("work") {
		t.Errorf("active file: got %s", got)
	}
	t.Setenv(sessionconfig.ProfileEnv, "env")
	if got := pathOf(""); got != sessionconfig.ProfilePath("env") {
		t.Errorf("env: got %s", got)
	}
	// -config given as a profile name beats both.
	if got := pathOf("flag"); got != sessionconfig.ProfilePath("flag") {
		t.Errorf("-config profile name: got %s", got)
	}
	// -config given as a path is used as written.
	explicit := filepath.Join(f.dir, "default-copy.yml")
	writeSessionConfig(t, explicit, "data_dir: /x\n")
	if got := pathOf(explicit); got != explicit {
		t.Errorf("-config path: got %s", got)
	}

	// The same funnel serves every resolver.
	settingsFor := func(flag string) string {
		t.Helper()
		dataDir := "data"
		flags := newUseFlags()
		if err := resolveDataDirFromSessionConfig(flags, &dataDir, flag); err != nil {
			t.Fatal(err)
		}
		return dataDir
	}
	if got := settingsFor("flag"); got != f.roots["flag"] {
		t.Errorf("resolveDataDirFromSessionConfig(-config flag) = %s, want %s", got, f.roots["flag"])
	}
	if got := settingsFor(""); got != f.roots["env"] {
		t.Errorf("resolveDataDirFromSessionConfig() = %s, want the env profile's %s", got, f.roots["env"])
	}
	if p, ok := resolveEffectiveConfigPath("work"); !ok || p != sessionconfig.ProfilePath("work") {
		t.Errorf("resolveEffectiveConfigPath(work) = %s, %v", p, ok)
	}
	if p, err := resolveServiceConfigPath("work"); err != nil || p != sessionconfig.ProfilePath("work") {
		t.Errorf("resolveServiceConfigPath(work) = %s, %v", p, err)
	}
	if p, err := resolveServiceConfigPath(""); err != nil || p != sessionconfig.ProfilePath("env") {
		t.Errorf("resolveServiceConfigPath() = %s, %v", p, err)
	}
}

func TestActiveProfileWithMissingFileErrorsInEveryResolver(t *testing.T) {
	newProfileFixture(t, "default")
	if err := sessionconfig.SetActiveProfile("ghost"); err != nil {
		t.Fatal(err)
	}
	dataDir := "data"
	err := resolveDataDirFromSessionConfig(newUseFlags(), &dataDir, "")
	if err == nil || !strings.Contains(err.Error(), "factoryd use") || !strings.Contains(err.Error(), "ghost") {
		t.Errorf("resolveDataDirFromSessionConfig error = %v", err)
	}
	if _, err := resolveServiceConfigPath(""); err == nil {
		t.Error("resolveServiceConfigPath: want an error, not a fallback to config.yml")
	}
	if _, err := loadSettingsForConfig(""); err == nil {
		t.Error("loadSettingsForConfig: want an error")
	}
}

func TestStatusFirstLineNamesProfileAndDataDir(t *testing.T) {
	f := newProfileFixture(t, "default", "work")
	if err := sessionconfig.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := statusMain(nil); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	first := strings.SplitN(out, "\n", 2)[0]
	want := "profile: work (" + sessionconfig.ProfilePath("work") + ") · data dir " + f.roots["work"]
	if first != want {
		t.Errorf("first line = %q, want %q", first, want)
	}

	// -config as a profile name switches the line and the data dir.
	out = captureStdout(t, func() {
		if err := statusMain([]string{"-config", "default"}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	if first = strings.SplitN(out, "\n", 2)[0]; !strings.HasPrefix(first, "profile: default (") || !strings.HasSuffix(first, "data dir "+f.roots["default"]) {
		t.Errorf("first line = %q", first)
	}
}

func TestStatusJSONCarriesProfileAndConfigPath(t *testing.T) {
	newProfileFixture(t, "default", "work")
	if err := sessionconfig.SetActiveProfile("work"); err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() {
		if err := statusMain([]string{"-json"}); err != nil {
			t.Fatalf("statusMain: %v", err)
		}
	})
	var got struct {
		Profile    string `json:"profile"`
		ConfigPath string `json:"config_path"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if got.Profile != "work" || got.ConfigPath != sessionconfig.ProfilePath("work") {
		t.Errorf("profile=%q config_path=%q", got.Profile, got.ConfigPath)
	}
}

func TestConfigureImagesAllProfilesAppliesToEveryProfile(t *testing.T) {
	f := newProfileFixture(t, "default", "work", "codex")
	writeSessionConfig(t, filepath.Join(f.dir, "work.yml.bak-20260930"), "data_dir: /backup\n")
	const ref = "localhost:5050/buildgate-worker@sha256:eeee"
	if err := configureImagesMain([]string{"-all-profiles", "-sandbox-image", ref}); err != nil {
		t.Fatalf("configureImagesMain: %v", err)
	}
	for _, name := range []string{"default", "work", "codex"} {
		cfg, err := sessionconfig.Load(sessionconfig.ProfilePath(name))
		if err != nil {
			t.Fatalf("Load %s: %v", name, err)
		}
		if cfg.SandboxImage == nil || *cfg.SandboxImage != ref {
			t.Errorf("profile %s SandboxImage = %v, want %s", name, cfg.SandboxImage, ref)
		}
		if cfg.DataDir == nil || *cfg.DataDir != f.roots[name] {
			t.Errorf("profile %s lost its data_dir: %v", name, cfg.DataDir)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.dir, "work.yml.bak-20260930")); strings.Contains(string(b), "sandbox_image") {
		t.Error("a backup file must not be touched")
	}
}

func TestConfigureImagesAllProfilesWithNoneCreatesDefaultAndRejectsConfig(t *testing.T) {
	newProfileFixture(t)
	const ref = "localhost:5050/buildgate-worker@sha256:ffff"
	if err := configureImagesMain([]string{"-all-profiles", "-config", "x", "-sandbox-image", ref}); err == nil {
		t.Error("-all-profiles with -config must be refused")
	}
	if err := configureImagesMain([]string{"-all-profiles", "-sandbox-image", ref}); err != nil {
		t.Fatal(err)
	}
	if cfg, err := sessionconfig.Load(sessionconfig.ProfilePath("default")); err != nil || cfg.SandboxImage == nil {
		t.Errorf("default config not created: %v %v", cfg, err)
	}
}

func TestConfigureImagesConfigAcceptsProfileName(t *testing.T) {
	newProfileFixture(t, "default", "work")
	const ref = "localhost:5050/buildgate-worker@sha256:9999"
	if err := configureImagesMain([]string{"-config", "work", "-sandbox-image", ref}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := sessionconfig.Load(sessionconfig.ProfilePath("work"))
	if cfg == nil || cfg.SandboxImage == nil || *cfg.SandboxImage != ref {
		t.Error("work not updated")
	}
	if cfg, _ := sessionconfig.Load(sessionconfig.ProfilePath("default")); cfg == nil || cfg.SandboxImage != nil {
		t.Error("default must not be touched")
	}
}

// A profile that sets no data_dir has one all the same: the default for a
// profile, so `use` lists it, and stop -all, inbox and restart find it.
func TestProfileWithoutDataDirHasTheDefaultOne(t *testing.T) {
	dp := newTestDeps(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	xdg := filepath.Join(home, "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv(sessionconfig.ProfileEnv, "")
	cfgDir := filepath.Join(xdg, "factoryd")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "loose.yml"), []byte("registry_proxy: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := sessionconfig.DefaultDataDir()

	var list bytes.Buffer
	if err := useList(dp, &list); err != nil {
		t.Fatalf("use: %v", err)
	}
	if !strings.Contains(list.String(), "loose") || !strings.Contains(list.String(), want) {
		t.Errorf("use should list the profile with its default data dir %s:\n%s", want, list.String())
	}

	var sw bytes.Buffer
	if err := useSwitch("loose", &sw); err != nil {
		t.Fatalf("use loose: %v", err)
	}
	if !strings.Contains(sw.String(), "data dir: "+want) || strings.Contains(sw.String(), "warning") {
		t.Errorf("use <name> should name the default data dir and warn of nothing:\n%s", sw.String())
	}

	profiles, err := loadProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if dirs, _ := distinctDataDirs(profiles); len(dirs) != 1 || dirs[0] != want {
		t.Errorf("data dirs = %v, want the default %s", dirs, want)
	}
}
