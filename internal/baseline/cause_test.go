package baseline

import (
	"strings"
	"testing"

	"buildgate/internal/run"
)

func TestCauseNamesWhatTheLogShows(t *testing.T) {
	for _, tc := range []struct {
		name, log, cause, detail string
	}{
		{"python module", "/usr/local/bin/python: No module named pytest\n", run.BaselineCauseMissingModule, "pytest"},
		{"python module, quoted", "ModuleNotFoundError: No module named 'yaml.cyaml'\n", run.BaselineCauseMissingModule, "yaml.cyaml"},
		{"sh program", "sh: 1: mvn: not found\n", run.BaselineCauseMissingProgram, "mvn"},
		{"bash program", "bash: line 1: cargo: command not found\n", run.BaselineCauseMissingProgram, "cargo"},
		{"go exec", "exec: \"gcc\": executable file not found in $PATH\n", run.BaselineCauseMissingProgram, "gcc"},
		{"maven parent", "[ERROR]     Non-resolvable parent POM for com.example:AmqDemo:0.0.1-SNAPSHOT: Cannot access central (https://repo.maven.apache.org/maven2) in offline mode and the artifact org.springframework.boot:spring-boot-starter-parent:pom:2.1.8.RELEASE has not been downloaded from it before. @\n",
			run.BaselineCauseDependencyUnavailable, "org.springframework.boot:spring-boot-starter-parent:pom:2.1.8.RELEASE"},
		{"maven missing artifact", "[INFO] Finished at: 2026-10-14T03:45:04Z\n[ERROR] Failed to execute goal org.apache.maven.plugins:maven-surefire-plugin:2.22.2:test (default-test) on project AmqDemo: Unable to generate classpath: org.apache.maven.artifact.resolver.MultipleArtifactsNotFoundException: Missing:\n[ERROR] ----------\n[ERROR] 1) org.apache.maven.surefire:surefire-junit4:jar:2.22.2\n[ERROR] \n",
			run.BaselineCauseDependencyUnavailable, "org.apache.maven.surefire:surefire-junit4:jar:2.22.2"},
		{"gradle", "> Could not resolve org.junit:junit-bom:5.10.0.\n", run.BaselineCauseDependencyUnavailable, "org.junit:junit-bom:5.10.0"},
		{"nuget", "error NU1101: Unable to find package Newtonsoft.Json. No packages exist\n", run.BaselineCauseDependencyUnavailable, "Newtonsoft.Json"},
		{"bundler", "Could not find gem 'rake' in locally installed gems.\n", run.BaselineCauseDependencyUnavailable, "rake"},
		{"pnpm blocked registry with EACCES", "2026-10-22T14:01:03Z ERR_PNPM_META_FETCH_FAIL GET https://jsr.io/@std/assert/meta.json: request to https://jsr.io/@std/assert/meta.json failed, reason: connect EACCES 10.0.0.1:443 - Local (0.0.0.0:0)\n", run.BaselineCauseDependencyUnavailable, "jsr.io"},
		{"a blocked word in test output is not a dependency failure", "--- FAIL: TestGoodRequestBlocked (0.01s)\n    request to https://example.test was blocked\n", "", ""},
		{"npm blocked registry with EACCES", "npm error code EACCES\nnpm error network request to https://registry.npmjs.org/left-pad failed, reason: connect EACCES 104.16.0.1:443\n", run.BaselineCauseDependencyUnavailable, "registry.npmjs.org"},
		{"a test's own refused connection is not a dependency failure", "FAIL api.test.js\n  GET https://api.example.test/users: Error: connect EACCES 10.0.0.2:443\n", "", ""},
		{"local npm permission error is not a dependency failure", "npm error code EACCES\nnpm error syscall mkdir\nnpm error path /workspace/node_modules\n", "", ""},
		{"msbuild project", "Time Elapsed 00:00:03.72\nMSBUILD : error MSB1009: Project file does not exist.\nSwitch: tests/ImageGallery.Model.Tests/ImageGallery.Model.Tests.csproj\n",
			run.BaselineCauseMissingPath, "tests/ImageGallery.Model.Tests/ImageGallery.Model.Tests.csproj"},
		{"pytest path", "ERROR: file or directory not found: tests/test_x.py\n", run.BaselineCauseMissingPath, "tests/test_x.py"},
		{"ruby require", "<internal:/usr/lib/ruby/vendor_ruby/rubygems/core_ext/kernel_require.rb>:85:in `require': cannot load such file -- test_helper (LoadError)\n", run.BaselineCauseMissingPath, "test_helper"},
		{"no such file", "cat: docs/x.txt: No such file or directory\n", run.BaselineCauseMissingPath, "docs/x.txt"},
		{"first in log order wins", "sh: 1: mvn: not found\nNo module named pytest\n", run.BaselineCauseMissingProgram, "mvn"},
		{"cause without detail", "Could not resolve dependencies for project a:b:jar:1\n", run.BaselineCauseDependencyUnavailable, ""},
		{"nothing recognisable", "make: *** [Makefile:3: test] Error 1\nexit status 1\n", "", ""},
		{"a program's own not found", "GET /api/users/7: 404: not found\nError: not found\n", "", ""},
		{"another tool's Missing", "Missing: required env DATABASE_URL\ncoverage: Missing: 12 lines\n", "", ""},
		{"absolute shell path", "/bin/sh: 1: pytest: not found\n", run.BaselineCauseMissingProgram, "pytest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause, detail := Cause(tc.log)
			if cause != tc.cause || detail != tc.detail {
				t.Fatalf("Cause = %q, %q; want %q, %q", cause, detail, tc.cause, tc.detail)
			}
		})
	}
}

