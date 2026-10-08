package toolchain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func version(t *testing.T, s string) Version {
	t.Helper()
	v, ok := ParseVersion(s)
	if !ok {
		t.Fatalf("ParseVersion(%q) found no version", s)
	}
	return v
}

func TestParseVersionReadsTheFirstDottedNumber(t *testing.T) {
	for in, want := range map[string]string{
		"go version go1.26.8 linux/arm64": "1.26.8",
		"Python 3.13.15":                  "3.13.15",
		"v22.23.2":                        "22.23.2",
		"1.27":                            "1.27",
	} {
		if got := version(t, in).String(); got != want {
			t.Errorf("ParseVersion(%q) = %q, want %q", in, got, want)
		}
	}
	if _, ok := ParseVersion("lts/*"); ok {
		t.Error("ParseVersion found a version in an alias")
	}
	if version(t, "1.27").Compare(version(t, "1.27.0")) != 0 || version(t, "1.26.8").Compare(version(t, "1.27")) >= 0 {
		t.Error("Compare: 1.27 must equal 1.27.0 and be newer than 1.26.8")
	}
}

// One row per declaration: what an image must have, and whether these
// installed versions do.
func TestDetectAndCheck(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		tool      string
		install   string
		satisfied []string
		refused   []string
	}{
		{"go line is a minimum", map[string]string{"go.mod": "module m\n\ngo 1.27\n"}, Go, "1.27",
			[]string{"1.27.0", "1.27.3", "1.28.1"}, []string{"1.26.8"}},
		{"toolchain line is what to install", map[string]string{"go.mod": "module m\n\ngo 1.27.0\n\ntoolchain go1.27.2\n"}, Go, "1.27.2",
			[]string{"1.27.0", "1.27.2"}, []string{"1.26.8"}},
		{".python-version pins the numbers it states", map[string]string{".python-version": "# pin\n3.12\n"}, Python, "3.12",
			[]string{"3.12.0", "3.12.9"}, []string{"3.13.15", "3.11.4"}},
		{".python-version wins over pyproject.toml", map[string]string{".python-version": "3.11.4\n", "pyproject.toml": "[project]\nrequires-python = \">=3.13\"\n"}, Python, "3.11.4",
			[]string{"3.11.4"}, []string{"3.11.5", "3.13.0"}},
		{"requires-python range", map[string]string{"pyproject.toml": "[project]\nname = \"x\"\nrequires-python = \">=3.10, <3.13\"\n"}, Python, "3.10",
			[]string{"3.10.0", "3.12.9"}, []string{"3.9.18", "3.13.0"}},
		{"requires-python compatible release", map[string]string{"pyproject.toml": "[project]\nrequires-python = \"~=3.11\"\n"}, Python, "3.11",
			[]string{"3.11.0", "3.13.15"}, []string{"3.10.9", "4.0.0"}},
		{"requires-python wildcard", map[string]string{"pyproject.toml": "[project]\nrequires-python = \"==3.12.*\"\n"}, Python, "3.12",
			[]string{"3.12.7"}, []string{"3.13.0"}},
		{".nvmrc major", map[string]string{".nvmrc": "v20\n"}, Node, "20",
			[]string{"20.11.1"}, []string{"22.23.2"}},
		{".node-version when there is no .nvmrc", map[string]string{".node-version": "22.3.0\n"}, Node, "22.3.0",
			[]string{"22.3.0"}, []string{"22.23.2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := Detect(repo(t, tc.files))
			if err != nil || len(reqs) != 1 {
				t.Fatalf("Detect = %+v, %v; want one requirement", reqs, err)
			}
			if reqs[0].Tool != tc.tool || reqs[0].Install.String() != tc.install {
				t.Errorf("requirement = %s install %s, want %s install %s", reqs[0].Tool, reqs[0].Install, tc.tool, tc.install)
			}
			for _, have := range tc.satisfied {
				if f := Check(reqs, Installed{tc.tool: version(t, have)}); !f[0].OK {
					t.Errorf("%s %s does not satisfy %q", tc.tool, have, reqs[0].Spec)
				}
			}
			for _, have := range tc.refused {
				if f := Check(reqs, Installed{tc.tool: version(t, have)}); f[0].OK {
					t.Errorf("%s %s satisfies %q", tc.tool, have, reqs[0].Spec)
				}
			}
		})
	}
}

func TestDetectDeclaresNothingForAnAliasOrNoFile(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"empty repo":                        {},
		"nvm alias":                         {".nvmrc": "lts/*\n"},
		"pyenv name":                        {".python-version": "system\n"},
		"go.mod with no go line":            {"go.mod": "module m\n"},
		"pyproject without requires-python": {"pyproject.toml": "[project]\nname = \"x\"\n"},
	} {
		if reqs, err := Detect(repo(t, files)); err != nil || len(reqs) != 0 {
			t.Errorf("%s: Detect = %+v, %v; want nothing", name, reqs, err)
		}
	}
}

func TestDetectNamesAFileItCannotRead(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"pyproject.toml does not parse": {"pyproject.toml": "[project\n"},
		"requires-python":               {"pyproject.toml": "[project]\nrequires-python = \"python3\"\n"},
	} {
		if _, err := Detect(repo(t, files)); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("Detect = %v, want an error naming %q", err, name)
		}
	}
}

func TestCheckFailsAToolTheImageLacks(t *testing.T) {
	reqs, err := Detect(repo(t, map[string]string{"go.mod": "module m\n\ngo 1.20\n", ".nvmrc": "22\n"}))
	if err != nil || len(reqs) != 2 {
		t.Fatalf("Detect = %+v, %v", reqs, err)
	}
	findings := Check(reqs, Installed{Node: version(t, "22.1.0")})
	if findings[0].Tool != Go || findings[0].OK || !findings[1].OK {
		t.Errorf("findings = %+v, want go missing and node satisfied", findings)
	}
}
