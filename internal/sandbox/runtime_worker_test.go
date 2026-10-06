package sandbox

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
)

// testJWT is an unsigned compact JWT whose only claim is exp.
func testJWT(exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + ".sig"
}

var workerTestNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func chatGPTRelaySpec(tokenExpires time.Time) *RouteSpec {
	return &RouteSpec{
		RoutePolicy:      chatGPTPolicy(),
		ChatGPTToken:     NewRouteSecret(testJWT(tokenExpires)),
		ChatGPTAccountID: NewRouteSecret("acct-secret"),
	}
}

func runtimeWorker(relaySpec *RouteSpec, ledgerRoot string) RuntimeWorker {
	return RuntimeWorker{Relay: relaySpec, ModelBinaries: piBinaries, MeterLedgerRoot: ledgerRoot, Now: func() time.Time { return workerTestNow }}
}

func TestRunWorkerThroughRuntimeGivesTheWorkerItsRouteAndCountsItsSpend(t *testing.T) {
	spec := runtimeSpec(t)
	spec.Name = "factoryd-temporal-worker-1"
	spec.Environment = []string{"FACTORY_MODEL_BASE_URL=http://stale.invalid:8091", "KEEP=1"}
	ledgerRoot := t.TempDir()
	// The fake gateway's id for the sandbox is "sandbox-id-1".
	writeMeterLedger(t, ledgerRoot, spec.DataDir, spec.RunID, "sandbox-id-1",
		admit("a", 5000, 1000, 900),
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7, ReasoningEffort: "high"},
		complete("a"))
	rt := &workerRuntime{lines: []string{"ok"}, startedAt: "t"}

	result, err := RunWorkerThroughRuntime(context.Background(), rt, spec, runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), ledgerRoot))
	if err != nil {
		t.Fatal(err)
	}
	name, _ := SandboxName(spec.DataDir, spec.RunID, spec.Name)
	provider, _ := ProviderName(spec.DataDir, "chatgpt")
	if rt.req.Name != name || rt.req.Route == nil || rt.req.Route.Provider != provider || rt.req.Route.Endpoint.Path != "/backend-api/codex/responses" {
		t.Errorf("request name %q, route %+v", rt.req.Name, rt.req.Route)
	}
	if rt.req.MeterConfig["sandbox"] != name || rt.req.MeterConfig["token_ceiling"] != int64(50000) {
		t.Errorf("meter config = %v", rt.req.MeterConfig)
	}
	env := strings.Join(rt.req.Environment, "\n")
	for _, want := range []string{"KEEP=1", "FACTORY_MODEL_BASE_URL=https://chatgpt.com/backend-api/codex", "FACTORY_MODEL_KEY_ENV=BG_CHATGPT_TOKEN"} {
		if !strings.Contains(env, want) {
			t.Errorf("worker environment lacks %q", want)
		}
	}
	if strings.Contains(env, "stale.invalid") {
		t.Error("the caller's own base URL survived")
	}
	if want := "push " + provider; rt.calls[0] != want {
		t.Errorf("first call = %q, want %q", rt.calls[0], want)
	}
	facts := result.RelayFacts
	if facts.Route != "chatgpt" || facts.CredentialMode != "chatgpt-codex" || facts.WorkerModelID != "gpt-5.6-luna" || facts.TokenCeiling != 50000 {
		t.Errorf("route facts = %+v", facts)
	}
	if tokens, cost, err := RunRelaySpend(spec.DataDir, spec.RunID); err != nil || tokens != 120 || cost != 7 {
		t.Errorf("the run's spend after the launch = %d, %d, %v; want 120, 7", tokens, cost, err)
	}
	if facts.ConsumedInputTokens != 100 || facts.ConsumedOutputTokens != 20 || facts.ConsumedCostMicroUSD != 7 || facts.ReasoningEffort != "high" ||
		facts.CeilingExceeded || facts.UsageReadFailed || facts.SpendPartial {
		t.Errorf("spend facts = %+v", facts)
	}
}

func TestRunWorkerThroughRuntimeEndsWithTheCeilingErrorOnARefusal(t *testing.T) {
	spec := runtimeSpec(t)
	ledgerRoot := t.TempDir()
	writeMeterLedger(t, ledgerRoot, spec.DataDir, spec.RunID, "sandbox-id-1", meter.LedgerRecord{Kind: meter.LedgerKindCeilingExceeded, RequestID: "c"})
	rt := &workerRuntime{lines: []string{"ok"}, startedAt: "t"}
	result, err := RunWorkerThroughRuntime(context.Background(), rt, spec, runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), ledgerRoot))
	if !errors.Is(err, ErrRelayCeilingExceeded) || !result.RelayFacts.CeilingExceeded {
		t.Fatalf("err = %v, facts %+v; want the ceiling error and the refusal recorded", err, result.RelayFacts)
	}
}