func TestCauseDetailIsBoundedAndClean(t *testing.T) {
	_, detail := Cause("No module named " + strings.Repeat("a", 500) + "\n")
	if len(detail) != maxCauseDetailLen {
		t.Fatalf("detail length = %d, want %d", len(detail), maxCauseDetailLen)
	}
	_, detail = Cause("No module named py\x1b[31mt\x00e\x07st\";rm\n")
	if detail == "" || strings.ContainsAny(detail, "\x1b\x00\x07\"; ") {
		t.Fatalf("detail %q keeps characters outside path and identifier characters", detail)
	}
}

func TestEvaluateRecordsACauseOnlyForAFailureNamingNoTest(t *testing.T) {
	b := Evaluate("pytest", 1, "/usr/local/bin/python: No module named pytest\n", "No module named pytest", "ticket", nil)
	if b.Cause != run.BaselineCauseMissingModule || b.CauseDetail != "pytest" {
		t.Fatalf("cause = %q, %q", b.Cause, b.CauseDetail)
	}
	want := `failed: exit 1; the sandbox image lacks the Python module "pytest"`
	if got := b.Summary(); got != want {
		t.Fatalf("Summary = %q, want %q", got, want)
	}
	// A failing test named by the log keeps today's behaviour.
	b = Evaluate("pytest", 1, "FAILED tests/test_a.py::test_x - assert 1\nNo module named foo\n", "", "ticket", nil)
	if b.Cause != "" || b.CauseDetail != "" {
		t.Fatalf("failing test log has cause %q, %q", b.Cause, b.CauseDetail)
	}
	// Nothing recognisable: today's sentence.
	b = Evaluate("make test", 1, "boom\n", "boom", "ticket", nil)
	if b.Cause != "" || b.Summary() != `failed: exit 1; first error in log: "boom"` {
		t.Fatalf("unrecognised log: cause %q, summary %q", b.Cause, b.Summary())
	}
	// A path the ticket creates stays the ticket's own work.
	b = Evaluate("go test ./...", 1, "ERROR: file or directory not found: tests/\n", "ERROR: file or directory not found: tests/", "ticket", []string{"tests"})
	if b.Cause != "" || !b.Expected {
		t.Fatalf("created path: cause %q, expected %v", b.Cause, b.Expected)
	}
}
