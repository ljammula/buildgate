package evidence

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

type lockEntry struct {
	identity string
	name     string
	version  string
	source   string
}

func diffLockEntries(base, result map[string]lockEntry) []DependencyChange {
	keys := map[string]bool{}
	for k := range base {
		keys[k] = true
	}
	for k := range result {
		keys[k] = true
	}
	changes := make([]DependencyChange, 0)
	seen := map[string]bool{}
	for key := range keys {
		old, now := base[key], result[key]
		if old.name == now.name && old.version == now.version && old.source == now.source {
			continue
		}
		change := DependencyChange{Name: now.name, Base: old.version, Result: now.version, BaseSourceReference: old.source, ResultSourceReference: now.source}
		if change.Name == "" {
			change.Name = old.name
		}
		encoded := fmt.Sprintf("%q|%q|%q|%q|%q", change.Name, change.Base, change.Result, change.BaseSourceReference, change.ResultSourceReference)
		if !seen[encoded] {
			changes = append(changes, change)
			seen[encoded] = true
		}
	}
	SortDependencyChanges(changes)
	return changes
}

// SortDependencyChanges establishes one deterministic order for evidence
// collected from multiple lockfile ecosystems and paths.
func SortDependencyChanges(changes []DependencyChange) {
	sort.Slice(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		for _, pair := range [][2]string{
			{a.Name, b.Name}, {a.Base, b.Base}, {a.Result, b.Result},
			{a.BaseScope, b.BaseScope}, {a.ResultScope, b.ResultScope},
			{a.BaseSourceReference, b.BaseSourceReference}, {a.ResultSourceReference, b.ResultSourceReference},
			{a.BaseDistReference, b.BaseDistReference}, {a.ResultDistReference, b.ResultDistReference},
			{a.BaseDistURL, b.BaseDistURL}, {a.ResultDistURL, b.ResultDistURL},
			{a.BaseDistShasum, b.BaseDistShasum}, {a.ResultDistShasum, b.ResultDistShasum},
			{a.BaseDistType, b.BaseDistType}, {a.ResultDistType, b.ResultDistType},
			{a.BaseSourceURL, b.BaseSourceURL}, {a.ResultSourceURL, b.ResultSourceURL},
			{a.BaseSourceType, b.BaseSourceType}, {a.ResultSourceType, b.ResultSourceType},
			{a.BaseAlias, b.BaseAlias}, {a.ResultAlias, b.ResultAlias},
			{a.BaseAliasNormalized, b.BaseAliasNormalized}, {a.ResultAliasNormalized, b.ResultAliasNormalized},
		} {
			if pair[0] != pair[1] {
				return pair[0] < pair[1]
			}
		}
		return false
	})
}

func addLockEntry(entries map[string]lockEntry, entry lockEntry) error {
	if entry.identity == "" || entry.name == "" || entry.version == "" {
		return fmt.Errorf("lock entry requires identity, name, and version")
	}
	if prior, ok := entries[entry.identity]; ok {
		if prior != entry {
			return fmt.Errorf("conflicting duplicate lock entry %q", entry.identity)
		}
		return nil
	}
	entries[entry.identity] = entry
	return nil
}

func YarnLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := parseYarnLock(base)
	if err != nil {
		return nil, fmt.Errorf("parse base yarn.lock: %w", err)
	}
	now, err := parseYarnLock(result)
	if err != nil {
		return nil, fmt.Errorf("parse result yarn.lock: %w", err)
	}
	return diffLockEntries(old, now), nil
}

func parseYarnLock(data []byte) (map[string]lockEntry, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]lockEntry{}, nil
	}
	root, err := decodeYAMLDocument(data)
	if err == nil && root.Kind == yaml.MappingNode {
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "__metadata" {
				return parseYarnBerry(root)
			}
		}
	}
	return parseYarnClassic(data)
}

