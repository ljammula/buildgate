package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/sessionconfig"
)

const kafkaAndPostgresCompose = "services:\n  db:\n    image: postgres:16\n  kafka:\n    image: apache/kafka:3.8.0\n  search:\n    image: ghcr.io/acme/search:1\n  queue:\n    image: apache/activemq:6\n"

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestComposeRegistriesToAllowNamesEachMissingPrefixOnce: the prefixes
// offered are exactly the narrow namespaces of the rejected images, each
// once, and nothing for an image already allowed.
func TestComposeRegistriesToAllowNamesEachMissingPrefixOnce(t *testing.T) {
	repo := commitComposeFile(t, kafkaAndPostgresCompose)
	got, err := composeRegistriesToAllow(sessionconfig.DefaultSettings(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if want := "docker.io/apache/ ghcr.io/acme/"; strings.Join(got, " ") != want {
		t.Fatalf("missing prefixes = %q, want %q", got, want)
	}
	settings := sessionconfig.DefaultSettings()
	settings.ComposeServicesAllowedRegistries = append(settings.ComposeServicesAllowedRegistries, got...)
	if again, err := composeRegistriesToAllow(settings, repo); err != nil || len(again) != 0 {
		t.Fatalf("with the prefixes allowed, still missing %q (err %v)", again, err)
	}
}

// TestOfferComposeRegistriesAddsOnlyOnAYes: the allow-list is the
// operator's limit on what a repository's compose file may select. Without
// a yes the config is untouched; with one, exactly the missing prefixes are
// added, and the repo's compose file then passes.
func TestOfferComposeRegistriesAddsOnlyOnAYes(t *testing.T) {
	repo := commitComposeFile(t, kafkaAndPostgresCompose)
	const original = "data_dir: /tmp/x\ncode_review_policy: required\n"
	cases := []struct {
		name        string
		yes         bool
		interactive bool
		stdin       string
		wantChanged bool
	}{
		{"not a terminal and no -yes: only says what it would do", false, false, "", false},
		{"asked and declined", false, true, "n\n", false},
		{"asked and left at the default", false, true, "\n", false},
		{"asked and accepted", false, true, "y\n", true},
		{"-yes", true, false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := writeConfig(t, original)
			var out strings.Builder
			changed := offerComposeRegistries(newQuickstartPrompter(strings.NewReader(tc.stdin)), &out, tc.yes, tc.interactive, sessionconfig.DefaultSettings(), repo, cfgPath, "factoryd doctor -fix")
			after, _ := os.ReadFile(cfgPath)
			if changed != tc.wantChanged || (string(after) != original) != tc.wantChanged {
				t.Fatalf("changed = %v, config changed = %v, want %v\n%s\n%s", changed, string(after) != original, tc.wantChanged, out.String(), after)
			}
			if !strings.Contains(out.String(), "docker.io/apache/, ghcr.io/acme/") {
				t.Errorf("the offer does not name the prefixes:\n%s", out.String())
			}
			if !tc.wantChanged {
				return
			}
			if !strings.HasPrefix(string(after), original) {
				t.Errorf("lines the operator wrote were changed:\n%s", after)
			}
			settings, err := loadSettingsForConfig(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if want := "docker.io/library/ docker.io/apache/ ghcr.io/acme/"; strings.Join(settings.ComposeServicesAllowedRegistries, " ") != want {
				t.Errorf("allowed registries = %q, want %q (the default kept, the two added)", settings.ComposeServicesAllowedRegistries, want)
			}
			if missing, err := composeRegistriesToAllow(settings, repo); err != nil || len(missing) != 0 {
				t.Errorf("after the fix, still missing %q (err %v)", missing, err)
			}
		})
	}
}

// TestOfferComposeRegistriesIsSilentWhenNothingIsMissing: a repo with no
// compose file, or one whose images are all allowed, gets no prompt.
func TestOfferComposeRegistriesIsSilentWhenNothingIsMissing(t *testing.T) {
	for name, content := range map[string]string{"all allowed": "services:\n  db:\n    image: postgres:16\n", "another rejection": "services:\n  db:\n    image: postgres:16\n    privileged: true\n"} {
		t.Run(name, func(t *testing.T) {
			cfgPath := writeConfig(t, "data_dir: /tmp/x\n")
			var out strings.Builder
			if offerComposeRegistries(newQuickstartPrompter(strings.NewReader("y\n")), &out, true, true, sessionconfig.DefaultSettings(), commitComposeFile(t, content), cfgPath, "factoryd doctor -fix") || out.Len() != 0 {
				t.Fatalf("offered or changed something:\n%s", out.String())
			}
		})
	}
}

// TestAllowedRegistriesTextEditsOnlyTheList covers the three shapes of
// config the edit meets.
func TestAllowedRegistriesTextEditsOnlyTheList(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	add := []string{"docker.io/apache/"}
	t.Run("an existing block list gets the item after its last one, at its indentation", func(t *testing.T) {
		config := "a: 1\ncompose_services_allowed_registries:\n  - docker.io/library/\n  # ours\n  - ghcr.io/acme/\nz: 2\n"
		got, err := allowedRegistriesText(config, nil, add, "w", now)
		if err != nil {
			t.Fatal(err)
		}
		if want := "a: 1\ncompose_services_allowed_registries:\n  - docker.io/library/\n  # ours\n  - ghcr.io/acme/\n  - docker.io/apache/\nz: 2\n"; got != want {
			t.Fatalf("got:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("a config without the key gets the list in force plus the item", func(t *testing.T) {
		got, err := allowedRegistriesText("a: 1", []string{"docker.io/library/"}, add, "factoryd doctor -fix", now)
		if err != nil {
			t.Fatal(err)
		}
		if want := "a: 1\n# added by factoryd doctor -fix 2026-10-06: registries a target repo's compose images come from\ncompose_services_allowed_registries:\n    - docker.io/library/\n    - docker.io/apache/\n"; got != want {
			t.Fatalf("got:\n%s\nwant:\n%s", got, want)
		}
	})
	for name, config := range map[string]string{"a flow list": "compose_services_allowed_registries: [docker.io/library/]\n", "an empty key": "compose_services_allowed_registries:\nz: 2\n"} {
		t.Run(name+" is left for the operator", func(t *testing.T) {
			if got, err := allowedRegistriesText(config, nil, add, "w", now); err == nil || !strings.Contains(err.Error(), "by hand") {
				t.Fatalf("got %q, err %v; want a refusal naming the manual edit", got, err)
			}
		})
	}
}

// TestAddAllowedRegistriesRestoresAConfigThatWouldNotLoad: an edit that
// leaves the config unloadable is undone.
func TestAddAllowedRegistriesRestoresAConfigThatWouldNotLoad(t *testing.T) {
	const broken = "data_dir: /tmp/x\nno_such_key: 1\n"
	cfgPath := writeConfig(t, broken)
	if err := addAllowedRegistries(cfgPath, []string{"docker.io/library/"}, []string{"docker.io/apache/"}, "w"); err == nil {
		t.Fatal("want an error for a config that does not load")
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != broken {
		t.Fatalf("config was not restored:\n%s", after)
	}
}

// TestDoctorTargetRepoPointsARejectedImageAtTheFix: the check that fails on
// an unlisted image names the command that offers to allow it.
func TestDoctorTargetRepoPointsARejectedImageAtTheFix(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  kafka:\n    image: apache/kafka:3.8.0\n")
	var out strings.Builder
	runDoctorChecks(doctorTargetRepoChecks(t.Context(), doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "8589934592"))), &out)
	if want := "-fix` offers to add docker.io/apache/ to compose_services_allowed_registries"; !strings.Contains(out.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, out.String())
	}
}

// TestDoctorFixRetakesTheComposeChecksAfterAllowingARegistry: the run that
// adds the prefix reports the repo's services as they now stand, not the
// rejection it has just fixed; a run that adds nothing keeps its checks.
func TestDoctorFixRetakesTheComposeChecksAfterAllowingARegistry(t *testing.T) {
	repo := commitComposeFile(t, "services:\n  kafka:\n    image: apache/kafka:3.8.0\n")
	in := doctorTargetRepoInputs(repo, fakeDockerMemTotal(t, "8589934592"))
	before := append([]doctorCheck{{Name: "unrelated"}}, doctorTargetRepoChecks(t.Context(), in)...)

	var out strings.Builder
	declined := doctorOfferComposeRegistries(before, in, writeConfig(t, "data_dir: /tmp/x\n"), false, strings.NewReader(""), &out)
	if len(declined) != len(before) {
		t.Fatalf("checks changed without a yes: %d -> %d", len(before), len(declined))
	}

	after := doctorOfferComposeRegistries(before, in, writeConfig(t, "data_dir: /tmp/x\n"), true, strings.NewReader(""), &out)
	var report strings.Builder
	if failed := runDoctorChecks(after, &report); failed != 0 {
		t.Fatalf("still failing after the fix:\n%s", report.String())
	}
	for _, want := range []string{"unrelated", "ok    compose service kafka (apache/kafka:3.8.0)"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, report.String())
		}
	}
}
