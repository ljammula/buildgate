// Package openshell launches sandboxed workers through an OpenShell gateway:
// it is the sandbox.Runtime that turns a sandbox.SandboxRequest into the
// gateway's sandbox spec and policy, and reads back what the gateway and
// Docker report.
package openshell

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"
	"github.com/NVIDIA/OpenShell/sdk/go/openshell/v1/types"

	"buildgate/internal/sandbox"
)

const (
	// policyRuleModel names the one network rule and the meter binding of a
	// sandbox's policy.
	policyRuleModel = "model"
	policyMeter     = "meter"
	// landlockRequired makes a kernel without Landlock a launch failure, not
	// an unconfined worker.
	landlockRequired = "hard_requirement"
	// meterOnError: a meter that cannot be reached, or that fails, denies
	// the request.
	meterOnError = "fail_closed"
	meterOrder   = 10
)

// Labels the gateway stores with a sandbox (they are not on the container).
const (
	LabelRun     = "buildgate.run"
	LabelDataDir = "buildgate.data-dir"
)

// sandboxSpec is req as the gateway takes it.
func sandboxSpec(req sandbox.SandboxRequest) (*v1.SandboxSpec, error) {
	if (req.Route == nil) != (req.MeterConfig == nil) {
		return nil, errors.New("openshell: a model route and its meter configuration go together")
	}
	environment := map[string]string{}
	for _, entry := range req.Environment {
		key, value, found := strings.Cut(entry, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("openshell: environment entry %q is not KEY=VALUE", entry)
		}
		environment[key] = value
	}
	mounts := make([]any, 0, len(req.Mounts))
	for _, m := range req.Mounts {
		mounts = append(mounts, driverMount(m))
	}
	spec := &v1.SandboxSpec{
		Environment: environment,
		Command:     append([]string(nil), req.Command...),
		Template: &v1.SandboxTemplate{
			Image:        req.Image,
			DriverConfig: map[string]any{"docker": map[string]any{"mounts": mounts}},
		},
		Policy: &v1.SandboxPolicy{
			Version: 1,
			Filesystem: &v1.FilesystemPolicy{
				ReadOnly:  append([]string(nil), req.ReadOnlyPaths...),
				ReadWrite: append([]string(nil), req.ReadWritePaths...),
			},
			Landlock: &v1.LandlockPolicy{Compatibility: landlockRequired},
		},
	}
	if req.Route != nil {
		addRoute(spec, *req.Route, req.MeterConfig)
	}
	if err := addSidecars(spec, req.Sidecars); err != nil {
		return nil, err
	}
	return spec, nil
}

// policyRuleSidecars names the rule admitting the factory's own containers.
const policyRuleSidecars = "sidecars"

// sidecarBinary admits every worker executable: a sidecar is reached by
// whatever the build runs (go, npm, pip, the project's own tests), and what
// it may reach is one address and its ports, nothing else.
const sidecarBinary = "/**"

// addSidecars lets the worker reach each sidecar's address on its ports and
// nothing else there. The endpoints carry no host, no path rule and no
// credential, and the meter, bound to the model route's host, is not in
// front of them. TLS inspection is skipped: a sidecar speaks its own
// protocol (a database's, a broker's, plain HTTP), not one to terminate.
func addSidecars(spec *v1.SandboxSpec, sidecars []sandbox.SidecarEndpoint) error {
	if len(sidecars) == 0 {
		return nil
	}
	rule := v1.NetworkPolicyRule{Name: policyRuleSidecars, Binaries: []v1.PolicyNetworkBinary{{Path: sidecarBinary}}}
	for _, sidecar := range sidecars {
		if sidecar.IP == "" || len(sidecar.Ports) == 0 {
			return fmt.Errorf("openshell: sidecar %q needs an address and at least one port", sidecar.Name)
		}
		endpoint := v1.PolicyNetworkEndpoint{
			AllowedIPs:  []string{sidecar.IP},
			TLS:         v1.NetworkTLSModeSkip,
			Enforcement: v1.NetworkEnforcementModeEnforce,
		}
		for _, port := range sidecar.Ports {
			if port < 1 || port > 65535 {
				return fmt.Errorf("openshell: sidecar %q port %d is invalid", sidecar.Name, port)
			}
			endpoint.Ports = append(endpoint.Ports, uint32(port))
		}
		rule.Endpoints = append(rule.Endpoints, endpoint)
	}
	if spec.Policy.NetworkPolicies == nil {
		spec.Policy.NetworkPolicies = map[string]v1.NetworkPolicyRule{}
	}
	spec.Policy.NetworkPolicies[policyRuleSidecars] = rule
	return nil
}