func parseYarnBerry(root *yaml.Node) (map[string]lockEntry, error) {
	entries := map[string]lockEntry{}
	for i := 0; i < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Value == "__metadata" {
			continue
		}
		if value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("entry %q must be a mapping", key.Value)
		}
		version, resolution, checksum := "", "", ""
		for j := 0; j < len(value.Content); j += 2 {
			switch value.Content[j].Value {
			case "version":
				if value.Content[j+1].Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("entry %q version must be a scalar", key.Value)
				}
				version = value.Content[j+1].Value
			case "resolution":
				if value.Content[j+1].Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("entry %q resolution must be a scalar", key.Value)
				}
				resolution = value.Content[j+1].Value
			case "checksum":
				if value.Content[j+1].Kind != yaml.ScalarNode {
					return nil, fmt.Errorf("entry %q checksum must be a scalar", key.Value)
				}
				checksum = value.Content[j+1].Value
			}
		}
		if version == "" {
			return nil, fmt.Errorf("entry %q has no version", key.Value)
		}
		descriptors, err := normalizeYarnDescriptors(key.Value)
		if err != nil {
			return nil, err
		}
		name := ""
		for _, descriptor := range strings.Split(descriptors, "\x00") {
			descriptorName, err := yarnDescriptorName(descriptor)
			if err != nil {
				return nil, err
			}
			if name == "" {
				name = descriptorName
			} else if descriptorName != name {
				return nil, fmt.Errorf("yarn descriptor set %q mixes package names", key.Value)
			}
		}
		if err := addLockEntry(entries, lockEntry{descriptors, name, version, resolution + "|" + checksum}); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func decodeYAMLDocument(data []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if len(document.Content) != 1 {
		return nil, fmt.Errorf("YAML document has no root")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing YAML document")
		}
		return nil, err
	}
	return document.Content[0], nil
}