func TestRunWorkerThroughRuntimeChargesADroppedRequestWithoutFailingTheStep(t *testing.T) {
	spec := runtimeSpec(t)
	ledgerRoot := t.TempDir()
	writeMeterLedger(t, ledgerRoot, spec.DataDir, spec.RunID, "sandbox-id-1",
		meter.LedgerRecord{InputTokens: 100, OutputTokens: 20, CostMicroUSD: 7},
		admit("dropped", 5000, 1000, 900))
	rt := &workerRuntime{lines: []string{"ok"}, startedAt: "t"}
	result, err := RunWorkerThroughRuntime(context.Background(), rt, spec, runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), ledgerRoot))
	if err != nil {
		t.Fatalf("a request the client dropped failed the step: %v", err)
	}
	facts := result.RelayFacts
	// 100 + (5000-1000) input, 20 + 1000 output, 7 + 900 cost.
	if !facts.SpendPartial || facts.CeilingExceeded || facts.ConsumedInputTokens != 4100 || facts.ConsumedOutputTokens != 1020 || facts.ConsumedCostMicroUSD != 907 {
		t.Errorf("facts = %+v; want the estimate charged and the spend marked partial", facts)
	}
	if tokens, _, err := RunRelaySpend(spec.DataDir, spec.RunID); err != nil || tokens != 5120 {
		t.Errorf("the run's spend = %d, %v; want 5120, the estimate included", tokens, err)
	}
}

func TestRunWorkerThroughRuntimeFailsClosedOnALedgerItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	spec := runtimeSpec(t)
	ledgerRoot := t.TempDir()
	path := writeMeterLedger(t, ledgerRoot, spec.DataDir, spec.RunID, "sandbox-id-1", meter.LedgerRecord{InputTokens: 1})
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	rt := &workerRuntime{lines: []string{"ok"}, startedAt: "t"}
	result, err := RunWorkerThroughRuntime(context.Background(), rt, spec, runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), ledgerRoot))
	if !errors.Is(err, ErrRelayCeilingExceeded) || !result.RelayFacts.UsageReadFailed {
		t.Fatalf("err = %v, facts %+v; want the ceiling error and UsageReadFailed", err, result.RelayFacts)
	}
}

func TestRunWorkerThroughRuntimeRefusesBeforeAnyLaunch(t *testing.T) {
	checks := map[string]func(*RuntimeWorker){
		"a token that expires inside the step": func(w *RuntimeWorker) { w.Relay = chatGPTRelaySpec(workerTestNow.Add(10 * time.Second)) },
		"a token that is not a JWT":            func(w *RuntimeWorker) { w.Relay.ChatGPTToken = NewRouteSecret("opaque") },
		"no ledger root":                       func(w *RuntimeWorker) { w.MeterLedgerRoot = "" },
		"no harness executable":                func(w *RuntimeWorker) { w.ModelBinaries = nil },
		"a github-copilot route": func(w *RuntimeWorker) {
			w.Relay.AuthMode, w.Relay.Billing = "github-copilot", "subscription"
		},
	}
	for name, edit := range checks {
		t.Run(name, func(t *testing.T) {
			spec := runtimeSpec(t)
			w := runtimeWorker(chatGPTRelaySpec(workerTestNow.Add(24*time.Hour)), t.TempDir())
			edit(&w)
			rt := &workerRuntime{}
			if _, err := RunWorkerThroughRuntime(context.Background(), rt, spec, w); err == nil {
				t.Fatal("the launch was accepted")
			}
			if len(rt.calls) != 0 {
				t.Errorf("the runtime was called: %v", rt.calls)
			}
			if records, _ := RecordedSandboxes(spec.DataDir, spec.RunID); len(records) != 0 {
				t.Errorf("a sandbox was recorded: %v", records)
			}
		})
	}
}

func TestRunWorkerThroughRuntimeWithNoRoute(t *testing.T) {
	spec := runtimeSpec(t)
	rt := &workerRuntime{lines: []string{"verify ok"}, exitCode: 2, startedAt: "t"}
	result, err := RunWorkerThroughRuntime(context.Background(), rt, spec, RuntimeWorker{})
	if err != nil || result.ExitCode != 2 {
		t.Fatalf("result %+v, err %v", result, err)
	}
	if rt.req.Route != nil || rt.req.MeterConfig != nil || !reflect.DeepEqual(result.RelayFacts, RouteLaunchFacts{}) {
		t.Errorf("a step with no model got a route (%v), a meter config (%v) or facts (%+v)", rt.req.Route, rt.req.MeterConfig, result.RelayFacts)
	}
	if logged, _ := os.ReadFile(filepath.Clean(spec.LogPath)); string(logged) != "verify ok\n" {
		t.Errorf("log = %q", logged)
	}
}
