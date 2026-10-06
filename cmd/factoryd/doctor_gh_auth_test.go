package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeGHAuthRunner returns a ghAuthRunner scripted per test: keyed by the
// joined "name args..." command line, so a test can stub exactly the
// `git remote get-url origin` / `gh --version` / `gh auth status` calls
// doctorCheckGHAuth issues without a real git/gh binary anywhere.
func fakeGHAuthRunner(t *testing.T, scripted map[string]struct {
	out string
	err error
}) ghAuthRunner {
	t.Helper()
	return func(_ context.Context, name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		resp, ok := scripted[key]
		if !ok {
			t.Fatalf("unscripted command: %s", key)
		}
		return resp.out, resp.err
	}
}

func TestDoctorCheckGHAuthSkipsWithNoGitHubRemote(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin": {out: "", err: errors.New("no such remote")},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err != nil {
		t.Fatalf("expected no error (skipped check), got %v", check.Err)
	}
}

func TestDoctorCheckGHAuthFailsWhenGHNotInstalled(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin": {out: "https://github.com/acme/widgets.git\n", err: nil},
		"gh --version":                       {out: "", err: errors.New("exec: \"gh\": executable file not found in $PATH")},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err == nil {
		t.Fatal("expected an error for a missing gh binary")
	}
	if !strings.Contains(check.Err.Error(), "not installed") {
		t.Errorf("expected a not-installed message, got: %v", check.Err)
	}
}

