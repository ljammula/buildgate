package openshell

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"

	"buildgate/internal/sandbox"
)

// fakeVolumes records the volume calls, in order, in a log it shares with
// faultyClient so a test can read the order of volume and gateway calls.
type fakeVolumes struct {
	log       *[]string
	createErr error
	removeErr error
	labels    map[string]string
}

func (f *fakeVolumes) Create(_ context.Context, name string, labels map[string]string) error {
	*f.log = append(*f.log, "volume.create "+name)
	f.labels = labels
	return f.createErr
}

func (f *fakeVolumes) Remove(_ context.Context, name string) error {
	*f.log = append(*f.log, "volume.remove "+name)
	return f.removeErr
}

// faultyClient fails the gateway call each field names and logs the calls.
type faultyClient struct {
	clientWithProfiles
	log         *[]string
	templateErr error
	sandboxErr  error
	readyErr    error
	gotLabels   map[string]string
}

type faultyTemplates struct {
	v1.SandboxTemplateInterface
	c *faultyClient
}

func (t faultyTemplates) Create(ctx context.Context, workspace string, template *v1.SandboxWorkloadTemplate) (*v1.SandboxWorkloadTemplate, error) {
	*t.c.log = append(*t.c.log, "template.create")
	if t.c.templateErr != nil {
		return nil, t.c.templateErr
	}
	return t.SandboxTemplateInterface.Create(ctx, workspace, template)
}

type faultySandboxes struct {
	v1.SandboxInterface
	c *faultyClient
}

func (s faultySandboxes) WaitReady(ctx context.Context, workspace, name string, opts ...v1.WaitOptions) (*v1.Sandbox, error) {
	*s.c.log = append(*s.c.log, "sandbox.wait")
	if s.c.readyErr != nil {
		return nil, s.c.readyErr
	}
	return s.SandboxInterface.WaitReady(ctx, workspace, name, opts...)
}

func (c *faultyClient) SandboxTemplates() v1.SandboxTemplateInterface {
	return faultyTemplates{SandboxTemplateInterface: c.clientWithProfiles.SandboxTemplates(), c: c}
}

func (c *faultyClient) Sandboxes() v1.SandboxInterface {
	return faultySandboxes{SandboxInterface: c.clientWithProfiles.Sandboxes(), c: c}
}

func (c *faultyClient) CreateSandboxFromTemplate(ctx context.Context, workspace, name, templateName string, spec *v1.SandboxSpec, labels map[string]string, opts ...v1.CreateOptions) (*v1.Sandbox, error) {
	*c.log = append(*c.log, "sandbox.create")
	c.gotLabels = labels
	if c.sandboxErr != nil {
		return nil, c.sandboxErr
	}
	return c.clientWithProfiles.CreateSandboxFromTemplate(ctx, workspace, name, templateName, spec, labels, opts...)
}

func scratchRuntime() (*Runtime, *faultyClient, *fakeVolumes, *fakeContainers, *[]string) {
	rt, client, containers := newTestRuntime()
	log := &[]string{}
	faulty := &faultyClient{clientWithProfiles: client, log: log}
	volumes := &fakeVolumes{log: log}
	rt.Client, rt.Volumes = faulty, volumes
	return rt, faulty, volumes, containers, log
}

func scratchRequest() sandbox.SandboxRequest {
	req := testRequest()
	req.Scratch = true
	return req
}

const scratchVolume = "bg-scratch-bg-0123456789abcdef"

func TestCreateWithScratchMakesTheVolumeBeforeTheTemplate(t *testing.T) {
	rt, client, volumes, _, log := scratchRuntime()
	if _, err := rt.Create(context.Background(), scratchRequest()); err != nil {
		t.Fatal(err)
	}
	want := []string{"volume.create " + scratchVolume, "template.create", "sandbox.create", "sandbox.wait"}
	if !reflect.DeepEqual(*log, want) {
		t.Errorf("calls = %q, want %q", *log, want)
	}
	wantLabels := map[string]string{"buildgate.run": "run1", "buildgate.data-dir": "bd47413b5c03dfc6e4b5b8143fe205eae10ecaf0c28b426be98d74cff48c745"}
	if !reflect.DeepEqual(volumes.labels, wantLabels) || !reflect.DeepEqual(client.gotLabels, wantLabels) {
		t.Errorf("volume labels = %v, sandbox labels = %v, want both %v", volumes.labels, client.gotLabels, wantLabels)
	}
}

func TestSandboxSpecMountsTheScratchVolume(t *testing.T) {
	spec, err := sandboxSpec(scratchRequest())
	if err != nil {
		t.Fatal(err)
	}
	mounts := spec.Template.DriverConfig["docker"].(map[string]any)["mounts"].([]any)
	if len(mounts) != 4 {
		t.Fatalf("mounts = %v, want the request's three and the volume", mounts)
	}
	want := map[string]any{"type": "volume", "source": "bg-scratch-bg-0123456789abcdef", "target": "/scratch", "read_only": false}
	if !reflect.DeepEqual(mounts[3], want) {
		t.Errorf("volume mount = %v, want %v", mounts[3], want)
	}
}

