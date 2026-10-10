package openshell

import (
	"context"
	"reflect"
	"testing"

	"buildgate/internal/sandbox"
)

// TestSandboxSpecLeavesTheWorkerUserToTheImage: what the gateway is sent for
// a launch names no user, group, user-namespace or runtime-class setting and
// no driver option but the mounts. The worker's user is the image's, whose
// recipe
// TestWorkerDockerfilesEndAsTheNonRootWorkerUser pins.
func TestSandboxSpecLeavesTheWorkerUserToTheImage(t *testing.T) {
	withSidecars := routedRequest()
	withSidecars.Sidecars = []sandbox.SidecarEndpoint{{Name: "db", IP: "172.29.0.5", Ports: []int{5432}}}
	requests := map[string]sandbox.SandboxRequest{
		"no route":              testRequest(),
		"a model route":         routedRequest(),
		"a route and a sidecar": withSidecars,
	}
	for name, req := range requests {
		t.Run(name, func(t *testing.T) {
			spec, err := sandboxSpec(req)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Policy == nil || spec.Policy.Process != nil {
				t.Errorf("policy = %+v, want one with no process identity", spec.Policy)
			}
			if spec.Template.UserNamespaces != nil || spec.Template.RuntimeClassName != "" || spec.Template.Resources != nil {
				t.Errorf("template = %+v, want no user-namespace, runtime-class or untyped resource setting", spec.Template)
			}
			template, err := workloadTemplate(req, spec)
			if err != nil {
				t.Fatal(err)
			}
			driver := template.Spec.DriverConfig
			docker, _ := driver["docker"].(map[string]any)
			if len(driver) != 1 || len(docker) != 1 || docker["mounts"] == nil {
				t.Errorf("driver config = %v, want the Docker mounts and nothing else", driver)
			}
			if spec.Policy.Process != nil || spec.Template != nil {
				t.Errorf("spec after the template took its part = %+v, policy %+v", spec, spec.Policy)
			}
		})
	}
}

// TestCreateStoresASandboxWithNoProcessIdentity is the same on the sandbox
// the gateway client receives from Create.
func TestCreateStoresASandboxWithNoProcessIdentity(t *testing.T) {
	rt, client, _ := newTestRuntime()
	ctx := context.Background()
	ref, err := rt.Create(ctx, testRequest())
	if err != nil {
		t.Fatal(err)
	}
	stored, err := client.Sandboxes().Get(ctx, DefaultWorkspace, ref.Name)
	if err != nil {
		t.Fatal(err)
	}
	policy := stored.Spec.Policy
	if policy == nil || policy.Landlock == nil || policy.Landlock.Compatibility != landlockRequired {
		t.Fatalf("stored policy = %+v, want the launch's own (Landlock required)", policy)
	}
	if policy.Process != nil {
		t.Errorf("stored process identity = %+v, want none", policy.Process)
	}
	if !reflect.DeepEqual(stored.Spec.Command, testRequest().Command) {
		t.Errorf("stored command = %q, want the request's", stored.Spec.Command)
	}
}