func TestDoctorCheckGHAuthFailsWhenNotLoggedIn(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/fake/xdg")
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":   {out: "git@github.com:acme/widgets.git\n", err: nil},
		"gh --version":                         {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                       {out: "You are not logged into any GitHub hosts\n", err: errors.New("exit status 1")},
		"gh auth status --hostname github.com": {out: "You are not logged into any GitHub hosts\n", err: errors.New("exit status 1")},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err == nil {
		t.Fatal("expected an error for a failed gh auth status")
	}
	// Never surface gh's own output verbatim (it could carry a token in
	// some future gh version's own diagnostics) -- only exit status plus
	// the config dir this checked.
	if strings.Contains(check.Err.Error(), "not logged into any GitHub hosts") {
		t.Errorf("check error should not echo gh's own output, got: %v", check.Err)
	}
	if !strings.Contains(check.Err.Error(), "/fake/xdg/gh") {
		t.Errorf("expected the checked config dir /fake/xdg/gh named in the error, got: %v", check.Err)
	}
	if check.Fix != "gh auth login --hostname github.com" {
		t.Errorf("unexpected fix: %q", check.Fix)
	}
}

func TestDoctorCheckGHAuthPassesWhenLoggedIn(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":   {out: "https://github.com/acme/widgets\n", err: nil},
		"gh --version":                         {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                       {out: "github.com\n  Logged in to github.com as someone\n", err: nil},
		"gh auth status --hostname github.com": {out: "logged in to github.com as someone\n", err: nil},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err != nil {
		t.Fatalf("expected no error, got %v", check.Err)
	}
}

// TestDoctorCheckGHAuthSSHAliasForGitHubChecksGitHubCom is the regression
// test proving a `git@github.com-work:...` remote resolves to the literal
// host "github.com-work" (an SSH config alias, not a real DNS name) --
// doctorCheckGHAuth must still check github.com, the host gh actually
// authenticates against, not fail outright on an alias gh has never
// heard of.
func TestDoctorCheckGHAuthSSHAliasForGitHubChecksGitHubCom(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":   {out: "git@github.com-work:acme/widgets.git\n", err: nil},
		"gh --version":                         {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                       {out: "github.com\n  Logged in to github.com as someone\n", err: nil},
		"gh auth status --hostname github.com": {out: "logged in to github.com as someone\n", err: nil},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err != nil {
		t.Fatalf("expected no error for an SSH alias resolving to github.com, got %v", check.Err)
	}
}

// TestDoctorCheckGHAuthKnownNonGitHubHostIsChecked covers an operator who
// already ran `gh auth login --hostname` for a self-hosted GitHub
// Enterprise remote: ghHostToCheck must recognize it from `gh auth
// status`'s own (no -hostname) output and check it directly, not treat
// it as unknown.
func TestDoctorCheckGHAuthKnownNonGitHubHostIsChecked(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":        {out: "git@ghe.example.com:acme/widgets.git\n", err: nil},
		"gh --version":                              {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                            {out: "ghe.example.com\n  Logged in to ghe.example.com account someone\n", err: nil},
		"gh auth status --hostname ghe.example.com": {out: "logged in to ghe.example.com as someone\n", err: nil},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err != nil {
		t.Fatalf("expected no error for a host gh already knows, got %v", check.Err)
	}
}

// TestDoctorCheckGHAuthUnknownNonGitHubHostIsAdvisoryOnly is the core case
// for that same finding: a GitLab remote (or any host gh has never heard of)
// must never block `doctor`/`quickstart` -- gh has nothing to say about
// it. Reported as an advisory warning, not a failure.
func TestDoctorCheckGHAuthUnknownNonGitHubHostIsAdvisoryOnly(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin": {out: "git@gitlab.com:acme/widgets.git\n", err: nil},
		"gh --version":                       {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                     {out: "You are not logged into any GitHub hosts\n", err: errors.New("exit status 1")},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err == nil {
		t.Fatal("expected a warning naming the unknown host")
	}
	if !check.Advisory {
		t.Errorf("check.Advisory = false, want true: an unknown host must never block doctor/quickstart")
	}
}

// TestDoctorCheckGHAuthSSHWithPortForGitHubChecksGitHubCom covers an
// ssh:// remote naming a nonstandard port for github.com.
func TestDoctorCheckGHAuthSSHWithPortForGitHubChecksGitHubCom(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":   {out: "ssh://git@github.com:443/acme/widgets.git\n", err: nil},
		"gh --version":                         {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                       {out: "github.com\n  Logged in to github.com as someone\n", err: nil},
		"gh auth status --hostname github.com": {out: "logged in to github.com as someone\n", err: nil},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err != nil {
		t.Fatalf("expected no error for an ssh:// github.com remote with a port, got %v", check.Err)
	}
}

// TestDoctorCheckGHAuthGHEHostContainingGithubIsNotForcedToGitHubCom is
// the regression test proving a real GitHub Enterprise host like
// "github.acme.com" contains the substring "github" but is NOT
// github.com and is NOT an SSH alias for it (its remote here is plain
// https://, and even as an SSH remote its host does not start with
// "github.com") -- ghHostToCheck must never force it onto github.com. gh
// has no login for it here, so this must be advisory, not check (and
// pass/fail against) the wrong host.
func TestDoctorCheckGHAuthGHEHostContainingGithubIsNotForcedToGitHubCom(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin": {out: "https://github.acme.com/acme/widgets.git\n", err: nil},
		"gh --version":                       {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                     {out: "github.com\n  Logged in to github.com as someone\n", err: nil},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err == nil || !check.Advisory {
		t.Fatalf("check = %+v, want an advisory warning, not a github.com check for github.acme.com", check)
	}
	if check.Fix != "" && strings.Contains(check.Fix, "--hostname github.com") {
		t.Errorf("Fix = %q, must not suggest logging into github.com for a github.acme.com remote", check.Fix)
	}
}

// TestDoctorCheckGHAuthKnownGHEHostLoggedIn covers a real GHE host gh has
// a login for (ghKnownHosts finds it, checked first, exact match) --
// confirms that checking known hosts before falling back to github.com
// still passes the happy path.
func TestDoctorCheckGHAuthKnownGHEHostLoggedIn(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":        {out: "https://github.acme.com/acme/widgets.git\n", err: nil},
		"gh --version":                              {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                            {out: "github.acme.com\n  Logged in to github.acme.com as someone\n", err: nil},
		"gh auth status --hostname github.acme.com": {out: "logged in to github.acme.com as someone\n", err: nil},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err != nil {
		t.Fatalf("expected no error for a known, logged-in GHE host, got %v", check.Err)
	}
}

// TestDoctorCheckGHAuthKnownGHEHostNotLoggedIn covers the same known GHE
// host, but its own auth status check fails -- a real, blocking failure
// (not advisory), since gh does know about this host.
func TestDoctorCheckGHAuthKnownGHEHostNotLoggedIn(t *testing.T) {
	run := fakeGHAuthRunner(t, map[string]struct {
		out string
		err error
	}{
		"git -C /repo remote get-url origin":        {out: "https://github.acme.com/acme/widgets.git\n", err: nil},
		"gh --version":                              {out: "gh version 2.0.0\n", err: nil},
		"gh auth status":                            {out: "github.acme.com\n  Logged out of github.acme.com\n", err: errors.New("exit status 1")},
		"gh auth status --hostname github.acme.com": {out: "not logged in\n", err: errors.New("exit status 1")},
	})
	check := doctorCheckGHAuth(context.Background(), run, "/repo")
	if check.Err == nil {
		t.Fatal("expected an error for a known but not-logged-in GHE host")
	}
	if check.Advisory {
		t.Error("check.Advisory = true, want false: gh DOES know this host, so a login failure is a real, blocking problem")
	}
}

func TestParseGitRemoteHost(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/widgets.git":        "github.com",
		"https://github.com/acme/widgets.git":    "github.com",
		"https://github.com/acme/widgets":        "github.com",
		"ssh://git@github.com/acme/widgets.git":  "github.com",
		"ssh://git@ghe.example.com/acme/widgets": "ghe.example.com",
		"not a url at all":                       "",
	}
	for remote, want := range cases {
		if got := parseGitRemoteHost(remote); got != want {
			t.Errorf("parseGitRemoteHost(%q) = %q, want %q", remote, got, want)
		}
	}
}
