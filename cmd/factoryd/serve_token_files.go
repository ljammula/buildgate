package main

import (
	"fmt"
	"path/filepath"
)

// serveOverrideTokenFileName and serveReadTokenFileName are the files beside
// the session config that hold the override and read tokens of a `serve`
// whose environment sets neither FACTORYD_API_OVERRIDE_TOKEN nor
// FACTORYD_API_READ_TOKEN. `install-service -require-tokens` writes them
// (mode 0600); the plist never carries them. With both, no caller is trusted
// for being local: a forwarder that adds no forwarding header (colima's
// host.docker.internal) makes every container look local
// (safety-contract.md, "Console loopback writes").
const (
	serveOverrideTokenFileName = "serve-override-token"
	serveReadTokenFileName     = "serve-read-token"
)

// serveTokenFilePathFor returns the path of the token file name beside
// configPath (as resolveEffectiveConfigPath resolved it).
func serveTokenFilePathFor(configPath, name string) string {
	return filepath.Join(filepath.Dir(configPath), name)
}

// serveTokenFromFile returns the token in the file name beside configPath,
// or "" when there is no such file. A file that exists and fails
// inspectTokenFile's checks is an error, not "no token": these
// tokens take the no-token relaxations away, so serve must not start
// without them. Its fix keeps the protection: serve never writes these
// files, so the start token's "remove it and rerun" would drop it.
func serveTokenFromFile(configPath, name string) (string, error) {
	token, issue, err := inspectTokenFile(serveTokenFilePathFor(configPath, name))
	if err != nil {
		return "", err
	}
	if issue != nil {
		issue.Fix = "`chmod 600` it if its mode is the reason; otherwise delete it and run `factoryd install-service -force -require-tokens` to write a new one (deleting it alone turns this protection off)"
		return "", issue
	}
	return token, nil
}

// tokensFromFiles fills in the override and read tokens the environment left
// unset, from their files beside the session config.
func (sv *serveRun) tokensFromFiles() error {
	configPath, ok := resolveEffectiveConfigPath(*sv.sf.configPath)
	if !ok {
		return nil
	}
	for _, source := range []struct {
		token *string
		name  string
	}{
		{&sv.overrideToken, serveOverrideTokenFileName},
		{&sv.readToken, serveReadTokenFileName},
	} {
		if *source.token != "" {
			continue
		}
		token, err := serveTokenFromFile(configPath, source.name)
		if err != nil {
			return fmt.Errorf("serve: refusing to start without the token in %s: %w", source.name, err)
		}
		*source.token = token
	}
	return nil
}
