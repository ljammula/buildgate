package openshell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/NVIDIA/OpenShell/sdk/go/openshell/v1"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/meter"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
)

// liveRuntime connects to this machine's real gateway. The live tests run
// only with OPENSHELL_LIVE=1 and OPENSHELL_LIVE_IMAGE set to a digest-pinned
// worker image whose WORKDIR is /sandbox (`make sandbox-image` prints one),
// after `factoryd doctor -fix` has started the stack. An OpenShell upgrade
// repeats them.
func liveRuntime(t *testing.T) (*Runtime, string) {
	t.Helper()
	image := os.Getenv("OPENSHELL_LIVE_IMAGE")
	if os.Getenv("OPENSHELL_LIVE") != "1" || image == "" {
		t.Skip("set OPENSHELL_LIVE=1 and OPENSHELL_LIVE_IMAGE to run against the real gateway")
	}
	stack, err := hostcontrol.OpenShellStackPath()
	if err != nil {
		t.Fatal(err)
	}
	client, err := Connect("127.0.0.1:8080", hostcontrol.OpenShellMTLSDir(stack))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	readiness, err := ConnectReadiness("127.0.0.1:8080", hostcontrol.OpenShellMTLSDir(stack))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readiness.Close() })
	return &Runtime{Client: client, Containers: &DockerContainers{}, Readiness: readiness, PollEvery: 500 * time.Millisecond}, image
}

