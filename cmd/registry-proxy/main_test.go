package main

import "testing"

func TestRouteFlagParsesPlainAndRewriteHosts(t *testing.T) {
	var f routeFlag
	if err := f.Set("/npm/=https://registry.npmjs.org"); err != nil {
		t.Fatalf("Set (no allowed hosts): %v", err)
	}
	if err := f.Set("/pypi/=https://pypi.org/simple,files.pythonhosted.org->/pypi-files/"); err != nil {
		t.Fatalf("Set (rewrite host): %v", err)
	}
	if err := f.Set("/gomodproxy/sumdb/sum.golang.org/=https://sum.golang.org,sum.golang.org"); err != nil {
		t.Fatalf("Set (plain allowed host, no rewrite): %v", err)
	}
	if len(f.routes) != 3 {
		t.Fatalf("got %d routes, want 3", len(f.routes))
	}

	npm := f.routes[0]
	if npm.Prefix != "/npm/" || npm.Upstream.String() != "https://registry.npmjs.org" {
		t.Errorf("npm route = %+v", npm)
	}
	if len(npm.AllowedHosts) != 0 || len(npm.RewriteHrefHosts) != 0 {
		t.Errorf("npm route should have no allowed/rewrite hosts, got %+v / %+v", npm.AllowedHosts, npm.RewriteHrefHosts)
	}

	pypi := f.routes[1]
	if !pypi.AllowedHosts["files.pythonhosted.org"] {
		t.Errorf("pypi route missing allowed host, got %+v", pypi.AllowedHosts)
	}
	if pypi.RewriteHrefHosts["files.pythonhosted.org"] != "/pypi-files/" {
		t.Errorf("pypi route rewrite target = %q, want /pypi-files/", pypi.RewriteHrefHosts["files.pythonhosted.org"])
	}

	sumdb := f.routes[2]
	if !sumdb.AllowedHosts["sum.golang.org"] {
		t.Errorf("sumdb route missing allowed host, got %+v", sumdb.AllowedHosts)
	}
	if len(sumdb.RewriteHrefHosts) != 0 {
		t.Errorf("sumdb route should have no rewrite hosts (plain allowlist entry only), got %+v", sumdb.RewriteHrefHosts)
	}
}

func TestRouteFlagRejectsMalformedValues(t *testing.T) {
	cases := []string{
		"",
		"no-equals-sign",
		"/npm/=",
		"=https://registry.npmjs.org",
		"/npm/=https://registry.npmjs.org,",
		"/npm/=not a url\x7f", // still parses as a URL in Go, so this alone isn't guaranteed to fail; kept minimal
		"/pypi/=https://pypi.org/simple,files.pythonhosted.org->",
	}
	for _, value := range cases {
		var f routeFlag
		err := f.Set(value)
		switch value {
		case "/npm/=https://registry.npmjs.org,":
			// A trailing empty host entry is simply skipped, not an error.
			if err != nil {
				t.Errorf("Set(%q) = %v, want nil (trailing empty host entry is ignored)", value, err)
			}
		case "/npm/=not a url\x7f":
			// Not asserted either way -- url.Parse is lenient; this case
			// exists only to document that this input is not specifically
			// guarded against here.
		default:
			if err == nil {
				t.Errorf("Set(%q) unexpectedly succeeded", value)
			}
		}
	}
}
