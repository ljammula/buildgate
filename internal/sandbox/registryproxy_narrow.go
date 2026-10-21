package sandbox

import "fmt"

// registryRoutePrefixes maps a registry name a repository's sandbox block may
// list (projectconfig.SandboxRegistries) to the proxy route prefixes that
// serve it.
var registryRoutePrefixes = map[string][]string{
	"npm":  {npmRoutePrefix},
	"pypi": {pypiRoutePrefix, pypiFilesRoutePrefix},
	"go":   {goProxyRoutePrefix, goSumDBRoutePrefix},
}

// NarrowRegistryRoutes keeps only the routes that serve the named registries.
// It never adds a route: a registry none of routes serves is an error, so a
// repository cannot reach an upstream the operator did not configure. No
// names leaves routes as they are.
func NarrowRegistryRoutes(routes []RegistryProxyRoute, registries []string) ([]RegistryProxyRoute, error) {
	if len(registries) == 0 {
		return routes, nil
	}
	keep := map[string]bool{}
	for _, name := range registries {
		prefixes, ok := registryRoutePrefixes[name]
		if !ok {
			return nil, fmt.Errorf("registry %q is not one the proxy serves", name)
		}
		served := false
		for _, r := range routes {
			for _, prefix := range prefixes {
				if r.Prefix == prefix {
					keep[r.Prefix] = true
					served = true
				}
			}
		}
		if !served {
			return nil, fmt.Errorf("registry %q is not served by this run's registry proxy", name)
		}
	}
	var out []RegistryProxyRoute
	for _, r := range routes {
		if keep[r.Prefix] {
			out = append(out, r)
		}
	}
	return out, nil
}