// liveLaunch builds one launch's directories under the data root (the only
// host directory the VM shares read-write) and its spec.
func liveLaunch(t *testing.T, image, script string) (sandbox.LaunchSpec, sandbox.SandboxLaunch) {
	t.Helper()
	root := filepath.Join(sessionconfig.DataRoot(), "openshell-live", fmt.Sprintf("%d", time.Now().UnixNano()))
	dirs := map[string]string{}
	for _, name := range []string{"workspace", "inputs", "data", "guard", "output", "scratch"} {
		dirs[name] = filepath.Join(root, name)
		if err := os.MkdirAll(dirs[name], 0o775); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if out, err := exec.Command("git", "-C", dirs["workspace"], "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dirs["inputs"], "input.txt"), []byte("an input\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runID := "live-" + filepath.Base(root)
	name, err := sandbox.SandboxName(dirs["data"], runID, "a1")
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.LaunchSpec{
		Image: image, WorkDir: dirs["workspace"], InputDir: dirs["inputs"], ScratchDir: dirs["scratch"],
		LogPath: filepath.Join(root, "worker.log"), Name: name, User: fmt.Sprintf("65532:%d", os.Getgid()),
		Command: []string{"/bin/sh", "-c", script},
		Memory:  "1g", CPUs: "1", TmpfsSize: "64m", Timeout: 3 * time.Minute,
		Network: "none", RunID: runID, DataDir: dirs["data"],
	}
	return spec, sandbox.SandboxLaunch{Name: name, GuardDir: dirs["guard"], OutputDir: dirs["output"]}
}

func waitForFile(t *testing.T, path string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %s", path, within)
}

const liveProbeScript = `echo "uid=$(id -u)"; echo "cwd=$(pwd)"; echo "home=$HOME"
echo hello > /workspace/out.txt && echo wrote-workspace
(echo x > /workspace/.git/evil) 2>/dev/null && echo GIT-WRITABLE || echo git-readonly
(touch /guard/x) 2>/dev/null && echo GUARD-WRITABLE || echo guard-readonly
(rm /guard/started) 2>/dev/null && echo STARTED-REMOVABLE || echo started-kept
echo "input=$(cat /inputs/input.txt)"
echo s > /scratch/s && echo wrote-scratch
echo t > /tmp/t && echo wrote-tmp
echo h > /home/worker/h && echo wrote-home
(echo x > /etc/evil) 2>/dev/null && echo ETC-WRITABLE || echo etc-denied
(echo x > /sandbox/evil) 2>/dev/null && echo SANDBOX-WRITABLE || echo sandbox-denied
echo "tmpfs=$(df -k /tmp | tail -1 | awk '{print $2}')"
echo "nofile=$(ulimit -n) core=$(ulimit -c)"
printf 'all:\n\t@echo make-ran-a-recipe\n' > /tmp/Makefile.probe; make -f /tmp/Makefile.probe 2>&1
grep -E ' (/tmp|/home/worker|/workspace|/workspace/.git|/guard|/inputs|/scratch|/worker-log) ' /proc/mounts | awk '{print "mount " $2 " " $3 " " $4}'
cp /bin/true /tmp/t-exec 2>/dev/null; /tmp/t-exec 2>/dev/null && echo TMP-EXEC || echo tmp-noexec
cp /bin/true /home/worker/h-exec && /home/worker/h-exec && echo home-exec
python3 -c 'import urllib.request,sys
try:
    urllib.request.urlopen("https://example.com", timeout=5); print("NETWORK-OPEN")
except Exception as e:
    print("network-denied")'
exit 5`

// TestLiveSandboxRunsTheGuardedCommand launches a worker with no model
// route and checks the mounts, the filesystem policy, the guard, the output
// file, the worker's recorded start, the exit code and the removal.
func TestLiveSandboxRunsTheGuardedCommand(t *testing.T) {
	rt, image := liveRuntime(t)
	spec, launch := liveLaunch(t, image, liveProbeScript)
	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if err := sandbox.RecordSandbox(spec.DataDir, spec.RunID, sandbox.SandboxRecord{Name: launch.Name}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.Delete(context.Background(), launch.Name); err != nil {
			t.Errorf("cleanup delete: %v", err)
		}
	})
	ref, err := rt.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Logf("created %+v", ref)
	if _, err := rt.Client.SandboxTemplates().Get(ctx, DefaultWorkspace, launch.Name); !v1.IsNotFound(err) {
		t.Errorf("the launch's workload template is still held by the gateway after Create: %v", err)
	}
	if ref.ID == "" {
		t.Error("Create returned no sandbox id; the meter's ledger file is named by it")
	}
	state, err := rt.Status(ctx, launch.Name)
	if err != nil || !state.Present || state.StartedAt == "" {
		t.Fatalf("Status = %+v, %v; want present with a start time", state, err)
	}
	t.Logf("worker started at %s", state.StartedAt)
	ids, _ := exec.Command("docker", "ps", "-q", "--filter", "label="+labelSandboxName+"="+launch.Name).Output()
	limits, _ := exec.Command("docker", append([]string{"inspect", "--format",
		`{{index .Config.Labels "` + labelRole + `"}} memory={{.HostConfig.Memory}} swap={{.HostConfig.MemorySwap}} nanocpus={{.HostConfig.NanoCpus}} pids={{.HostConfig.PidsLimit}}`},
		strings.Fields(string(ids))...)...).Output()
	t.Logf("container limits:\n%s", limits)
	if !strings.Contains(string(limits), " memory=1073741824 ") || !strings.Contains(string(limits), " nanocpus=1000000000 ") {
		t.Errorf("the worker container lacks the 1g memory and 1 CPU limits")
	}
	if !strings.Contains(string(limits), " pids=1024") {
		t.Errorf("no container of the sandbox has the gateway configuration's process limit (sandbox_pids_limit, 1024)")
	}

	outputFile := filepath.Join(launch.OutputDir, sandbox.WorkerOutputFile)
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(outputFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the command produced output before it was released: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launch.GuardDir, sandbox.WorkerGuardGoFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, outputFile, 30*time.Second)
	if err := os.WriteFile(filepath.Join(launch.GuardDir, sandbox.WorkerGuardStartedFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	exit, err := rt.Wait(ctx, launch.Name)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	logged, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("exit %+v, output:\n%s", exit, logged)
	if exit.ExitCode != 5 {
		t.Errorf("exit code = %d, want 5", exit.ExitCode)
	}
	for _, want := range []string{
		"uid=65532", "cwd=/workspace", "home=/home/worker", "wrote-workspace", "git-readonly", "guard-readonly", "started-kept",
		"input=an input", "wrote-scratch", "wrote-tmp", "wrote-home", "etc-denied", "sandbox-denied",
		"tmpfs=65536", "nofile=4096 core=0", "network-denied", "tmp-noexec", "home-exec", "make-ran-a-recipe",
		"mount /tmp tmpfs rw,nosuid,nodev,noexec,relatime,size=65536k,inode64",
		"mount /workspace/.git virtiofs ro,relatime", "mount /guard virtiofs ro,relatime", "mount /inputs virtiofs ro,relatime",
	} {
		if !strings.Contains(string(logged), want+"\n") {
			t.Errorf("output lacks %q", want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(spec.WorkDir, "out.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("the worker's file on the host: %q, %v", b, err)
	}
	after, err := rt.Status(ctx, launch.Name)
	t.Logf("status after exit: %+v, %v", after, err)
	if after.StartedAt != "" && after.StartedAt != state.StartedAt {
		t.Errorf("the worker's start moved from %s to %s with no restart", state.StartedAt, after.StartedAt)
	}
	names, err := rt.ListByRun(ctx, spec.DataDir, spec.RunID)
	t.Logf("ListByRun after exit: %v, %v", names, err)
	if err := rt.Delete(ctx, launch.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gone, err := rt.Status(ctx, launch.Name); err != nil || gone.Present {
		t.Errorf("after Delete: %+v, %v; want absent", gone, err)
	}
}

// TestLiveGatewayRestartDoesNotRunTheCommandTwice restarts the gateway under
// a running sandbox. The gateway starts every sandbox's command again; the
// guard must stop that second start before it touches the worktree, and the
// worker's recorded start must move so the factory can tell. It restarts the
// machine's gateway: run it only when no build is using it.
func TestLiveGatewayRestartDoesNotRunTheCommandTwice(t *testing.T) {
	rt, image := liveRuntime(t)
	if os.Getenv("OPENSHELL_LIVE_RESTART") != "1" {
		t.Skip("set OPENSHELL_LIVE_RESTART=1 to restart the gateway")
	}
	spec, launch := liveLaunch(t, image, `echo ran >> /workspace/runs.txt; sleep 120`)
	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		if err := rt.Delete(context.Background(), launch.Name); err != nil {
			t.Errorf("cleanup delete: %v", err)
		}
	})
	if _, err := rt.Create(ctx, req); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := rt.Status(ctx, launch.Name)
	if err != nil || first.StartedAt == "" {
		t.Fatalf("Status = %+v, %v", first, err)
	}
	if err := os.WriteFile(filepath.Join(launch.GuardDir, sandbox.WorkerGuardGoFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, filepath.Join(launch.OutputDir, sandbox.WorkerOutputFile), 30*time.Second)
	if err := os.WriteFile(filepath.Join(launch.GuardDir, sandbox.WorkerGuardStartedFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runs := filepath.Join(spec.WorkDir, "runs.txt")
	waitForFile(t, runs, 30*time.Second)

	if out, err := exec.Command("docker", "restart", "buildgate-openshell-gateway-1").CombinedOutput(); err != nil {
		t.Fatalf("restart the gateway: %v: %s", err, out)
	}
	var starts []string
	seen := map[string]bool{first.StartedAt: true}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		state, err := rt.Status(ctx, launch.Name)
		if err == nil && state.StartedAt != "" && !seen[state.StartedAt] {
			seen[state.StartedAt] = true
			starts = append(starts, state.StartedAt)
			t.Logf("worker started again at %s (first %s)", state.StartedAt, first.StartedAt)
		}
		time.Sleep(time.Second)
	}
	if len(starts) == 0 {
		t.Error("the worker's start time did not move after the gateway restart: the factory could not detect the rerun")
	}
	t.Logf("starts after the restart: %d", len(starts))
	ran, err := os.ReadFile(runs)
	if err != nil {
		t.Fatal(err)
	}
	if string(ran) != "ran\n" {
		t.Errorf("the command ran %d times, want once: %q", strings.Count(string(ran), "ran"), ran)
	}
	final, err := rt.Client.Sandboxes().Get(ctx, DefaultWorkspace, launch.Name)
	if err != nil {
		t.Fatalf("read the sandbox after the restart: %v", err)
	}
	exit := "none"
	if final.Status.ExitCode != nil {
		exit = fmt.Sprint(*final.Status.ExitCode)
	}
	t.Logf("after the restart: phase %s, exit code %s", final.Status.Phase, exit)
}

const liveModelScript = `import json, os, urllib.request, urllib.error
tok = os.environ.get("BG_CHATGPT_TOKEN", ""); acct = os.environ.get("BG_CHATGPT_ACCOUNT", "")
print("token-is-jwt", tok.count(".") == 2, "token-is-placeholder", tok.startswith("openshell:"))
base = os.environ["FACTORY_MODEL_BASE_URL"]
headers = {"Authorization": "Bearer " + tok, "chatgpt-account-id": acct, "Content-Type": "application/json",
           "Accept": "text/event-stream", "OpenAI-Beta": "responses=experimental", "originator": "codex_cli_rs"}
def call(label, url, data):
    try:
        r = urllib.request.urlopen(urllib.request.Request(url, data=data, headers=headers), timeout=120)
        body = r.read().decode("utf-8", "replace")
        print(label, "HTTP", r.status, "completed", "response.completed" in body)
    except urllib.error.HTTPError as e:
        print(label, "HTTP", e.code, e.read()[:200].decode("utf-8", "replace").replace("\n", " "))
    except Exception as e:
        print(label, "ERR", type(e).__name__, str(e)[:200])
body = {"model": os.environ["FACTORY_MODEL_ID"], "instructions": "You are terse.", "store": False, "stream": True,
        "input": [{"role": "user", "content": [{"type": "input_text", "text": "Reply with the single word OK."}]}]}
call("responses", base + "/responses", json.dumps(body).encode())
call("other-path", "https://chatgpt.com/backend-api/plugins/export/curated", None)
call("other-host", "https://example.com/", None)
`

// TestLiveModelRouteThroughTheMeter pushes this machine's ChatGPT credential
// to the route's provider, launches a worker allowed one endpoint, and checks
// that the worker holds only placeholders, that its one model call succeeds
// and is in the meter's ledger under the sandbox id, and that another path on
// the same host and another host are refused. It spends one short request.
func TestLiveModelRouteThroughTheMeter(t *testing.T) {
	rt, image := liveRuntime(t)
	model := os.Getenv("OPENSHELL_LIVE_CHATGPT_MODEL")
	if model == "" {
		t.Skip("set OPENSHELL_LIVE_CHATGPT_MODEL to a ChatGPT Codex model id to spend one request")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	authFile, err := os.ReadFile(filepath.Join(home, ".codex", "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var auth struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(authFile, &auth); err != nil || auth.Tokens.AccessToken == "" {
		t.Fatalf("read the ChatGPT login: %v", err)
	}

	spec, launch := liveLaunch(t, image, `python3 /inputs/call.py`)
	if err := os.WriteFile(filepath.Join(spec.InputDir, "call.py"), []byte(liveModelScript), 0o644); err != nil {
		t.Fatal(err)
	}
	policy := sandbox.RoutePolicy{
		Route:    "chatgpt",
		Upstream: meter.ChatGPTCodexAPIBase, AllowedPathPrefix: meter.ChatGPTCodexResponsesPath,
		AuthMode: meter.CredentialModeChatGPTCodex, UsageFormat: meter.UsageFormatOpenAIResponses,
		WorkerModelID: model, WorkerModelAPI: meter.RequestFormatOpenAIResponses,
		MaxRequestBytes: 1 << 18, RequestsPerMinute: 30,
		TokenBudget: 200000, TokenBudgetWindow: time.Minute, CostBudgetMicroUSD: 5000000, CostBudgetWindow: time.Minute,
		TokenCeiling: 200000, CostCeilingMicroUSD: 5000000,
		InputMicroUSDPerMTok: 1000000, CachedInputMicroUSDPerMTok: 100000, CacheWriteMicroUSDPerMTok: 1000000, OutputMicroUSDPerMTok: 8000000,
	}
	access, err := policy.RouteAccess(spec.DataDir, []string{"/usr/local/bin/python3*"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cred, err := policy.RouteCredential(access, sandbox.RouteSecret{}, sandbox.RouteSecret{}, sandbox.NewRouteSecret(auth.Tokens.AccessToken), sandbox.NewRouteSecret(auth.Tokens.AccountID), time.Time{}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		if err := rt.Delete(context.Background(), launch.Name); err != nil {
			t.Errorf("cleanup delete: %v", err)
		}
		if _, err := rt.Client.Providers().Delete(context.Background(), DefaultWorkspace, access.Provider); err != nil {
			t.Errorf("cleanup: delete provider %s: %v", access.Provider, err)
		}
	})
	if err := rt.PushCredential(ctx, cred); err != nil {
		t.Fatalf("PushCredential: %v", err)
	}
	if err := rt.PushCredential(ctx, cred); err != nil {
		t.Fatalf("a second PushCredential (the update path): %v", err)
	}
	spec.Environment = access.Environment
	req, err := spec.SandboxRequest(launch)
	if err != nil {
		t.Fatal(err)
	}
	req.Route = &access
	if req.MeterConfig, err = policy.MeterConfig(spec.DataDir, spec.RunID, launch.Name); err != nil {
		t.Fatal(err)
	}
	ref, err := rt.Create(ctx, req)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(launch.GuardDir, sandbox.WorkerGuardGoFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	exit, err := rt.Wait(ctx, launch.Name)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	logged, _ := os.ReadFile(filepath.Join(launch.OutputDir, sandbox.WorkerOutputFile))
	t.Logf("exit %+v, output:\n%s", exit, logged)
	for _, want := range []string{"token-is-jwt False token-is-placeholder True", "responses HTTP 200 completed True"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("output lacks %q", want)
		}
	}
	for _, refused := range []string{"other-path HTTP 200", "other-host HTTP 200"} {
		if strings.Contains(string(logged), refused) {
			t.Errorf("the worker reached what its policy does not admit: %q", refused)
		}
	}
	if strings.Contains(string(logged), auth.Tokens.AccessToken) {
		t.Error("the worker's output contains the real access token")
	}
	ledgerRoot := filepath.Join(sessionconfig.DataRoot(), "meter-ledgers")
	usage, err := sandbox.SandboxMeterUsage(ledgerRoot, spec.DataDir, spec.RunID, ref.ID)
	t.Logf("meter usage for sandbox id %s: %+v, %v", ref.ID, usage, err)
	if err != nil || usage.Tokens() == 0 {
		t.Errorf("the meter's ledger records no usage for the sandbox: %+v, %v", usage, err)
	}
	if err := usage.CeilingErr(); err != nil {
		t.Errorf("CeilingErr() = %v", err)
	}
}

// TestLiveRunThroughRuntime runs the whole launch sequence the factory uses
// (sandbox.RunThroughRuntime) against the real gateway.
func TestLiveRunThroughRuntime(t *testing.T) {
	rt, image := liveRuntime(t)
	spec, _ := liveLaunch(t, image, `echo first; echo second >&2; echo hello > /workspace/out.txt; exit 4`)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, ref, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "attempt-1"})
	if err != nil {
		t.Fatalf("RunThroughRuntime: %v", err)
	}
	t.Logf("result %+v, ref %+v", result, ref)
	if result.ExitCode != 4 || ref.ID == "" {
		t.Errorf("exit code = %d, sandbox id %q; want 4 and an id", result.ExitCode, ref.ID)
	}
	if logged, err := os.ReadFile(spec.LogPath); err != nil || string(logged) != "first\nsecond\n" {
		t.Errorf("log = %q, %v", logged, err)
	}
	if b, err := os.ReadFile(filepath.Join(spec.WorkDir, "out.txt")); err != nil || string(b) != "hello\n" {
		t.Errorf("the worker's file on the host: %q, %v", b, err)
	}
	if state, err := rt.Status(ctx, ref.Name); err != nil || state.Present {
		t.Errorf("after the run: %+v, %v; want the sandbox gone", state, err)
	}
	if names, err := rt.ListByRun(ctx, spec.DataDir, spec.RunID); err != nil || len(names) != 0 {
		t.Errorf("ListByRun after the run = %v, %v; want none", names, err)
	}
}

// TestLiveWorkerReachesASidecarByAddress starts a container on an internal
// Docker network (as the registry proxy and compose services sit on), gives
// the worker its address and port as a sidecar, and checks the worker
// reaches it over HTTP and raw TCP, and reaches neither another port there
// nor another container on the same network.
func TestLiveWorkerReachesASidecarByAddress(t *testing.T) {
	rt, image := liveRuntime(t)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	network, sidecar, other := "bg-live-net-"+suffix, "bg-live-sidecar-"+suffix, "bg-live-other-"+suffix
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	docker("network", "create", "--internal", network)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", sidecar, other).Run()
		_ = exec.Command("docker", "network", "rm", network).Run()
	})
	serve := `import http.server, socketserver, threading
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(b"sidecar says hello " + self.path.encode())
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
for port in (8092, 9999):
    threading.Thread(target=socketserver.TCPServer(("0.0.0.0", port), H).serve_forever, daemon=True).start()
threading.Event().wait()`
	for _, name := range []string{sidecar, other} {
		docker("run", "-d", "--name", name, "--network", network, "--entrypoint", "python3", image, "-c", serve)
	}
	address := func(name string) string {
		return docker("inspect", "-f", `{{(index .NetworkSettings.Networks "`+network+`").IPAddress}}`, name)
	}
	sidecarIP, otherIP := address(sidecar), address(other)
	t.Logf("sidecar %s, other %s", sidecarIP, otherIP)

	script := fmt.Sprintf(`python3 - <<'PY'
import socket, urllib.request
def get(label, url):
    try:
        print(label, urllib.request.urlopen(url, timeout=8).read().decode())
    except Exception as e:
        print(label, "REFUSED", type(e).__name__)
def tcp(label, ip, port):
    try:
        s = socket.create_connection((ip, port), timeout=8); s.sendall(b"GET /raw HTTP/1.0\r\n\r\n"); print(label, s.recv(200).decode().splitlines()[-1]); s.close()
    except Exception as e:
        print(label, "REFUSED", type(e).__name__)
get("http", "http://%[1]s:8092/npm/@scope%%2Fpkg")
tcp("tcp", "%[1]s", 8092)
get("other-port", "http://%[1]s:9999/")
get("other-container", "http://%[2]s:8092/")
import os
print("proxy-env", sorted(k for k in os.environ if "proxy" in k.lower()))
PY`, sidecarIP, otherIP)
	spec, _ := liveLaunch(t, image, script)
	spec.Sidecars = []sandbox.SidecarEndpoint{{Name: "sidecar", IP: sidecarIP, Ports: []int{8092}}}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	_, _, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "attempt-1"})
	if err != nil {
		t.Fatalf("RunThroughRuntime: %v", err)
	}
	logged, _ := os.ReadFile(spec.LogPath)
	t.Logf("output:\n%s", logged)
	for _, want := range []string{"http sidecar says hello /npm/@scope%2Fpkg", "tcp sidecar says hello /raw", "other-port REFUSED", "other-container REFUSED"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("output lacks %q", want)
		}
	}
}

// TestLiveWorkerFetchesPackagesThroughTheRegistryProxy starts the real
// registry proxy for a run and launches a worker that is given it by
// address. The worker must download a Go module through it; npm and pip are
// reported. Needs OPENSHELL_LIVE_REGISTRY_PROXY_IMAGE (a digest-pinned
// registry proxy image) and reaches the public registries.
func TestLiveWorkerFetchesPackagesThroughTheRegistryProxy(t *testing.T) {
	rt, image := liveRuntime(t)
	proxyImage := os.Getenv("OPENSHELL_LIVE_REGISTRY_PROXY_IMAGE")
	if proxyImage == "" {
		t.Skip("set OPENSHELL_LIVE_REGISTRY_PROXY_IMAGE to start the registry proxy")
	}
	spec, _ := liveLaunch(t, image, `echo "GOPROXY=$GOPROXY"
mkdir -p /scratch/m && cd /scratch/m && go mod init example.com/m >/dev/null 2>&1
GONOSUMDB=golang.org go get golang.org/x/text@v0.14.0 2>&1 | tail -2; test -d /scratch/gomodcache/golang.org/x/text@v0.14.0 && echo go-ok
python3 -c 'import os,urllib.request
for path in ("/sumdb/sum.golang.org/supported", "/sumdb/sum.golang.org/lookup/golang.org/x/text@v0.14.0"):
    try:
        r = urllib.request.urlopen(os.environ["GOPROXY"] + path, timeout=20); print("sumdb", path, r.status)
    except Exception as e:
        print("sumdb", path, "ERR", getattr(e, "code", ""), str(e)[:120])'
npm view left-pad version 2>&1 | tail -1 | sed 's/^/npm: /'
pip download --no-deps -d /tmp/p idna==3.6 2>&1 | tail -4 | sed 's/^/pip: /'; ls /tmp/p 2>/dev/null | sed 's/^/pip-file: /'`)
	policy := sandbox.RegistryProxyPolicy{
		Image: proxyImage, Routes: sandbox.DefaultRegistryProxyRoutes(),
		CacheBytes: 1 << 30, MaxObjectBytes: 256 << 20, MaxConcurrentUpstream: 16, UpstreamTimeout: time.Minute,
	}
	proxy, err := sandbox.BeginRegistryProxyLifecycleFor(rt, policy.Spec(spec.RunID, spec.DataDir), "docker", spec.RunID, spec.DataDir, sandbox.RegistryProxyHooks{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := proxy.Cleanup(); err != nil {
			t.Errorf("cleanup registry proxy: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := proxy.Ensure(ctx, ""); err != nil {
		t.Fatalf("start the registry proxy: %v", err)
	}
	spec.ScratchDir = ""
	if spec, err = proxy.PrepareWorker(spec); err != nil {
		t.Fatal(err)
	}
	if spec, err = sandbox.PrepareWorkerScratch(spec); err != nil {
		t.Fatal(err)
	}
	t.Logf("sidecars %+v", spec.Sidecars)
	if _, _, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "attempt-1"}); err != nil {
		t.Fatalf("RunThroughRuntime: %v", err)
	}
	logged, _ := os.ReadFile(spec.LogPath)
	t.Logf("output:\n%s", logged)
	// The Go download skips the checksum database: the proxy does not
	// answer Go's "supported" probe for it, on either launcher, so a module
	// with no go.sum entry cannot be verified from a worker.
	for want, what := range map[string]string{"go-ok": "download a Go module", "npm: 1.3.0": "query npm", "pip-file: idna-3.6": "download a pip package"} {
		if !strings.Contains(string(logged), want) {
			t.Errorf("the worker did not %s through the registry proxy", what)
		}
	}
}

// TestLiveScript runs the shell script in OPENSHELL_LIVE_SCRIPT in a worker
// launched through the gateway and logs its output: a probe for finding out
// what a worker can and cannot do there.
func TestLiveScript(t *testing.T) {
	rt, image := liveRuntime(t)
	script := os.Getenv("OPENSHELL_LIVE_SCRIPT")
	if script == "" {
		t.Skip("set OPENSHELL_LIVE_SCRIPT to a shell script to run in a worker")
	}
	spec, _ := liveLaunch(t, image, script)
	// OPENSHELL_LIVE_INPUT names a host file to put at /inputs/<its name>.
	if input := os.Getenv("OPENSHELL_LIVE_INPUT"); input != "" {
		content, err := os.ReadFile(input)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(spec.InputDir, filepath.Base(input)), content, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, _, err := sandbox.RunThroughRuntime(ctx, rt, spec, sandbox.RuntimeLaunch{Nonce: "attempt-1"})
	logged, _ := os.ReadFile(spec.LogPath)
	t.Logf("exit %d, err %v, output:\n%s", result.ExitCode, err, logged)
}