func parseYarnClassic(data []byte) (map[string]lockEntry, error) {
	type pending struct{ descriptors, name, version, source string }
	entries := map[string]lockEntry{}
	var current *pending
	flush := func() error {
		if current == nil {
			return nil
		}
		if current.version == "" {
			return fmt.Errorf("yarn entry %q has no version", current.descriptors)
		}
		if err := addLockEntry(entries, lockEntry{current.descriptors, current.name, current.version, current.source}); err != nil {
			return err
		}
		return nil
	}
	s := bufio.NewScanner(bytes.NewReader(data))
	lineNo := 0
	for s.Scan() {
		lineNo++
		line := strings.TrimRight(s.Text(), "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if len(line) > 0 && line[0] != ' ' && strings.HasSuffix(trimmed, ":") {
			if err := flush(); err != nil {
				return nil, err
			}
			header := strings.TrimSuffix(trimmed, ":")
			descriptors, err := normalizeYarnDescriptors(header)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			name, err := yarnDescriptorName(strings.Split(descriptors, "\x00")[0])
			if err != nil {
				return nil, err
			}
			current = &pending{descriptors: descriptors, name: name}
			continue
		}
		if current == nil {
			return nil, fmt.Errorf("line %d appears outside an entry", lineNo)
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
		value = strings.TrimSpace(value)
		if fields[0] == "version" {
			current.version = unquoteYarn(value)
		}
		if fields[0] == "resolved" || fields[0] == "checksum" || fields[0] == "integrity" {
			if current.source != "" {
				current.source += "|"
			}
			current.source += unquoteYarn(value)
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return entries, nil
}

func normalizeYarnDescriptors(header string) (string, error) {
	parts := []string{}
	var b strings.Builder
	quoted := false
	for _, r := range header {
		if r == '"' {
			quoted = !quoted
		}
		if r == ',' && !quoted {
			parts = append(parts, unquoteYarn(strings.TrimSpace(b.String())))
			b.Reset()
		} else {
			b.WriteRune(r)
		}
	}
	if quoted {
		return "", fmt.Errorf("unterminated descriptor quote")
	}
	parts = append(parts, unquoteYarn(strings.TrimSpace(b.String())))
	for _, p := range parts {
		if p == "" {
			return "", fmt.Errorf("empty Yarn descriptor")
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00"), nil
}
func unquoteYarn(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if v, err := strconv.Unquote(s); err == nil {
			return v
		}
	}
	return s
}

// yarnDescriptorName finds the package-name half of a Yarn lock descriptor
// ("name@range"), splitting on the FIRST '@' after any leading scope
// marker — not the last. An npm-alias descriptor legitimately embeds a
// second, unrelated '@' in its range half, e.g.
// "my-alias@npm:real-package@^1.0.0" (alias "my-alias" for
// "real-package"), or the scoped form
// "@scope/name@npm:@scope/other@^1.0.0"; splitting on the last '@'
// instead would fold part of the aliased target into the name.
func yarnDescriptorName(descriptor string) (string, error) {
	descriptor = strings.TrimSpace(descriptor)
	searchFrom := 0
	if strings.HasPrefix(descriptor, "@") {
		// Skip the scope's own leading '@' so it isn't mistaken for the
		// name/range separator.
		searchFrom = 1
	}
	at := strings.Index(descriptor[searchFrom:], "@")
	if at < 0 {
		return "", fmt.Errorf("invalid Yarn descriptor %q", descriptor)
	}
	at += searchFrom
	name := descriptor[:at]
	if strings.HasPrefix(name, "@") && !strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid scoped Yarn descriptor %q", descriptor)
	}
	return name, nil
}

func PnpmLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := parsePnpmLock(base)
	if err != nil {
		return nil, fmt.Errorf("parse base pnpm-lock.yaml: %w", err)
	}
	now, err := parsePnpmLock(result)
	if err != nil {
		return nil, fmt.Errorf("parse result pnpm-lock.yaml: %w", err)
	}
	return diffLockEntries(old, now), nil
}

// parsePnpmLock supports pnpm's modern packages/snapshots maps. Importer-only
// and other historical shapes are rejected as unsupported rather than being
// mistaken for an empty dependency set.
func parsePnpmLock(data []byte) (map[string]lockEntry, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]lockEntry{}, nil
	}
	root, err := decodeYAMLDocument(data)
	if err != nil {
		return nil, err
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top-level document must be a mapping")
	}
	sections := map[string]map[string]lockEntry{}
	top := root
	foundSection := false
	for i := 0; i < len(top.Content); i += 2 {
		section, value := top.Content[i].Value, top.Content[i+1]
		if section != "packages" && section != "snapshots" {
			continue
		}
		foundSection = true
		if value.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s must be a mapping", section)
		}
		entries := map[string]lockEntry{}
		for j := 0; j < len(value.Content); j += 2 {
			locator, node := value.Content[j].Value, value.Content[j+1]
			locator = strings.TrimPrefix(locator, "/")
			name, version, err := pnpmLocator(locator)
			if err != nil {
				return nil, err
			}
			if node.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("package locator %q must be a mapping", locator)
			}
			source := ""
			if resolution := yamlMapValue(node, "resolution"); resolution != nil {
				var err error
				source, err = yamlScalarMap(resolution)
				if err != nil {
					return nil, fmt.Errorf("package locator %q: %w", locator, err)
				}
			}
			entry := lockEntry{locator, name, version, source}
			if err := addLockEntry(entries, entry); err != nil {
				return nil, err
			}
		}
		sections[section] = entries
	}
	if !foundSection {
		return nil, fmt.Errorf("unsupported pnpm lockfile: no packages or snapshots section")
	}
	entries := map[string]lockEntry{}
	locators := map[string]bool{}
	for locator := range sections["packages"] {
		locators[locator] = true
	}
	for locator := range sections["snapshots"] {
		locators[locator] = true
	}
	for locator := range locators {
		packageEntry, hasPackage := sections["packages"][locator]
		snapshotEntry, hasSnapshot := sections["snapshots"][locator]
		chosen := snapshotEntry
		if hasPackage && (!hasSnapshot || packageEntry.source != "") {
			chosen = packageEntry
		} else if hasPackage {
			chosen = packageEntry
			chosen.source = snapshotEntry.source
		}
		if err := addLockEntry(entries, chosen); err != nil {
			return nil, err
		}
	}
	return entries, nil
}
func yamlMapValue(node *yaml.Node, want string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == want {
			return node.Content[i+1]
		}
	}
	return nil
}
func yamlScalarMap(node *yaml.Node) (string, error) {
	if node.Kind == yaml.ScalarNode {
		return node.Value, nil
	}
	if node.Kind != yaml.MappingNode {
		return "", fmt.Errorf("resolution must be a scalar or mapping")
	}
	keys := []string{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		value, err := yamlScalarMap(node.Content[i+1])
		if err != nil {
			return "", err
		}
		keys = append(keys, node.Content[i].Value+"="+value)
	}
	sort.Strings(keys)
	return strings.Join(keys, "|"), nil
}
func pnpmLocator(locator string) (string, string, error) {
	locator = strings.TrimPrefix(locator, "/")
	at := strings.Index(locator, "@")
	if strings.HasPrefix(locator, "@") {
		at = strings.Index(locator[1:], "@")
		if at >= 0 {
			at++
		}
	}
	if at <= 0 || at == len(locator)-1 {
		return "", "", fmt.Errorf("invalid pnpm package locator %q", locator)
	}
	name := locator[:at]
	if strings.HasPrefix(name, "@") && !strings.Contains(name, "/") {
		return "", "", fmt.Errorf("invalid scoped pnpm locator %q", locator)
	}
	return name, locator[at+1:], nil
}

func GemfileLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	old, err := parseGemfileLock(base)
	if err != nil {
		return nil, fmt.Errorf("parse base Gemfile.lock: %w", err)
	}
	now, err := parseGemfileLock(result)
	if err != nil {
		return nil, fmt.Errorf("parse result Gemfile.lock: %w", err)
	}
	return diffLockEntries(old, now), nil
}

// parseGemfileLock supports Bundler GEM, GIT, and PATH sections. The source
// kind, remote, and GIT revision are retained in identity/evidence; nested
// dependency requirement lines are intentionally not lock entries.
func parseGemfileLock(data []byte) (map[string]lockEntry, error) {
	entries := map[string]lockEntry{}
	section, source, inSpecs := "", "", false
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		line := strings.TrimRight(s.Text(), "\r")
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if len(line) > 0 && line[0] != ' ' {
			section, source, inSpecs = trim, "", false
			if section != "GEM" && section != "GIT" && section != "PATH" {
				section = ""
			}
			continue
		}
		if section == "" {
			continue
		}
		if strings.HasPrefix(trim, "remote:") {
			source = section + "|" + strings.TrimSpace(strings.TrimPrefix(trim, "remote:"))
			continue
		}
		if strings.HasPrefix(trim, "revision:") {
			source += "|revision=" + strings.TrimSpace(strings.TrimPrefix(trim, "revision:"))
			continue
		}
		if trim == "specs:" {
			inSpecs = true
			continue
		}
		if inSpecs && len(line)-len(strings.TrimLeft(line, " ")) == 4 {
			name, version, ok := gemSpec(trim)
			if !ok {
				return nil, fmt.Errorf("invalid gem specification %q", trim)
			}
			if err := addLockEntry(entries, lockEntry{source + "|" + name, name, version, source}); err != nil {
				return nil, err
			}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}
func gemSpec(s string) (string, string, bool) {
	close := strings.LastIndex(s, ")")
	open := strings.LastIndex(s[:max(0, close)], " (")
	if close <= 0 || open <= 0 {
		return "", "", false
	}
	name, version := strings.TrimSpace(s[:open]), strings.TrimSpace(s[open+2:close])
	return name, version, name != "" && version != ""
}
func PoetryLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	return tomlLockDependencyDiff(base, result, "poetry.lock")
}
func CargoLockDependencyDiff(base, result []byte) ([]DependencyChange, error) {
	return tomlLockDependencyDiff(base, result, "Cargo.lock")
}

// tomlLockDependencyDiff supports Poetry/Cargo [[package]] tables with string name and
// version fields. Poetry source may be a string or a table whose fields are
// strings; Poetry package files arrays are retained as canonical integrity
// evidence. Cargo source/checksum are retained similarly. Other package
// fields are deliberately ignored.
func tomlLockDependencyDiff(base, result []byte, format string) ([]DependencyChange, error) {
	old, err := parseTOMLLock(base, format)
	if err != nil {
		return nil, fmt.Errorf("parse base %s: %w", format, err)
	}
	now, err := parseTOMLLock(result, format)
	if err != nil {
		return nil, fmt.Errorf("parse result %s: %w", format, err)
	}
	return diffLockEntries(old, now), nil
}

