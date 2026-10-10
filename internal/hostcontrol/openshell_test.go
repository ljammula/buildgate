package hostcontrol

import (
	"bytes"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"buildgate"
)

// The pinned images, repeated here so a change to a constant, the compose
// file, the gateway template or the Makefile that misses the others fails.
const (
	wantGatewayImage    = "ghcr.io/nvidia/openshell/gateway@sha256:2fe4dad9118e14ab80a8258b545ea6e6cd74c3469e24ad4e6610f964d98913a2"
	wantSandboxImage    = "ghcr.io/nvidia/openshell/sandbox@sha256:bf4797b6c511f2d8ba02955dbba4bf76c1f0dd6d83531420c5408d5f1fb9d72f"
	wantSupervisorImage = "ghcr.io/nvidia/openshell/supervisor@sha256:d7b5264bb6bc56f4796e6fa3617b8e4a8d785be0b7293542efd8cc250b0fb67a"
)

func TestOpenShellComposeIsLoopbackOnlyAndNeverRestarts(t *testing.T) {
	var doc struct {
		Name     string `yaml:"name"`
		Services map[string]struct {
			Image       string   `yaml:"image"`
			Restart     string   `yaml:"restart"`
			Ports       []string `yaml:"ports"`
			NetworkMode string   `yaml:"network_mode"`
			Command     []string `yaml:"command"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(buildgate.OpenShellCompose, &doc); err != nil {
		t.Fatalf("embedded compose does not parse: %v", err)
	}
	if doc.Name != "buildgate-openshell" {
		t.Errorf("project name = %q", doc.Name)
	}
	if len(doc.Services) != 2 {
		t.Fatalf("services = %d, want meter and gateway", len(doc.Services))
	}
	for name, svc := range doc.Services {
		if svc.Restart != "no" {
			t.Errorf("%s restart = %q, want \"no\"", name, svc.Restart)
		}
		for _, p := range svc.Ports {
			if !strings.HasPrefix(p, "127.0.0.1:") {
				t.Errorf("%s publishes %q on an address other than 127.0.0.1", name, p)
			}
		}
	}
	// The meter publishes on the VM's loopback. The gateway shares the VM's
	// network (it and the host-networked supervisors reach the meter there),
	// so it publishes nothing and must be told to bind loopback itself.
	if meter := doc.Services["meter"]; len(meter.Ports) != 1 || meter.NetworkMode != "" {
		t.Errorf("meter ports = %q, network_mode = %q; want one loopback port on its own network", meter.Ports, meter.NetworkMode)
	}
	gateway := doc.Services["gateway"]
	if gateway.NetworkMode != "host" || len(gateway.Ports) != 0 {
		t.Errorf("gateway network_mode = %q, ports = %q; want host and none", gateway.NetworkMode, gateway.Ports)
	}
	// The compose file repeats the ports of hostcontrol's addresses.
	_, gatewayPort, _ := net.SplitHostPort(OpenShellGatewayAddr)
	if want := []string{OpenShellMeterAddr + ":50051"}; !reflect.DeepEqual(doc.Services["meter"].Ports, want) {
		t.Errorf("meter ports = %q, want %q", doc.Services["meter"].Ports, want)
	}
	if want := []string{"--bind-address", "127.0.0.1", "--port", gatewayPort}; !reflect.DeepEqual(gateway.Command, want) {
		t.Errorf("gateway command = %q, want %q", gateway.Command, want)
	}
	if got := doc.Services["gateway"].Image; got != wantGatewayImage {
		t.Errorf("gateway image = %q, want %q", got, wantGatewayImage)
	}
}

func TestOpenShellPinnedImagesMatchTheEmbeddedFiles(t *testing.T) {
	for got, want := range map[string]string{
		OpenShellGatewayImage:    wantGatewayImage,
		OpenShellSandboxImage:    wantSandboxImage,
		OpenShellSupervisorImage: wantSupervisorImage,
	} {
		if got != want {
			t.Errorf("constant %q, want %q", got, want)
		}
	}
	if !bytes.Contains(buildgate.OpenShellCompose, []byte(OpenShellGatewayImage)) {
		t.Error("the compose file does not pin OpenShellGatewayImage")
	}
	if !strings.Contains(buildgate.OpenShellGatewayTemplate, OpenShellSandboxImage) {
		t.Errorf("the gateway template does not pin %s", OpenShellSandboxImage)
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{wantGatewayImage, wantSandboxImage, wantSupervisorImage} {
		if !bytes.Contains(makefile, []byte("docker pull "+image)) {
			t.Errorf("make openshell-images does not pull %s", image)
		}
	}
}
