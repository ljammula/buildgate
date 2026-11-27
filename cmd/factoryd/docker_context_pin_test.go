package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/hostcontrol/hostcontroltest"
	"buildgate/internal/sessionconfig"
)

func TestConfigArgOf(t *testing.T) {
	cases := map[string][]string{
		"":          {"status"},
		"a.yml":     {"worker", "-config", "a.yml", "-data-dir", "x"},
		"b.yml":     {"serve", "--config=b.yml"},
		"work":      {"submit", "-config=work", "-goal", "x"},
		"c.yml ":    {"run", "--config", "c.yml "},
		"last-wins": {"doctor", "-x", "-config"},
	}
	for want, args := range cases {
		if want == "last-wins" {
			want = ""
		}
		if got := configArgOf(args); got != want {
			t.Errorf("configArgOf(%q) = %q, want %q", args, got, want)
		}
	}
}

// pinnedConfig writes a session config that pins dockerContext ("" sets no
// key).
func pinnedConfig(t *testing.T, dockerContext string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	body := "registry_proxy: false\n"
	if dockerContext != "" {
		body += "docker_context: " + dockerContext + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPinDockerContextExportsTheConfigsContext(t *testing.T) {
	dp := newTestDeps(t)
	path := pinnedConfig(t, "colima-buildgate")
	if err := pinDockerContext(dp, "worker", []string{"worker", "-config", path}); err != nil {
		t.Fatal(err)
	}
	if got := fakeHostOf(dp).env["DOCKER_CONTEXT"]; got != "colima-buildgate" {
		t.Errorf("DOCKER_CONTEXT = %q, want colima-buildgate", got)
	}
}

func TestPinDockerContextWithoutTheKeyChangesNothing(t *testing.T) {
	dp := newTestDeps(t)
	path := pinnedConfig(t, "")
	if err := pinDockerContext(dp, "worker", []string{"worker", "-config", path}); err != nil {
		t.Fatal(err)
	}
	if len(fakeHostOf(dp).env) != 0 {
		t.Errorf("env = %v, want nothing set", fakeHostOf(dp).env)
	}
}

func TestPinDockerContextSkipsTheCommandsThatFixTheConfig(t *testing.T) {
	dp := newTestDeps(t)
	fakeHostOf(dp).env["DOCKER_HOST"] = "tcp://elsewhere:2375"
	path := pinnedConfig(t, "colima-buildgate")
	for _, command := range []string{"help", "version", "use", "init-config"} {
		if err := pinDockerContext(dp, command, []string{command, "-config", path}); err != nil {
			t.Errorf("%s: %v", command, err)
		}
	}
	if _, set := fakeHostOf(dp).env["DOCKER_CONTEXT"]; set {
		t.Error("an exempt command pinned the context")
	}
}

func TestApplyDockerContextPinRefusals(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		refused []string
	}{
		{"nothing set", map[string]string{}, nil},
		{"the same context", map[string]string{"DOCKER_CONTEXT": "colima-buildgate"}, nil},
		{"an empty DOCKER_HOST", map[string]string{"DOCKER_HOST": ""}, nil},
		{"DOCKER_HOST", map[string]string{"DOCKER_HOST": "unix:///x.sock"}, []string{"DOCKER_HOST is set (unix:///x.sock)", "unset DOCKER_HOST", "/cfg.yml"}},
		{"another context", map[string]string{"DOCKER_CONTEXT": "colima"}, []string{"DOCKER_CONTEXT is colima", "pins docker_context colima-buildgate", "unset DOCKER_CONTEXT"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dp := newTestDeps(t)
			fakeHostOf(dp).env = c.env
			err := applyDockerContextPin(dp, "colima-buildgate", "/cfg.yml")
			if c.refused == nil {
				if err != nil || fakeHostOf(dp).env["DOCKER_CONTEXT"] != "colima-buildgate" {
					t.Fatalf("err = %v, env = %v", err, fakeHostOf(dp).env)
				}
				return
			}
			if err == nil {
				t.Fatal("no refusal")
			}
			for _, want := range c.refused {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal lacks %q: %v", want, err)
				}
			}
		})
	}
}

