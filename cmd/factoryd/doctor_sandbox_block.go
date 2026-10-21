package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"buildgate/internal/memory"
	"buildgate/internal/projectconfig"
	"buildgate/internal/release"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
)

// doctorCheckSandboxBlock reports the two rows a repository's committed
// sandbox block adds to doctor -target-repo: whether the operator approved
// the block (a run is refused until then) and whether the registries it
// lists are ones the registry proxy serves. A repository with no block has
// no rows.
func doctorCheckSandboxBlock(in doctorInputs) []doctorCheck {
	if in.targetRepo == "" {
		return nil
	}
	abs, err := filepath.Abs(in.targetRepo)
	if err != nil {
		return []doctorCheck{{Name: "sandbox block in " + in.targetRepo, Err: err}}
	}
	cfg, _, err := projectconfig.Load(abs)
	if err != nil {
		return []doctorCheck{{Name: "sandbox block in " + in.targetRepo, Err: err, Fix: "fix the repo's committed " + projectconfig.FileName}}
	}
	if cfg == nil || cfg.Sandbox == nil {
		return nil
	}
	root := release.RepositoryRoot(abs)
	hash := cfg.Sandbox.SHA256()
	return []doctorCheck{
		doctorSandboxApprovalCheck(in.dataDir, root, hash),
		doctorSandboxRegistriesCheck(in, cfg.Sandbox),
	}
}

func doctorSandboxApprovalCheck(dataDir, root, hash string) doctorCheck {
	key := memory.StoreKey(filepath.Base(root), root)
	if a, ok := projectconfig.OpenApprovals(dataDir).Approved(key, hash); ok {
		return doctorCheck{Name: fmt.Sprintf("sandbox block %s: approved by %s at %s", hash[:12], sanitize.Line(a.ApprovedBy), sanitize.Line(a.ApprovedAt))}
	}
	return doctorCheck{Name: "sandbox block " + hash[:12], Err: errors.New("not approved: a run against this repository is refused before any sandbox starts"),
		Fix: "read the block with `factoryd sandbox show -repo " + root + "`, then run `factoryd sandbox approve -repo " + root + "`"}
}

func doctorSandboxRegistriesCheck(in doctorInputs, block *projectconfig.Sandbox) doctorCheck {
	name := "sandbox registries: " + registriesLabel(block.Registries)
	if in.settings.RegistryProxyConfigured && !in.settings.RegistryProxy {
		return doctorCheck{Name: name, Err: errors.New("registry_proxy is off, and the sandbox install reaches package registries only through the registry proxy"),
			Fix: "turn registry_proxy on in the session config, or remove the sandbox block"}
	}
	if _, err := sandbox.NarrowRegistryRoutes(sandbox.DefaultRegistryProxyRoutes(), block.Registries); err != nil {
		return doctorCheck{Name: name, Err: err, Fix: "list only registries the registry proxy serves: " + strings.Join(projectconfig.SandboxRegistries, ", ")}
	}
	return doctorCheck{Name: name + " served by the registry proxy"}
}

func registriesLabel(registries []string) string {
	if len(registries) == 0 {
		return "all the proxy serves"
	}
	return strings.Join(registries, ", ")
}