// parseTOMLLock supports Poetry/Cargo [[package]] tables with string name and
// version fields. Poetry source may be a string or a table whose fields are
// strings; Poetry package files arrays are retained as canonical integrity
// evidence. Cargo source/checksum are retained similarly. Other package
// fields are deliberately ignored.
func parseTOMLLock(data []byte, format string) (map[string]lockEntry, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]lockEntry{}, nil
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	values, ok := raw["package"]
	if !ok {
		return map[string]lockEntry{}, nil
	}
	packageValues, ok := tomlSlice(values)
	if !ok {
		return nil, fmt.Errorf("package must be an array of tables")
	}
	entries := map[string]lockEntry{}
	for _, value := range packageValues {
		pkg, ok := tomlMap(value)
		if !ok {
			return nil, fmt.Errorf("package entry must be a table")
		}
		name, nameOK := pkg["name"].(string)
		version, versionOK := pkg["version"].(string)
		if !nameOK || !versionOK || name == "" || version == "" {
			return nil, fmt.Errorf("package requires nonempty name and version")
		}
		source := ""
		if rawSource, present := pkg["source"]; present {
			switch v := rawSource.(type) {
			case string:
				source = v
			default:
				sourceMap, ok := tomlMap(v)
				if !ok {
					return nil, fmt.Errorf("package %q source must be a string or table", name)
				}
				keys := []string{}
				for k, rawValue := range sourceMap {
					value, ok := rawValue.(string)
					if !ok {
						return nil, fmt.Errorf("package %q source field %q must be a string", name, k)
					}
					keys = append(keys, k+"="+value)
				}
				sort.Strings(keys)
				source = strings.Join(keys, "|")
			}
		}
		identitySource := source
		if format == "poetry.lock" {
			if files, present := pkg["files"]; present {
				encoded, err := tomlCanonical(files)
				if err != nil {
					return nil, fmt.Errorf("package %q files: %w", name, err)
				}
				if encoded != "" {
					source += "|files=" + encoded
				}
			}
		}
		if rawChecksum, present := pkg["checksum"]; present {
			checksum, ok := rawChecksum.(string)
			if !ok {
				return nil, fmt.Errorf("package %q checksum must be a string", name)
			}
			if checksum != "" {
				source += "|checksum=" + checksum
			}
		}
		// Version is part of identity so lockfiles containing two versions of
		// one package remain lossless instead of being collapsed together.
		identity := name + "|" + version + "|" + identitySource
		if err := addLockEntry(entries, lockEntry{identity, name, version, source}); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func tomlCanonical(value any) (string, error) {
	if text, ok := value.(string); ok {
		return strconv.Quote(text), nil
	}
	if values, ok := tomlSlice(value); ok {
		parts := make([]string, len(values))
		for i, item := range values {
			encoded, err := tomlCanonical(item)
			if err != nil {
				return "", err
			}
			parts[i] = encoded
		}
		return "[" + strings.Join(parts, ",") + "]", nil
	}
	if fields, ok := tomlMap(value); ok {
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			encoded, err := tomlCanonical(fields[key])
			if err != nil {
				return "", err
			}
			parts = append(parts, key+"="+encoded)
		}
		return "{" + strings.Join(parts, ",") + "}", nil
	}
	switch value.(type) {
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(value), nil
	default:
		return "", fmt.Errorf("unsupported TOML value type %T", value)
	}
}

func tomlSlice(value any) ([]any, bool) {
	v := reflect.ValueOf(value)
	if !v.IsValid() || (v.Kind() != reflect.Array && v.Kind() != reflect.Slice) {
		return nil, false
	}
	out := make([]any, v.Len())
	for i := range out {
		out[i] = v.Index(i).Interface()
	}
	return out, true
}

func tomlMap(value any) (map[string]any, bool) {
	if out, ok := value.(map[string]any); ok {
		return out, true
	}
	v := reflect.ValueOf(value)
	if !v.IsValid() || v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return nil, false
	}
	out := make(map[string]any, v.Len())
	for _, key := range v.MapKeys() {
		out[key.String()] = v.MapIndex(key).Interface()
	}
	return out, true
}
