package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/sandbox/sandboxtest"
	"buildgate/internal/sessionconfig"
)

// TestRouteKeyReachesOnlyTheGatewayPush proves a static route's key, read
// from its credential_file, goes to one place: the credential pushed to the
// gateway's route provider. It is in none of the session config, the
// sandbox request (its environment, command, meter configuration), the
// files the sandbox mounts (the staged prompt input and the workspace)
// while it runs, the data directory afterwards (request and run records,
// logs, evidence), and this process's log.
func TestRouteKeyReachesOnlyTheGatewayPush(t *testing.T) {
	const key = "sk-canary-route-key-5f1c2a"
	keyFile := filepath.Join(t.TempDir(), "litellm")
	if err := writeSecretFile(keyFile, key); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.yml")
	config := "routes:\n  litellm:\n    upstream: https://litellm.example.invalid\n    credential_file: " + keyFile + "\n" +
		"models:\n  luna:\n    id: gpt-5.6-luna\n    routes: [litellm]\n" +
		"roles:\n  execution:\n    model: luna\n    thinking: medium\n"
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := sessionconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := cfg.ApplySettings(sessionconfig.DefaultSettings())
	if err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	dataDir, r, workerCfg, rt := readBackJobFixture(t, 0)
	workerCfg.Settings = settings
	inSandbox := scanMountsAtLaunch(rt, key)
	launched := writeReadBackAfterLaunch(rt, readBackInputFileName,
		`{"summary": "ok", "questions": []}`,
		`{"schema_version": 1, "usage": null, "agent_exit_code": 0, "duration_s": 1, "agents_md_used": false}`)
	if _, _, err := runReadBackJob(context.Background(), dataDir, r, requestdrivertest.CanonicalValidSpec, workerCfg); err != nil {
		t.Fatalf("runReadBackJob: %v", err)
	}
	if seen := <-launched; seen.err != nil {
		t.Fatal(seen.err)
	}

	pushed := false
	for _, cred := range rt.Pushed() {
		for _, value := range cred.Secrets() {
			pushed = pushed || value == key
		}
	}
	if !pushed {
		t.Fatalf("the key was not pushed to the gateway: %v", rt.Pushed())
	}
	for _, found := range <-inSandbox {
		t.Errorf("the key is in %s, which the sandbox mounts", found)
	}
	requests := rt.Requests()
	encoded, _ := json.Marshal(requests)
	if strings.Contains(string(encoded), key) || strings.Contains(fmt.Sprintf("%+v", requests), key) {
		t.Error("the key is in the sandbox request (environment, command, mounts or meter configuration)")
	}
	if data, _ := os.ReadFile(configPath); strings.Contains(string(data), key) {
		t.Error("the key is in the session config")
	}
	for _, found := range filesContaining(t, dataDir, key) {
		t.Errorf("the key is in %s under the data directory", found)
	}
	if strings.Contains(logged.String(), key) {
		t.Error("the key is in the log")
	}
}

// scanMountsAtLaunch waits for rt's first launch and reports every file under
// its mount sources that holds needle, read while the sandbox is live.
func scanMountsAtLaunch(rt *sandboxtest.WorkerRuntime, needle string) <-chan []string {
	done := make(chan []string, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			requests := rt.Requests()
			if len(requests) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			var found []string
			for _, mount := range requests[0].Mounts {
				found = append(found, scanTree(mount.Source, needle)...)
			}
			done <- found
			return
		}
		done <- []string{"(no launch within 5s)"}
	}()
	return done
}

// filesContaining is scanTree from root, failing t when root cannot be read.
func filesContaining(t *testing.T, root, needle string) []string {
	t.Helper()
	if _, err := os.Stat(root); err != nil {
		t.Fatal(err)
	}
	return scanTree(root, needle)
}

// scanTree lists the regular files under root (root itself when it is a
// file) whose content holds needle.
func scanTree(root, needle string) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil && bytes.Contains(data, []byte(needle)) {
			found = append(found, path)
		}
		return nil
	})
	return found
}
