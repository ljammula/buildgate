package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// allowedRegistriesKey is the session-config key whose list admits a target
// repo's compose images.
const allowedRegistriesKey = "compose_services_allowed_registries"

// composeRegistriesToAllow lists, sorted, the registry/namespace prefixes
// that repo's compose file at HEAD needs and the operator's
// compose_services_allowed_registries does not have: one per service a run
// would reject for its image alone (composeservices.Rejection.AllowRegistry).
// Empty when the repo has no compose file, or nothing is rejected for that.
func composeRegistriesToAllow(settings sessionconfig.Settings, repo string) ([]string, error) {
	spec, err := composeServicesSpecAtCommit(settings, repo, "HEAD")
	if err != nil {
		return nil, err
	}
	if len(spec.ComposeYAML) == 0 {
		return nil, nil
	}
	verdict, err := sandbox.ValidateComposeServices(spec)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var missing []string
	for _, r := range verdict.Rejected {
		if r.AllowRegistry != "" && !seen[r.AllowRegistry] {
			seen[r.AllowRegistry] = true
			missing = append(missing, r.AllowRegistry)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// offerComposeRegistries is the setup-time half of the compose image
// allow-list: when repo's compose file names images under registries the
// operator has not allowed, it names them and, only on the operator's yes
// (doctorFixConfirm), adds exactly those prefixes to the session config.
//
// The list is the operator's limit on what a target repository's compose
// file may select, so nothing adds to it on the repository's say-so: not
// compose parsing, not a run, not this function without a yes. A run
// against a repo with an unlisted image still halts before its build.
// Reports whether the config was changed.
func offerComposeRegistries(p *quickstartPrompter, w io.Writer, yes, interactive bool, settings sessionconfig.Settings, repo, configPath, writer string) bool {
	missing, err := composeRegistriesToAllow(settings, repo)
	if err != nil || len(missing) == 0 {
		return false
	}
	_, cfgPath, found, err := loadConfigForPath(configPath)
	if err != nil || !found {
		fmt.Fprintf(w, "%s's compose file needs %s in %s, and no session config was found to add it to -- run `factoryd init-config` first.\n", repo, strings.Join(missing, ", "), allowedRegistriesKey)
		return false
	}
	desc := fmt.Sprintf("allow %s's compose images by adding %s to %s in %s (every image under each prefix becomes selectable by any target repo's compose file)", repo, strings.Join(missing, ", "), allowedRegistriesKey, cfgPath)
	if !doctorFixConfirm(p, w, yes, interactive, desc) {
		return false
	}
	if err := addAllowedRegistries(cfgPath, settings.ComposeServicesAllowedRegistries, missing, writer); err != nil {
		fmt.Fprintf(w, "could not update %s: %v\n", cfgPath, err)
		return false
	}
	fmt.Fprintf(w, "updated %s: %s now also lists %s\n", cfgPath, allowedRegistriesKey, strings.Join(missing, ", "))
	return true
}

// addAllowedRegistries adds each of add to path's
// compose_services_allowed_registries, changing no other line. The file is
// checked by loading it again; a result that does not load, or does not
// list every added prefix, is undone.
func addAllowedRegistries(path string, effective, add []string, writer string) error {
	original, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	updated, err := allowedRegistriesText(string(original), effective, add, writer, time.Now())
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	cfg, loadErr := sessionconfig.Load(path)
	if loadErr == nil {
		for _, prefix := range add {
			if !slices.Contains(cfg.ComposeServicesAllowedRegistries, prefix) {
				loadErr = fmt.Errorf("%s does not list %s after the edit", allowedRegistriesKey, prefix)
			}
		}
	}
	if loadErr != nil {
		if restoreErr := os.WriteFile(path, original, 0o600); restoreErr != nil {
			return fmt.Errorf("the edited config did not load (%v) and could not be restored: %w", loadErr, restoreErr)
		}
		return fmt.Errorf("the edited config did not load, so it was left as it was: %w", loadErr)
	}
	return nil
}

var (
	allowedRegistriesKeyLine = regexp.MustCompile(`^` + allowedRegistriesKey + `:\s*(#.*)?$`)
	yamlListItemLine         = regexp.MustCompile(`^(\s+)-\s+\S`)
)

// allowedRegistriesText returns config with each of add listed under
// compose_services_allowed_registries. A config without the key gets it
// appended, holding effective (the list in force until now: the key's
// default when it was absent) followed by add, since writing the key
// replaces the default. A config with the key as a block list gets add
// inserted after its last item, at that item's indentation. Any other
// spelling of the key (a flow list, a nested key) is refused, for the
// operator to edit by hand.
func allowedRegistriesText(config string, effective, add []string, writer string, now time.Time) (string, error) {
	lines := strings.Split(config, "\n")
	key := -1
	for i, line := range lines {
		if strings.HasPrefix(line, allowedRegistriesKey+":") {
			if !allowedRegistriesKeyLine.MatchString(line) {
				return "", fmt.Errorf("%s is not written as a block list; add %s to it by hand", allowedRegistriesKey, strings.Join(add, ", "))
			}
			key = i
			break
		}
	}
	if key < 0 {
		var b strings.Builder
		b.WriteString(config)
		if len(config) > 0 && !strings.HasSuffix(config, "\n") {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "# added by %s %s: registries a target repo's compose images come from\n%s:\n", writer, now.Format("2006-01-02"), allowedRegistriesKey)
		for _, prefix := range append(append([]string{}, effective...), add...) {
			fmt.Fprintf(&b, "    - %s\n", quickstartYAMLScalar(prefix))
		}
		return b.String(), nil
	}
	last, indent := -1, ""
	for i := key + 1; i < len(lines); i++ {
		m := yamlListItemLine.FindStringSubmatch(lines[i])
		if m == nil {
			if strings.TrimSpace(lines[i]) == "" || strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
				continue
			}
			break
		}
		last, indent = i, m[1]
	}
	if last < 0 {
		return "", errors.New(allowedRegistriesKey + " has no list items to add after; add " + strings.Join(add, ", ") + " to it by hand")
	}
	items := make([]string, len(add))
	for i, prefix := range add {
		items[i] = indent + "- " + quickstartYAMLScalar(prefix)
	}
	out := append(append(append([]string{}, lines[:last+1]...), items...), lines[last+1:]...)
	return strings.Join(out, "\n"), nil
}

// doctorOfferComposeRegistries is `doctor -target-repo <repo> -fix`'s call
// of offerComposeRegistries, on doctor's own stdin and -yes. Nothing
// without -target-repo, or with compose services off. When the config was
// changed it returns checks with the target repo's compose checks taken
// again under the new list, so the run does not end on the failure it has
// just fixed.
func doctorOfferComposeRegistries(checks []doctorCheck, in doctorInputs, configPath string, yes bool, stdin io.Reader, w io.Writer) []doctorCheck {
	if in.targetRepo == "" || !in.composeServices {
		return checks
	}
	interactive := false
	if f, ok := stdin.(*os.File); ok {
		interactive = quickstartStdinIsInteractive(f)
	}
	if !offerComposeRegistries(newQuickstartPrompter(stdin), w, yes, interactive, in.settings, in.targetRepo, configPath, "factoryd doctor -fix") {
		return checks
	}
	settings, err := loadSettingsForConfig(configPath)
	if err != nil {
		return checks
	}
	stale := map[string]bool{}
	for _, c := range doctorTargetRepoChecks(context.Background(), in) {
		stale[c.Name] = true
	}
	kept := make([]doctorCheck, 0, len(checks))
	for _, c := range checks {
		if !stale[c.Name] {
			kept = append(kept, c)
		}
	}
	in.settings.ComposeServicesAllowedRegistries = settings.ComposeServicesAllowedRegistries
	return append(kept, doctorTargetRepoChecks(context.Background(), in)...)
}
