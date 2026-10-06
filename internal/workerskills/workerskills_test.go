package workerskills_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"buildgate/internal/sandbox"
	"buildgate/internal/workerskills"
)

// TestBuiltinSkillsFollowTheWorkerContract guards every built-in against the
// rules that make a skill safe for every harness and consistent with
// buildgate's prompts: portable front matter (pi, codex and copilot all read
// name/description/license), name equal to the folder, accepted by the real
// snapshot exactly as a launch takes it, and none of the instructions that
// broke unattended runs in a live audit.
func TestBuiltinSkillsFollowTheWorkerContract(t *testing.T) {
	names := workerskills.Names()
	if len(names) == 0 {
		t.Fatal("no built-in skills")
	}
	forbidden := regexp.MustCompile(`(?i)\b(ask (the|your) (user|human)|human partner|wait for (the )?user|commit message|do not commit|git push)\b`)
	var sources []sandbox.SkillSource
	for _, name := range names {
		if !workerskills.Has(name) {
			t.Errorf("%s: Has = false", name)
		}
		body, err := fs.ReadFile(workerskills.FS(), name+"/SKILL.md")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		text := string(body)
		parts := strings.SplitN(text, "---\n", 3)
		if len(parts) != 3 || parts[0] != "" {
			t.Errorf("%s: SKILL.md must start with a --- front-matter block", name)
			continue
		}
		var fm map[string]any
		if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
			t.Errorf("%s: front matter: %v", name, err)
			continue
		}
		for key := range fm {
			if !slices.Contains([]string{"name", "description", "license"}, key) {
				t.Errorf("%s: front-matter key %q is not portable across pi, codex and copilot", name, key)
			}
		}
		if fm["name"] != name {
			t.Errorf("%s: front-matter name %v must equal the folder name", name, fm["name"])
		}
		if d, _ := fm["description"].(string); len(d) < 20 || len(d) > 1024 {
			t.Errorf("%s: description must be 20-1024 characters, got %d", name, len(d))
		}
		if m := forbidden.FindString(text); m != "" {
			t.Errorf("%s: contains %q, which conflicts with unattended buildgate runs", name, m)
		}
		// One name for the command that decides a round: the build prompt's
		// checklist names the resolved command (which may not be a ticket
		// header), so a skill pointing at "the ticket's Verify-Command"
		// would send the agent looking for a line that may not exist.
		if strings.Contains(text, "ticket's `Verify-Command`") {
			t.Errorf("%s: refer to \"the verify command named in the build prompt's checklist\", not the ticket's Verify-Command header", name)
		}
		sources = append(sources, sandbox.SkillSource{Name: name, Builtin: true})
	}
	dst := filepath.Join(t.TempDir(), "snap")
	if _, err := sandbox.SnapshotSkills(t.TempDir(), dst, sources); err != nil {
		t.Fatalf("the full built-in set must pass the worker snapshot: %v", err)
	}
	for _, name := range names {
		want, _ := fs.ReadFile(workerskills.FS(), name+"/SKILL.md")
		got, err := os.ReadFile(filepath.Join(dst, name, "SKILL.md"))
		if err != nil || string(got) != string(want) {
			t.Errorf("%s: snapshot differs from the embedded skill (%v)", name, err)
		}
	}
	if workerskills.Has("no-such-skill") {
		t.Error("Has(no-such-skill) = true")
	}
}
