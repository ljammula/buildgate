package openshell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/sandbox"
)

// factoryDirRepo makes workspace a repository whose one commit holds
// .factory/x.sh with the committed text, leaves other bytes in the worktree's
// copy, and returns the commit.
func factoryDirRepo(t *testing.T, workspace string) string {
	t.Helper()
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", workspace, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if _, err := os.Lstat(filepath.Join(workspace, ".git")); err != nil {
		git("init", "-q")
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".factory"), 0o775); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(workspace, ".factory", "x.sh")
	if err := os.WriteFile(script, []byte("echo committed\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	if err := os.WriteFile(script, []byte("echo worktree\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	return git("rev-parse", "HEAD")
}

// A launch that is not a review's (no model route, a repository command)
// with the `.factory/` mask: the gateway's sandbox spec binds the snapshot
// directory read-only at /workspace/.factory, after the workspace itself.
func TestSandboxSpecBindsTheFactoryDirSnapshotReadOnlyOverTheWorkspace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for _, name := range []string{"workspace", "data", "guard", "output"} {
		dirs[name] = filepath.Join(root, name)
		if err := os.Mkdir(dirs[name], 0o750); err != nil {
			t.Fatal(err)
		}
	}
	commit := factoryDirRepo(t, dirs["workspace"])
	mount, err := sandbox.PrepareCommitDirMount(context.Background(), dirs["workspace"], commit, ".factory", filepath.Join(root, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mount.Remove() })

	launch := sandbox.LaunchSpec{
		Image: "worker@sha256:" + strings.Repeat("a", 64), WorkDir: dirs["workspace"],
		LogPath: filepath.Join(root, "gate.log"), Name: "bg-0123456789abcdef", User: fmt.Sprintf("65532:%d", os.Getgid()),
		Command: []string{"/bin/sh", "-c", "sh .factory/x.sh"},
		Memory:  "1g", CPUs: "1", TmpfsSize: "64m", Timeout: time.Minute,
		Network: "none", RunID: "run1", DataDir: dirs["data"],
		WorkspaceMasks: mount.Masks(),
	}
	req, err := launch.SandboxRequest(sandbox.SandboxLaunch{Name: launch.Name, GuardDir: dirs["guard"], OutputDir: dirs["output"]})
	if err != nil {
		t.Fatalf("SandboxRequest: %v", err)
	}
	if req.Route != nil {
		t.Fatalf("the request has a model route: %+v", req.Route)
	}
	spec, err := sandboxSpec(req)
	if err != nil {
		t.Fatal(err)
	}
	mounts, _ := spec.Template.DriverConfig["docker"].(map[string]any)["mounts"].([]any)
	workspaceAt, factoryAt := -1, -1
	for i, raw := range mounts {
		m := raw.(map[string]any)
		switch m["target"] {
		case "/workspace":
			workspaceAt = i
			if m["read_only"] != false {
				t.Errorf("the workspace bind is read-only: %+v", m)
			}
		case "/workspace/.factory":
			factoryAt = i
			if m["type"] != "bind" || m["read_only"] != true || m["source"] != mount.Mask.Source {
				t.Errorf("the .factory mount = %+v, want a read-only bind of %s", m, mount.Mask.Source)
			}
			text, err := os.ReadFile(filepath.Join(m["source"].(string), "x.sh"))
			if err != nil || string(text) != "echo committed\n" {
				t.Errorf("the bound x.sh = %q (%v), want the commit's bytes", text, err)
			}
		}
	}
	if workspaceAt < 0 || factoryAt < workspaceAt {
		t.Errorf("mounts = %+v: want /workspace/.factory bound after /workspace", mounts)
	}
}
