package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// retiredRouteKeyField is the route field a static route once named its key's
// environment variable in; sessionconfig.Load refuses it.
const retiredRouteKeyField = "credential_env"

// errConfigNotConverted marks a session config configure-images left as it
// was: one of its routes' retired key could not be converted
// (convertCredentialEnvRoutes), or the config could not be rewritten in
// place (writeFileAtomic refused it, as it does a symlink).
var errConfigNotConverted = errors.New("not converted")

// routeKeyWrite is one key file a conversion or quickstart writes; replace
// is quickstart's leave to replace another key there (replaceSecretFile).
type routeKeyWrite struct {
	path, value string
	replace     bool
}

// convertCredentialEnvRoutes rewrites, in the session config at path, each
// route that still names the retired field into one that names a
// credential_file, for `make install`'s configure-images step. For each
// such route:
//
//   - the variable it names is set in this process's environment: its
//     trimmed value is written to routeCredentialFilePath (mode 0600) and the
//     line becomes credential_file: <that path>. A key file already there
//     holding the same key is kept; one holding another key, or a symlink,
//     stops the conversion.
//   - the variable is not set and the route has allow_no_credential: true:
//     the line is dropped.
//   - otherwise the conversion stops, as it does for a route that also names
//     credential_file already, and for a config writeFileAtomic refuses
//     (a symlink).
//
// A conversion that stops leaves the config and every key file as they were
// and returns an error wrapping errConfigNotConverted whose text names the
// profile, the route and the fix. The config is rewritten only after its key
// files are written, through writeFileAtomic with its own mode, and a key
// file this call created is removed when that rewrite fails. No key is
// written to the config or to w. A config that does not exist or names no
// retired field is left untouched.
func convertCredentialEnvRoutes(path string, w io.Writer) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		// Loading the config reports what is wrong with it.
		return nil
	}
	routes := yamlMappingValue(doc.Content[0], "routes")
	if routes == nil || routes.Kind != yaml.MappingNode {
		return nil
	}
	var writes []routeKeyWrite
	var notes []string
	for i := 0; i+1 < len(routes.Content); i += 2 {
		name, route := routes.Content[i].Value, routes.Content[i+1]
		write, note, err := convertCredentialEnvRoute(path, name, route)
		if err != nil {
			return fmt.Errorf("profile %s (%s): routes.%s: %w", configProfileName(path), path, name, err)
		}
		if write != nil {
			writes = append(writes, *write)
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	if len(notes) == 0 {
		return nil
	}
	if err := commitCredentialConversion(path, &doc, writes); err != nil {
		return err
	}
	for _, note := range notes {
		fmt.Fprintf(w, "%s: %s\n", path, note)
	}
	return nil
}

// convertCredentialEnvRoute plans the conversion of one route mapping (see
// convertCredentialEnvRoutes), changing route in place: the key file to
// write (nil when none is needed) and a line saying what changed ("" when
// nothing did). An error wraps errConfigNotConverted.
func convertCredentialEnvRoute(configPath, name string, route *yaml.Node) (*routeKeyWrite, string, error) {
	if route.Kind != yaml.MappingNode {
		return nil, "", nil
	}
	for j := 0; j+1 < len(route.Content); j += 2 {
		if route.Content[j].Value != retiredRouteKeyField {
			continue
		}
		if yamlMappingValue(route, "credential_file") != nil {
			return nil, "", fmt.Errorf("%w: the route names both %s and credential_file: remove %s", errConfigNotConverted, retiredRouteKeyField, retiredRouteKeyField)
		}
		variable := route.Content[j+1].Value
		value := strings.TrimSpace(os.Getenv(variable))
		if value == "" {
			if allow := yamlMappingValue(route, "allow_no_credential"); allow != nil && allow.Value == "true" {
				route.Content = append(route.Content[:j], route.Content[j+2:]...)
				return nil, fmt.Sprintf("routes.%s: dropped %s (the route has allow_no_credential: true)", name, retiredRouteKeyField), nil
			}
			return nil, "", fmt.Errorf("%w: %s names $%s, which this shell does not set: set it and run make install again, or write the key to a file of mode 0600 and name it in routes.%s.credential_file", errConfigNotConverted, retiredRouteKeyField, variable, name)
		}
		target := routeCredentialFilePath(configPath, name)
		holdsOther, err := keyFileHoldsOther(target, value)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %v", errConfigNotConverted, err)
		}
		if holdsOther {
			return nil, "", fmt.Errorf("%w: %s already holds another key: remove it, or set routes.%s.credential_file yourself", errConfigNotConverted, target, name)
		}
		var write *routeKeyWrite
		if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
			write = &routeKeyWrite{path: target, value: value}
		}
		route.Content[j].Value = "credential_file"
		route.Content[j+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: target}
		return write, fmt.Sprintf("routes.%s now reads its key from %s (written from $%s, mode 0600)", name, target, variable), nil
	}
	return nil, "", nil
}

// commitCredentialConversion writes the key files writes names, then doc
// over the config at path with the config's own mode. A key file it created
// is removed again when a later write fails, so no key file is left that no
// config names.
func commitCredentialConversion(path string, doc *yaml.Node, writes []routeKeyWrite) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("rewrite %s: %w", path, err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("rewrite %s: %w", path, err)
	}
	var created []string
	undo := func() {
		for _, p := range created {
			_ = os.Remove(p)
		}
	}
	for _, kw := range writes {
		if err := writeSecretFile(kw.path, kw.value); err != nil {
			undo()
			return fmt.Errorf("profile %s (%s): %w: %v", configProfileName(path), path, errConfigNotConverted, err)
		}
		created = append(created, kw.path)
	}
	if err := writeFileAtomic(path, out.Bytes(), info.Mode().Perm()); err != nil {
		undo()
		return fmt.Errorf("profile %s (%s): %w: rewrite the config: %v", configProfileName(path), path, errConfigNotConverted, err)
	}
	return nil
}

// configProfileName is the profile name of the session config at path: its
// file name without .yml or .yaml.
func configProfileName(path string) string {
	return strings.TrimSuffix(strings.TrimSuffix(filepath.Base(path), ".yml"), ".yaml")
}

// yamlMappingValue is the value node of key in mapping, nil when absent.
func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}
