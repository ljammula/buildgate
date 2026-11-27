package hostcontrol

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestParseVMStatReadsACapturedOutput: a vm_stat output captured on a Mac
// Studio (16 KiB pages) reads as free + inactive + speculative + purgeable.
func TestParseVMStatReadsACapturedOutput(t *testing.T) {
	out, err := os.ReadFile(filepath.Join("testdata", "vm_stat.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseVMStat(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := uint64((369227 + 2105980 + 52112 + 10256) * 16384); got != want {
		t.Errorf("ParseVMStat = %d, want %d", got, want)
	}
}

func TestParseVMStatRefusesAPartialOutput(t *testing.T) {
	for name, out := range map[string]string{
		"no page size": "Pages free: 1.\nPages inactive: 1.\nPages speculative: 1.\nPages purgeable: 1.\n",
		"missing line": "Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: 1.\n",
		"empty":        "",
	} {
		if _, err := ParseVMStat([]byte(out)); err == nil {
			t.Errorf("%s: ParseVMStat accepted %q", name, out)
		}
	}
}

func TestParseSysctlResources(t *testing.T) {
	memory, cpus, err := parseSysctlResources([]byte("103079215104\n28\n"))
	if err != nil || memory != 96<<30 || cpus != 28 {
		t.Errorf("got (%d, %d, %v)", memory, cpus, err)
	}
	if _, _, err := parseSysctlResources([]byte("123\n")); err == nil {
		t.Error("one line was accepted")
	}
}

// TestSizeVM: an eighth of the memory from 4 to 12 GiB, a quarter of the
// CPUs from 2 to 8.
func TestSizeVM(t *testing.T) {
	cases := []struct {
		memoryGiB uint64
		cpus      int
		want      VMSize
	}{
		{96, 28, VMSize{12, 7, 100}},
		{16, 8, VMSize{4, 2, 100}},
		{8, 4, VMSize{4, 2, 100}},
		{64, 12, VMSize{8, 3, 100}},
		{36, 14, VMSize{4, 3, 100}},
		{192, 64, VMSize{12, 8, 100}},
	}
	for _, c := range cases {
		if got := SizeVM(c.memoryGiB<<30, c.cpus); got != c.want {
			t.Errorf("SizeVM(%d GiB, %d) = %+v, want %+v", c.memoryGiB, c.cpus, got, c.want)
		}
	}
}

func TestParseColimaListReadsEachProfile(t *testing.T) {
	out := `{"name":"default","status":"Running","arch":"aarch64","cpus":8,"memory":17179869184,"disk":429496729600,"runtime":"docker"}
{"name":"other","status":"Stopped","arch":"aarch64","cpus":8,"memory":17179869184,"disk":107374182400,"runtime":"docker"}
not json
`
	vms := ParseColimaList([]byte(out))
	if len(vms) != 2 || !vms[0].Running() || vms[1].Running() || vms[1].Name != "other" || vms[0].Memory != 16<<30 {
		t.Errorf("ParseColimaList = %+v", vms)
	}
}

func TestVMMountArgsLeaveOutOverlappingDirectories(t *testing.T) {
	got := strings.Join(vmMountArgs(VMMounts{
		Writable: "/Users/op/buildgate",
		ReadOnly: []string{"/Users/op/code", "/Users/op", "/Users/op/buildgate/x", "/Users/op/code/repo", "/Users/op/src"},
	}), " ")
	want := "--mount /Users/op/buildgate:w --mount /Users/op/code --mount /Users/op/src"
	if got != want {
		t.Errorf("mounts = %q, want %q", got, want)
	}
}

// vmStatWith answers vm_stat with available bytes as free pages.
func vmStatWith(available uint64) func() ([]byte, error) {
	return func() ([]byte, error) {
		pages := available / 4096
		return []byte("Mach Virtual Memory Statistics: (page size of 4096 bytes)\nPages free: " +
			strconv.FormatUint(pages, 10) + ".\nPages inactive: 0.\nPages speculative: 0.\nPages purgeable: 0.\n"), nil
	}
}

func TestEnsureDockerVMCreatesAMissingProfileSizedForTheMac(t *testing.T) {
	dp := newFakeDeps(t)
	f := newColimaFake(dp, t, "default", false)
	f.SetList(t, `{"name":"default","status":"Running","cpus":8,"memory":17179869184}`)
	dp.hostResourcesFn = func() (uint64, int, error) { return 96 << 30, 28, nil }
	dp.vmStatFn = vmStatWith(40 << 30)
	data := filepath.Join(t.TempDir(), "buildgate")
	var out bytes.Buffer
	state, err := EnsureDockerVM(dp, context.Background(), &out, "buildgate", VMMounts{Writable: data, ReadOnly: []string{"/Users/op/code"}})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Created || state.Memory != 12<<30 || state.String() != "Docker VM colima-buildgate: 12 GiB, created and running" {
		t.Errorf("state = %+v (%s)", state, state)
	}
	want := "start --profile buildgate --activate=false --vm-type vz --mount-type virtiofs --cpus 7 --memory 12 --disk 100 --mount " + data + ":w --mount /Users/op/code"
	if got := strings.TrimSpace(f.ColimaCalls()); !strings.HasSuffix(got, want) || strings.Contains(got, "default") {
		t.Errorf("colima calls = %q, want only list and %q", got, want)
	}
	if _, err := os.Stat(data); err != nil {
		t.Errorf("the writable mount was not created: %v", err)
	}
}

func TestEnsureDockerVMRefusesWithoutHeadroomAndCreatesNothing(t *testing.T) {
	dp := newFakeDeps(t)
	f := newColimaFake(dp, t, "default", false)
	dp.hostResourcesFn = func() (uint64, int, error) { return 16 << 30, 8, nil }
	dp.vmStatFn = vmStatWith(7 << 30)
	_, err := EnsureDockerVM(dp, context.Background(), &bytes.Buffer{}, "buildgate", VMMounts{})
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{"7 GiB of memory available", "needs 10 GiB", "4 GiB for the VM plus 6 GiB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q: %v", want, err)
		}
	}
	if strings.Contains(f.ColimaCalls(), "start") {
		t.Errorf("colima started without headroom: %q", f.ColimaCalls())
	}
}