func TestCreateWithoutScratchTouchesNoVolume(t *testing.T) {
	rt, _, _, _, log := scratchRuntime()
	if _, err := rt.Create(context.Background(), testRequest()); err != nil {
		t.Fatal(err)
	}
	for _, call := range *log {
		if strings.HasPrefix(call, "volume.") {
			t.Errorf("a volume call without Scratch: %q", *log)
		}
	}
	spec, err := sandboxSpec(testRequest())
	if err != nil {
		t.Fatal(err)
	}
	if mounts := spec.Template.DriverConfig["docker"].(map[string]any)["mounts"].([]any); len(mounts) != 3 {
		t.Errorf("mounts = %v, want the request's three", mounts)
	}
}

func TestCreateWithScratchFailsWhenTheVolumeCannotBeMade(t *testing.T) {
	rt, _, volumes, _, log := scratchRuntime()
	volumes.createErr = errors.New("exists")
	if _, err := rt.Create(context.Background(), scratchRequest()); err == nil {
		t.Fatal("Create succeeded without its volume")
	}
	if want := []string{"volume.create " + scratchVolume}; !reflect.DeepEqual(*log, want) {
		t.Errorf("calls = %q, want %q", *log, want)
	}
}

func TestCreateRemovesTheVolumeWhenTheGatewayRefusesTheSandbox(t *testing.T) {
	for name, set := range map[string]func(*faultyClient){
		"template": func(c *faultyClient) { c.templateErr = errors.New("template refused") },
		"sandbox":  func(c *faultyClient) { c.sandboxErr = errors.New("sandbox refused") },
	} {
		rt, client, volumes, _, log := scratchRuntime()
		set(client)
		if _, err := rt.Create(context.Background(), scratchRequest()); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("%s: err = %v", name, err)
		}
		if last := (*log)[len(*log)-1]; last != "volume.remove "+scratchVolume {
			t.Errorf("%s: calls = %q, want the volume removed last", name, *log)
		}
		rt, client, volumes, _, _ = scratchRuntime()
		set(client)
		volumes.removeErr = errors.New("busy")
		if _, err := rt.Create(context.Background(), scratchRequest()); err == nil || !strings.Contains(err.Error(), "busy") || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: err = %v, want both the launch failure and the removal failure", name, err)
		}
	}
}

func TestCreateKeepsTheVolumeOnceTheSandboxExists(t *testing.T) {
	rt, client, _, _, log := scratchRuntime()
	client.readyErr = errors.New("never ready")
	if _, err := rt.Create(context.Background(), scratchRequest()); err == nil {
		t.Fatal("Create succeeded")
	}
	for _, call := range *log {
		if strings.HasPrefix(call, "volume.remove") {
			t.Errorf("Create removed the volume after the sandbox existed: %q", *log)
		}
	}
}

func TestCreateWithScratchAndNoVolumesFails(t *testing.T) {
	rt, _, _, _, log := scratchRuntime()
	rt.Volumes = nil
	if _, err := rt.Create(context.Background(), scratchRequest()); err == nil {
		t.Fatal("Create succeeded with no Volumes")
	}
	if len(*log) != 0 {
		t.Errorf("calls = %q, want none", *log)
	}
}

func TestSandboxSpecRefusesAMountOnTheScratchTarget(t *testing.T) {
	req := scratchRequest()
	req.Mounts = append(req.Mounts, sandbox.SandboxMount{Source: "/host/scratch", Target: "/scratch"})
	if _, err := sandboxSpec(req); err == nil {
		t.Error("two mounts on /scratch were accepted")
	}
	if _, err := sandboxSpec(testRequest()); err != nil {
		t.Error(err)
	}
	req.Scratch = false
	if _, err := sandboxSpec(req); err != nil {
		t.Errorf("a bind on /scratch without Scratch: %v", err)
	}
}

func TestDeleteRemovesTheVolumeOnceTheContainersAreGone(t *testing.T) {
	rt, _, volumes, containers, log := scratchRuntime()
	containers.startedAt["bg-a"] = "t"
	if err := rt.Delete(context.Background(), "bg-a"); err != nil {
		t.Fatal(err)
	}
	if last := (*log)[len(*log)-1]; last != "volume.remove bg-scratch-bg-a" {
		t.Errorf("calls = %q, want the volume removed", *log)
	}
	volumes.removeErr = errors.New("in use")
	var warned []string
	rt.Warn = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	if err := rt.Delete(context.Background(), "bg-a"); err != nil {
		t.Errorf("a volume that will not go: err = %v, want nil", err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "bg-scratch-bg-a") || !strings.Contains(warned[0], "in use") {
		t.Errorf("warnings = %q, want one naming the volume and the error", warned)
	}
	rt.Warn = nil
	if err := rt.Delete(context.Background(), "bg-a"); err != nil {
		t.Errorf("a volume that will not go, nil Warn: err = %v, want nil", err)
	}
}

func TestDeleteKeepsTheVolumeWhileAContainerMayRemain(t *testing.T) {
	rt, _, _, containers, log := scratchRuntime()
	containers.startedAt["bg-a"] = "t"
	containers.sticky = true
	_ = rt.Delete(context.Background(), "bg-a")
	containers.sticky, containers.listErr = false, errors.New("docker unreachable")
	_ = rt.Delete(context.Background(), "bg-a")
	for _, call := range *log {
		if strings.HasPrefix(call, "volume.remove") {
			t.Errorf("Delete removed the volume while a container may remain: %q", *log)
		}
	}
}