// workloadTemplate carries what a sandbox spec cannot: the CPU and memory
// limits, which the gateway takes only as a workload template's typed
// resources. It is created for one launch, under the sandbox's name, with
// the image, environment and mounts moved into it.
func workloadTemplate(req sandbox.SandboxRequest, spec *v1.SandboxSpec) (*v1.SandboxWorkloadTemplate, error) {
	memory, err := memoryQuantity(req.Memory)
	if err != nil {
		return nil, err
	}
	template := &v1.SandboxWorkloadTemplate{
		Name: req.Name,
		Spec: v1.SandboxWorkloadTemplateSpec{
			Workload: &v1.SandboxWorkloadConfig{
				Image:       spec.Template.Image,
				Environment: spec.Environment,
				Resources:   &v1.SandboxResources{CPU: req.CPUs, Memory: memory},
			},
			DriverConfig: spec.Template.DriverConfig,
		},
	}
	// A sandbox created from a template may set only its policy, providers,
	// command and tty.
	spec.Template, spec.Environment = nil, nil
	return template, nil
}

// memoryQuantity turns Docker's memory syntax ("512m", "4g"), which the
// session config uses, into the quantity the gateway takes: a plain number
// of bytes.
func memoryQuantity(value string) (string, error) {
	n, err := sandbox.ParseByteSize(value)
	if err != nil || n < 1 {
		return "", fmt.Errorf("openshell: memory limit %q is not a positive size with an optional b/k/m/g suffix", value)
	}
	return strconv.FormatInt(n, 10), nil
}

func driverMount(m sandbox.SandboxMount) map[string]any {
	if m.Tmpfs {
		return map[string]any{
			"type":       "tmpfs",
			"target":     m.Target,
			"size_bytes": m.SizeBytes,
			"mode":       int64(m.Mode),
			"options":    stringsAsAny(m.Options),
		}
	}
	return map[string]any{"type": "bind", "source": m.Source, "target": m.Target, "read_only": m.ReadOnly}
}

func stringsAsAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// addRoute opens the route's one endpoint to the harness's executables,
// binds the route's credential to it and puts the meter in front of it.
func addRoute(spec *v1.SandboxSpec, route sandbox.RouteAccess, meterConfig map[string]any) {
	endpoint := v1.PolicyNetworkEndpoint{
		Host:        route.Endpoint.Host,
		Port:        uint32(route.Endpoint.Port),
		Protocol:    "rest",
		Enforcement: v1.NetworkEnforcementModeEnforce,
		Rules:       []v1.L7Rule{{Allow: &v1.L7Allow{Method: route.Endpoint.Method, Path: route.Endpoint.Path}}},
	}
	if route.Endpoint.PathIsPrefix {
		endpoint.Rules = append(endpoint.Rules, v1.L7Rule{Allow: &v1.L7Allow{
			Method: route.Endpoint.Method, Path: strings.TrimRight(route.Endpoint.Path, "/") + "/**",
		}})
	}
	if route.Provider != "" {
		endpoint.CredentialBinding = &types.NetworkCredentialBinding{Provider: route.Provider}
		spec.Providers = []string{route.Provider}
	}
	rule := v1.NetworkPolicyRule{Name: policyRuleModel, Endpoints: []v1.PolicyNetworkEndpoint{endpoint}}
	for _, path := range route.Endpoint.Binaries {
		rule.Binaries = append(rule.Binaries, v1.PolicyNetworkBinary{Path: path})
	}
	spec.Policy.NetworkPolicies = map[string]v1.NetworkPolicyRule{policyRuleModel: rule}
	spec.Policy.NetworkMiddlewares = map[string]types.NetworkMiddlewareConfig{policyMeter: {
		Name:       policyMeter,
		Middleware: sandbox.MeterMiddleware,
		Config:     meterConfig,
		OnError:    meterOnError,
		Endpoints:  &types.MiddlewareEndpointSelector{Include: []string{route.Endpoint.Host}},
		Order:      meterOrder,
	}}
}

// credentialProfile is the provider profile for a credential with these
// environment names: it declares the names and nothing else. The endpoint,
// path rule and executables are in each sandbox's own policy.
func credentialProfile(names []string) v1.ProviderProfile {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	profile := v1.ProviderProfile{
		ID:          "buildgate-" + strings.ToLower(strings.ReplaceAll(strings.Join(sorted, "-"), "_", "-")),
		DisplayName: "Buildgate model route credential",
		Description: "Placeholders for " + strings.Join(sorted, ", "),
		Category:    v1.ProfileCategoryInference,
	}
	for _, name := range sorted {
		profile.Credentials = append(profile.Credentials, v1.ProfileCredential{
			Name: name, EnvVars: []string{name}, Required: true, Secret: true,
		})
	}
	return profile
}