func TestEnsureDockerVMStartsAStoppedProfileWithItsOwnSize(t *testing.T) {
	dp := newFakeDeps(t)
	f := newColimaFake(dp, t, "default", false)
	f.SetList(t, `{"name":"buildgate","status":"Stopped","cpus":2,"memory":8589934592}`)
	dp.vmStatFn = vmStatWith(20 << 30)
	state, err := EnsureDockerVM(dp, context.Background(), &bytes.Buffer{}, "buildgate", VMMounts{Writable: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !state.Started || state.Memory != 8<<30 {
		t.Errorf("state = %+v", state)
	}
	lines := strings.Split(strings.TrimSpace(f.ColimaCalls()), "\n")
	if got := lines[len(lines)-1]; got != "start --profile buildgate --activate=false" {
		t.Errorf("last colima call = %q", got)
	}
}

func TestEnsureDockerVMLeavesARunningProfileAlone(t *testing.T) {
	dp := newFakeDeps(t)
	f := newColimaFake(dp, t, "default", false)
	f.SetList(t, `{"name":"buildgate","status":"Running","cpus":2,"memory":4294967296}`)
	state, err := EnsureDockerVM(dp, context.Background(), &bytes.Buffer{}, "buildgate", VMMounts{})
	if err != nil {
		t.Fatal(err)
	}
	if state.Started || state.Created || state.String() != "Docker VM colima-buildgate: 4 GiB, running" {
		t.Errorf("state = %+v", state)
	}
	if strings.Contains(f.ColimaCalls(), "start") {
		t.Errorf("colima calls = %q", f.ColimaCalls())
	}
}

// TestAutostartFollowsThePinnedContext: with DOCKER_CONTEXT pinned, the
// profile autostart starts is the pinned one, whatever `docker context
// show` would print, and it starts without making its context current.
func TestAutostartFollowsThePinnedContext(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	t.Setenv("DOCKER_CONTEXT", "colima-buildgate")
	f := newColimaFake(dp, t, "colima", true)
	f.SetList(t, `{"name":"default","status":"Running","memory":4294967296}`, `{"name":"buildgate","status":"Stopped","memory":4294967296}`)
	dp.vmStatFn = vmStatWith(20 << 30)
	if p, ok := ColimaProfile(dp, context.Background()); !ok || p != "buildgate" {
		t.Fatalf("ColimaProfile = (%q, %v), want buildgate", p, ok)
	}
	if err := ensureDocker(dp, context.Background(), &bytes.Buffer{}, "not reachable"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(f.ColimaCalls()), "\n")
	if got := lines[len(lines)-1]; got != "start --profile buildgate --activate=false" {
		t.Errorf("colima calls = %q", f.ColimaCalls())
	}
}

// TestAutostartNamesDoctorFixForAMissingPinnedVM: a pinned VM that does not
// exist is never created by autostart: the reason names doctor -fix.
func TestAutostartNamesDoctorFixForAMissingPinnedVM(t *testing.T) {
	dp := newFakeDeps(t)
	t.Setenv(AutostartEnvVar, "1")
	t.Setenv("DOCKER_CONTEXT", "colima-buildgate")
	f := newColimaFake(dp, t, "colima", true)
	f.SetList(t, `{"name":"default","status":"Running","memory":4294967296}`)
	err := ensureDocker(dp, context.Background(), &bytes.Buffer{}, "not reachable at localhost:7233")
	if err == nil || !strings.Contains(err.Error(), "the Docker VM colima-buildgate does not exist: `factoryd doctor -fix` creates it") {
		t.Fatalf("ensureDocker = %v", err)
	}
	var missing MissingDockerVMError
	if !errors.As(MissingDockerVMError{Profile: "x"}, &missing) {
		t.Error("MissingDockerVMError is not matched by errors.As")
	}
	if strings.Contains(f.ColimaCalls(), "start") {
		t.Errorf("colima calls = %q", f.ColimaCalls())
	}
}