// TestDispatchPinsBeforeTheCommandRuns: dispatch, the one home of the pin,
// refuses before running a command whose environment contradicts the pin.
func TestDispatchPinsBeforeTheCommandRuns(t *testing.T) {
	dp := newTestDeps(t)
	fakeHostOf(dp).env["DOCKER_HOST"] = "tcp://elsewhere:2375"
	path := pinnedConfig(t, "colima-buildgate")
	err := dispatch(dp, []string{"docker-context", "-config", path})
	if err == nil || !strings.Contains(err.Error(), "DOCKER_HOST is set") {
		t.Fatalf("dispatch = %v, want the DOCKER_HOST refusal", err)
	}
}

func TestDockerContextPrintsTheConfigsContext(t *testing.T) {
	dp := newTestDeps(t)
	var out, errOut bytes.Buffer
	if err := dockerContextMain(dp, []string{"-config", pinnedConfig(t, "colima-buildgate")}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "colima-buildgate\n" {
		t.Errorf("stdout = %q", out.String())
	}
	// A fresh process: the call above pinned dp's environment.
	dp = newTestDeps(t)
	out.Reset()
	if err := dockerContextMain(dp, []string{"-config", pinnedConfig(t, "")}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != "\n" {
		t.Errorf("no key: stdout = %q, want an empty line", out.String())
	}
	// No key, and the shell exports a context: that one, so exporting it
	// changes nothing.
	fakeHostOf(dp).env["DOCKER_CONTEXT"] = "colima-other"
	out.Reset()
	if err := dockerContextMain(dp, []string{"-config", pinnedConfig(t, "")}, &out, &errOut); err != nil || out.String() != "colima-other\n" {
		t.Errorf("no key with DOCKER_CONTEXT exported: %v, stdout = %q", err, out.String())
	}
}

// withFakeColima points dp's colima at an executable, so a new config on a
// Mac gets the VM's context.
func withFakeColima(t *testing.T, dp *deps) *hostcontroltest.Colima {
	t.Helper()
	f := hostcontroltest.NewColima(t, "default", false)
	fakeDockerOf(dp).colimaBinaryFn = f.ColimaBinary
	return f
}

func TestNewConfigDockerContext(t *testing.T) {
	dp := newTestDeps(t)
	if got := newConfigDockerContext(dp, "darwin"); got != "" {
		t.Errorf("no colima: %q", got)
	}
	withFakeColima(t, dp)
	if got := newConfigDockerContext(dp, "darwin"); got != hostcontrol.DockerVMContext {
		t.Errorf("a Mac with colima: %q", got)
	}
	if got := newConfigDockerContext(dp, "linux"); got != "" {
		t.Errorf("linux: %q", got)
	}
	if text := initConfigText(dp, "darwin"); !strings.HasSuffix(text, "docker_context: colima-buildgate\n") {
		t.Errorf("init-config on a Mac does not pin the VM:\n%s", text)
	}
}

func TestTopLevelHomeDir(t *testing.T) {
	cases := map[string]string{
		"/Users/op/code/repo":   "/Users/op/code",
		"/Users/op/code":        "/Users/op/code",
		"/Users/op":             "",
		"/tmp/x":                "",
		"/Users/op/.config/x":   "",
		"/Users/operator/thing": "",
	}
	for dir, want := range cases {
		got, ok := topLevelHomeDir("/Users/op", dir)
		if got != want || ok != (want != "") {
			t.Errorf("topLevelHomeDir(%q) = (%q, %v), want %q", dir, got, ok, want)
		}
	}
}

// vmStatAvailable answers vm_stat with gib GiB available, as free pages.
func vmStatAvailable(gib uint64) func() ([]byte, error) {
	return func() ([]byte, error) {
		return []byte("Mach Virtual Memory Statistics: (page size of 1073741824 bytes)\nPages free: " +
			strconv.FormatUint(gib, 10) + ".\nPages inactive: 0.\nPages speculative: 0.\nPages purgeable: 0.\n"), nil
	}
}

func TestDoctorDockerVMRows(t *testing.T) {
	settingsPinned := sessionconfig.DefaultSettings()
	settingsPinned.DockerContext = "colima-buildgate"

	t.Run("not a Mac", func(t *testing.T) {
		if got := doctorDockerVMChecksOn(newTestDeps(t), "linux", settingsPinned, false, nil, &bytes.Buffer{}); got != nil {
			t.Errorf("rows = %+v", got)
		}
	})
	t.Run("missing VM", func(t *testing.T) {
		dp := newTestDeps(t)
		withFakeColima(t, dp).SetList(t, `{"name":"default","status":"Running","memory":4294967296}`)
		fakeHostOf(dp).vmStatFn = vmStatAvailable(20)
		rows := doctorDockerVMChecksOn(dp, "darwin", settingsPinned, false, nil, &bytes.Buffer{})
		if len(rows) != 2 || rows[0].Name != "Docker VM colima-buildgate" || rows[0].Err == nil || !strings.Contains(rows[0].Fix, "`factoryd doctor -fix` creates it") {
			t.Fatalf("rows = %+v", rows)
		}
		if rows[1].Name != "Mac memory available: 20.0 GiB (6.0 GiB kept for macOS)" || rows[1].Err != nil {
			t.Errorf("memory row = %+v", rows[1])
		}
	})
	t.Run("running VM", func(t *testing.T) {
		dp := newTestDeps(t)
		withFakeColima(t, dp).SetList(t, `{"name":"buildgate","status":"Running","memory":12884901888}`)
		fakeHostOf(dp).vmStatFn = vmStatAvailable(3)
		rows := doctorDockerVMChecksOn(dp, "darwin", settingsPinned, false, nil, &bytes.Buffer{})
		if rows[0].Name != "Docker VM colima-buildgate: 12 GiB, running" || rows[0].Err != nil {
			t.Errorf("vm row = %+v", rows[0])
		}
		if !rows[1].Advisory || rows[1].Err == nil {
			t.Errorf("3 GiB available must warn: %+v", rows[1])
		}
	})
	t.Run("stopped VM", func(t *testing.T) {
		dp := newTestDeps(t)
		withFakeColima(t, dp).SetList(t, `{"name":"buildgate","status":"Stopped","memory":4294967296}`)
		fakeHostOf(dp).vmStatFn = func() ([]byte, error) { return nil, errors.New("no vm_stat") }
		rows := doctorDockerVMChecksOn(dp, "darwin", settingsPinned, false, nil, &bytes.Buffer{})
		if rows[0].Err == nil || !strings.Contains(rows[0].Fix, "starts it") {
			t.Errorf("vm row = %+v", rows[0])
		}
	})
	t.Run("a config that pins nothing gets the memory row only", func(t *testing.T) {
		dp := newTestDeps(t)
		fakeHostOf(dp).vmStatFn = vmStatAvailable(20)
		if rows := doctorDockerVMChecksOn(dp, "darwin", sessionconfig.DefaultSettings(), false, nil, &bytes.Buffer{}); len(rows) != 1 {
			t.Errorf("rows = %+v", rows)
		}
	})
}

func TestPinnedDockerUnreachableNamesDoctorFix(t *testing.T) {
	err, fix := pinnedDockerUnreachable("colima-buildgate", errors.New("daemon down"))
	if !strings.Contains(err.Error(), "docker context colima-buildgate, which docker_context pins, does not answer") || fix != "`factoryd doctor -fix` creates or starts the VM colima-buildgate" {
		t.Errorf("err = %v, fix = %q", err, fix)
	}
	checks := explainDockerUnreachable(t.Context(), newTestDeps(t), "docker", "colima-buildgate", []doctorCheck{{Name: doctorDockerReachableName, Err: errors.New("down")}})
	if !strings.Contains(checks[0].Fix, "doctor -fix") {
		t.Errorf("fix = %q", checks[0].Fix)
	}
}
