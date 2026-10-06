package sandbox

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"buildgate/internal/composeservices"
)

// A worker launched through a sandbox runtime joins no Docker network and
// resolves no alias, so it is given each compose service by address. The
// services are started afresh for every attempt, so the addresses are read
// after each attempt's start (resolveAddresses) and applied to that
// attempt's launch (applyByAddress).

// runtimeBlockedPorts are ports the sandbox runtime never admits on an
// address-only endpoint (the control planes of etcd and Kubernetes).
var runtimeBlockedPorts = map[int]bool{2379: true, 2380: true, 6443: true, 10250: true, 10255: true}

// servicePorts lists every port of s the worker may be pointed at: the one
// its BG_SERVICE_<NAME>_PORT names, its declared container ports and the
// targets of its published ports.
func servicePorts(s composeservices.ServiceSpec) []int {
	seen := map[int]bool{}
	add := func(port int) {
		if port > 0 {
			seen[port] = true
		}
	}
	add(aliasReachablePort(s))
	for _, declared := range s.Ports {
		if port, err := strconv.Atoi(declared); err == nil {
			add(port)
		}
	}
	for _, published := range s.PublishedPorts {
		add(published.Target)
	}
	ports := make([]int, 0, len(seen))
	for port := range seen {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// runtimeBlockedService names a launched service and port the sandbox
// runtime would refuse, "" and 0 when there is none.
func (l *ComposeServicesLifecycle) runtimeBlockedService() (string, int) {
	for _, s := range l.services {
		for _, port := range servicePorts(s) {
			if runtimeBlockedPorts[port] {
				return s.Name, port
			}
		}
	}
	return "", 0
}

// resolveAddresses records the address of every launched service of project
// on the run's compose network. A service with no address is an error: the
// worker would be started unable to reach something its build depends on.
func (l *ComposeServicesLifecycle) resolveAddresses(ctx context.Context, project string) error {
	addresses := l.hooks.ServiceAddresses
	if addresses == nil {
		addresses = composeServiceAddresses
	}
	found, err := addresses(ctx, l.dockerBinary, project, l.NetworkName())
	if err != nil {
		return fmt.Errorf("resolve compose service addresses: %w", err)
	}
	resolved := make(map[string]string, len(l.services))
	for _, s := range l.services {
		ip := found[s.Name]
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("compose service %q has no address on network %q (got %q)", s.Name, l.NetworkName(), ip)
		}
		resolved[s.Name] = ip
	}
	l.addresses = resolved
	return nil
}

// composeServiceAddresses asks Docker for the address of each container of a
// compose project on one network, keyed by the service it belongs to.
func composeServiceAddresses(ctx context.Context, dockerBinary, project, network string) (map[string]string, error) {
	ids, err := dockerLines(ctx, dockerBinary, "ps", "-q", "--filter", "label=com.docker.compose.project="+project)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	format := `{{index .Config.Labels "com.docker.compose.service"}}` + "\t" + `{{with index .NetworkSettings.Networks "` + network + `"}}{{.IPAddress}}{{end}}`
	lines, err := dockerLines(ctx, dockerBinary, append([]string{"inspect", "--type", "container", "--format", format}, ids...)...)
	if err != nil {
		return nil, err
	}
	addresses := map[string]string{}
	for _, line := range lines {
		if service, ip, found := strings.Cut(line, "\t"); found && service != "" {
			addresses[service] = ip
		}
	}
	return addresses, nil
}

// applyByAddress is ApplyToWorkerLaunch for a worker that joins no network:
// BG_SERVICE_<NAME> and the forward targets carry this attempt's addresses,
// and each service becomes a sidecar the runtime's policy admits.
func (l *ComposeServicesLifecycle) applyByAddress(s LaunchSpec) LaunchSpec {
	s.Environment = append(s.Environment, "BG_COMPOSE_SERVICES=up")
	for _, svc := range l.services {
		ip := l.addresses[svc.Name]
		s.Environment = append(s.Environment, ComposeServicesEnvVarName(svc.Name)+"="+ip)
		if port := aliasReachablePort(svc); port != 0 {
			s.Environment = append(s.Environment, ComposeServicesEnvVarName(svc.Name)+"_PORT="+strconv.Itoa(port))
		}
		if ports := servicePorts(svc); len(ports) > 0 {
			s.Sidecars = append(s.Sidecars, SidecarEndpoint{Name: svc.Name, IP: ip, Ports: ports})
		}
	}
	s.UnrecordedEnvironment = append(s.UnrecordedEnvironment, l.workerEnvironment...)
	if len(l.forwards) > 0 {
		s.Environment = append(s.Environment, ComposeForwardsEnvVar+"="+strings.Join(l.forwardsByAddress(), ","))
		s.Command = append([]string{WorkerForwarderPath, "--"}, s.Command...)
	}
	return s
}

// forwardsByAddress rewrites each "<host port>=<service>:<port>" forward to
// name the service's address.
func (l *ComposeServicesLifecycle) forwardsByAddress() []string {
	out := make([]string, len(l.forwards))
	for i, forward := range l.forwards {
		hostPort, target, _ := strings.Cut(forward, "=")
		service, port, _ := strings.Cut(target, ":")
		out[i] = hostPort + "=" + net.JoinHostPort(l.addresses[service], port)
	}
	return out
}

// workerEnvNamingAnAlias returns the key of an operator worker-environment
// entry whose value names a launched service as a host, and that service;
// "" when there is none. Such a value ("postgres://app@db:5432/app") works
// for a worker on the compose network and cannot for one that is given
// addresses: it has no name service for aliases. Only the key is returned;
// the value may hold a password.
func (l *ComposeServicesLifecycle) workerEnvNamingAnAlias() (key, service string) {
	for _, entry := range l.workerEnvironment {
		k, value, _ := strings.Cut(entry, "=")
		for _, s := range l.services {
			if namesHost(value, s.Name) {
				return k, s.Name
			}
		}
	}
	return "", ""
}

// namesHost reports whether host appears in value as a whole host token:
// not inside a longer name, and not as a path or query fragment's part of
// one.
func namesHost(value, host string) bool {
	isNameChar := func(c byte) bool {
		return c == '-' || c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	for from := 0; ; {
		i := strings.Index(value[from:], host)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(host)
		if (start == 0 || !isNameChar(value[start-1])) && (end == len(value) || !isNameChar(value[end])) {
			return true
		}
		from = start + 1
	}
}
