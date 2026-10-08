package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// loadConfigAt writes content as a config at path and loads it.
func loadConfigAt(t *testing.T, path, content string) *Config {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

// One config means one data dir, from any working directory, whatever the
// file says about it.
func TestEffectiveDataDirIsFixedByTheConfigNotTheWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	profile := filepath.Join(ConfigDir(), "config.yml")
	elsewhere := filepath.Join(home, "throwaway", "try.yml")
	cases := []struct {
		name, path, content, want string
		isDefault                 bool
	}{
		{"a profile that sets none", profile, "registry_proxy: false\n", filepath.Join(home, "buildgate", "data"), true},
		{"an empty value", profile, "data_dir: \"\"\n", filepath.Join(home, "buildgate", "data"), true},
		{"a bare key", profile, "data_dir:\n", filepath.Join(home, "buildgate", "data"), true},
		{"a config elsewhere that sets none", elsewhere, "registry_proxy: false\n", filepath.Join(home, "throwaway", "data"), true},
		{"an absolute value", profile, "data_dir: /srv/records\n", "/srv/records", false},
		{"a home-relative value", profile, "data_dir: ~/records\n", filepath.Join(home, "records"), false},
		{"a relative value is relative to the config", elsewhere, "data_dir: records\n", filepath.Join(home, "throwaway", "records"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadConfigAt(t, tc.path, tc.content)
			got := cfg.EffectiveDataDir()
			if got != tc.want || cfg.DataDirIsDefault() != tc.isDefault {
				t.Fatalf("EffectiveDataDir = %q (default %v), want %q (default %v)", got, cfg.DataDirIsDefault(), tc.want, tc.isDefault)
			}
			t.Chdir(t.TempDir())
			again, err := Load(tc.path)
			if err != nil || again.EffectiveDataDir() != tc.want {
				t.Errorf("from another directory = %q, %v; want %q", again.EffectiveDataDir(), err, tc.want)
			}
		})
	}
}

// The same file reached through a symlinked config directory, or by a
// relative path, has the same data dir.
func TestEffectiveDataDirDoesNotDependOnHowThePathIsSpelled(t *testing.T) {
	home := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(home); err == nil {
		home = resolved
	}
	t.Setenv("HOME", home)
	real := filepath.Join(home, "dot", "factoryd")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	loadConfigAt(t, filepath.Join(real, "config.yml"), "registry_proxy: false\n")
	if err := os.MkdirAll(filepath.Join(home, ".config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, ConfigDir()); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "buildgate", "data")
	t.Chdir(real)
	for _, path := range []string{filepath.Join(ConfigDir(), "config.yml"), filepath.Join(real, "config.yml"), "config.yml"} {
		cfg, err := Load(path)
		if err != nil || cfg.EffectiveDataDir() != want {
			t.Errorf("Load(%q).EffectiveDataDir() = %q, %v; want %q", path, cfg.EffectiveDataDir(), err, want)
		}
	}
}

// Loading a config and writing it back adds no data_dir: the default is what
// commands use, never something a rewrite of the file starts to pin.
func TestLoadedConfigWritesBackNoDataDirItWasNotGiven(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfg := loadConfigAt(t, filepath.Join(ConfigDir(), "config.yml"), "registry_proxy: false\n")
	_ = cfg.EffectiveDataDir()
	out, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "data_dir") {
		t.Errorf("the rewritten config gained data_dir:\n%s", out)
	}
}
