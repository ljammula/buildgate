package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/notify"
	"buildgate/internal/request"
)

// TestReadSecretFileRefusesAnotherOwner proves a secret file owned by a uid
// other than the expected one is refused with the fix, and the same file is
// read for its own owner.
func TestReadSecretFileRefusesAnotherOwner(t *testing.T) {
	path := writeKeyFile(t, "sk-owned", 0o600)
	_, err := readSecretFileOwnedBy(path, os.Getuid()+1)
	want := "secret file " + path + " is not owned by this user: copy it to a file you own with mode 0600"
	if err == nil || err.Error() != want {
		t.Fatalf("readSecretFileOwnedBy(another uid) = %v, want %q", err, want)
	}
	if got, err := readSecretFileOwnedBy(path, os.Getuid()); err != nil || got != "sk-owned" {
		t.Fatalf("readSecretFileOwnedBy(own uid) = %q, %v; want sk-owned", got, err)
	}
}

// TestSecretValuesRefuseControlCharacters proves a key with a control
// character after trimming (a second line, a tab) is refused when read and
// when written, and a webhook URL file with one in a URL is refused whole.
func TestSecretValuesRefuseControlCharacters(t *testing.T) {
	for _, content := range []string{"sk-one\nsk-two", "sk-a\tb", "sk-\x00"} {
		path := writeKeyFile(t, content, 0o600)
		_, err := readSecretValue(path)
		want := "secret file " + path + " holds a control character (a second line?): it must hold the value alone on one line"
		if err == nil || err.Error() != want {
			t.Errorf("readSecretValue(%q) = %v, want %q", content, err, want)
		}
		target := filepath.Join(t.TempDir(), "key")
		if err := writeSecretFile(target, content); err == nil {
			t.Errorf("writeSecretFile(%q) = nil, want a refusal", content)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Errorf("writeSecretFile(%q) left a file (stat: %v)", content, err)
		}
	}
	if got, err := readSecretValue(writeKeyFile(t, "  sk-one\n", 0o600)); err != nil || got != "sk-one" {
		t.Fatalf("readSecretValue of a trimmed key = %q, %v; want sk-one", got, err)
	}
	hooks := writeKeyFile(t, "https://hooks.example/a\nhttps://hooks.example/b\tc\n", 0o600)
	if _, err := readWebhookFile(hooks); err == nil || !strings.Contains(err.Error(), "a URL with a control character") {
		t.Fatalf("readWebhookFile = %v, want the URL with a tab refused", err)
	}
}

// TestWriteSecretFileRefusesASymlink proves the key writer never writes
// through a symlink: the link and its target are left as they were.
func TestWriteSecretFileRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := writeSecretFile(link, "sk-x")
	want := "secret file " + link + " is a symlink: refusing to write through it; remove it or name another file"
	if err == nil || err.Error() != want {
		t.Fatalf("writeSecretFile(symlink) = %v, want %q", err, want)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("the write went through the symlink (stat: %v)", err)
	}
}

// TestReadWebhookFilesReadsTheConfiguredFiles proves the webhook URLs come
// from the files named, one per line or comma-separated, and a file readable
// beyond its owner sends nothing, logged in one line that holds no URL.
func TestReadWebhookFilesReadsTheConfiguredFiles(t *testing.T) {
	slack := writeKeyFile(t, "https://hooks.slack.com/a\nhttps://hooks.slack.com/b, https://hooks.slack.com/c\n", 0o600)
	discord := writeKeyFile(t, "https://discord.example/x", 0o644)
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	hooks := readWebhookFiles(webhookFiles{Slack: slack, Discord: discord})
	if got := strings.Join(hooks.Slack, " "); got != "https://hooks.slack.com/a https://hooks.slack.com/b https://hooks.slack.com/c" {
		t.Errorf("Slack = %q, want the three URLs of the file", got)
	}
	if len(hooks.Discord) != 0 {
		t.Errorf("Discord = %v, want none from a file of mode 0644", hooks.Discord)
	}
	if !strings.Contains(logged.String(), "discord_webhook_urls_file: secret file "+discord+" has mode 0644, readable beyond its owner: chmod 600 "+discord) {
		t.Errorf("log = %q, want the discord file and its fix", logged.String())
	}
	if strings.Contains(logged.String(), "discord.example") {
		t.Errorf("log %q holds the URL", logged.String())
	}
}

// TestWebhookChecksNameEveryProblem proves doctor's notification rows: a
// readable file passes with its URL count, a file of mode 0644 and a symlink
// fail with the fix, and each retired variable that is set fails, saying it
// is no longer read.
func TestWebhookChecksNameEveryProblem(t *testing.T) {
	good := writeKeyFile(t, "https://hooks.slack.com/a\nhttps://hooks.slack.com/b", 0o600)
	loose := writeKeyFile(t, "https://discord.example/x", 0o644)
	checks := webhookChecks(webhookFiles{Slack: good, Discord: loose}, func(string) string { return "" })
	if len(checks) != 2 {
		t.Fatalf("checks = %+v, want two rows", checks)
	}
	if c := checks[0]; c.Name != "notifications: slack_webhook_urls_file" || c.Err != nil || c.Detail != "2 URL(s) in "+good {
		t.Errorf("slack row = %+v, want ok with 2 URL(s)", c)
	}
	if c := checks[1]; c.Name != "notifications: discord_webhook_urls_file" || c.Err == nil || c.Err.Error() != "secret file "+loose+" has mode 0644, readable beyond its owner: chmod 600 "+loose ||
		c.Fix != "write the webhook URLs, one per line, to "+loose+" as a regular file you own with mode 0600 (chmod 600 "+loose+")" {
		t.Errorf("discord row = %+v, want the mode refusal and its fix", c)
	}

	link := filepath.Join(t.TempDir(), "hooks")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FACTORYD_SLACK_WEBHOOK_URLS": "https://hooks.slack.com/old", "FACTORYD_DISCORD_WEBHOOK_URLS": "https://discord.example/old"}
	checks = webhookChecks(webhookFiles{Slack: link}, func(k string) string { return env[k] })
	if len(checks) != 3 {
		t.Fatalf("checks = %+v, want three rows", checks)
	}
	if c := checks[0]; c.Err == nil || c.Err.Error() != "secret file "+link+" is a symlink: name the file itself, a regular file of mode 0600" {
		t.Errorf("symlink row = %+v, want the symlink refusal", c)
	}
	for i, want := range []string{
		"FACTORYD_SLACK_WEBHOOK_URLS is set and no longer read: put the URLs in a file named by slack_webhook_urls_file",
		"FACTORYD_DISCORD_WEBHOOK_URLS is set and no longer read: put the URLs in a file named by discord_webhook_urls_file",
	} {
		if c := checks[i+1]; c.Err == nil || c.Err.Error() != want || strings.Contains(c.Err.Error()+c.Fix, "/old") {
			t.Errorf("retired variable row = %+v, want %q and no URL", c, want)
		}
	}
}

// TestWorkerUsesTheWebhooksOfItsOwnConfig proves a worker started with
// -config naming a config that is not the active profile posts its
// notifications to the webhooks that config names, never the active
// profile's, and logs at start a webhook file it cannot read and a retired
// variable that is set.
func TestWorkerUsesTheWebhooksOfItsOwnConfig(t *testing.T) {
	dp := newTestDeps(t)
	active := isolateSessionConfig(t)
	activeHooks := writeKeyFile(t, "https://hooks.slack.com/active", 0o600)
	writeSessionConfig(t, active, "sandbox_image: img@sha256:aaaa\nslack_webhook_urls_file: "+activeHooks+"\n")
	ownHooks := writeKeyFile(t, "https://hooks.slack.com/own", 0o600)
	looseHooks := writeKeyFile(t, "https://discord.example/own", 0o644)
	own := filepath.Join(t.TempDir(), "side.yml")
	writeSessionConfig(t, own, "sandbox_image: img@sha256:aaaa\nslack_webhook_urls_file: "+ownHooks+"\ndiscord_webhook_urls_file: "+looseHooks+"\n")
	t.Setenv("FACTORYD_SLACK_WEBHOOK_URLS", "https://hooks.slack.com/old")
	t.Setenv("FACTORYD_DISCORD_WEBHOOK_URLS", "")
	var seen webhookFiles
	fakeHostOf(dp).notifyWebhooksFn = func(files webhookFiles) notify.Webhooks {
		seen = files
		return readWebhookFiles(files)
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	if _, _, err := loadTestWorkerConfig(dp, []string{"-config", own, "-data-dir", t.TempDir(), "-skip-doctor"}); err != nil {
		t.Fatalf("loadWorkerConfig: %v", err)
	}
	hooks := dp.NotifyWebhooks()
	if seen != (webhookFiles{Slack: ownHooks, Discord: looseHooks}) {
		t.Fatalf("webhook files = %+v, want the -config's", seen)
	}
	if strings.Join(hooks.Slack, " ") != "https://hooks.slack.com/own" || len(hooks.Discord) != 0 {
		t.Fatalf("webhooks = %+v, want only the -config's Slack URL", hooks)
	}
	for _, want := range []string{
		"notifications: discord_webhook_urls_file: secret file " + looseHooks + " has mode 0644, readable beyond its owner: chmod 600 " + looseHooks,
		"notifications: FACTORYD_SLACK_WEBHOOK_URLS: FACTORYD_SLACK_WEBHOOK_URLS is set and no longer read: put the URLs in a file named by slack_webhook_urls_file",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("start log %q, want the line %q", logged.String(), want)
		}
	}
	if strings.Contains(logged.String(), "hooks.slack.com/old") {
		t.Errorf("start log %q holds the retired variable's URL", logged.String())
	}
}

// TestWriteSecretFileNeverReplacesAnotherValue proves the key writer, at the
// moment it puts the file in place, keeps a file holding the same value,
// refuses one holding another (left as it was), and that only the explicit
// replace writer replaces it.
func TestWriteSecretFileNeverReplacesAnotherValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creds", "key")
	if err := writeSecretFile(path, "sk-first"); err != nil {
		t.Fatal(err)
	}
	if err := writeSecretFile(path, " sk-first\n"); err != nil {
		t.Fatalf("writeSecretFile(same value) = %v, want nil", err)
	}
	err := writeSecretFile(path, "sk-second")
	want := "secret file " + path + " already holds another value: refusing to replace it; remove it or name another file"
	if err == nil || err.Error() != want {
		t.Fatalf("writeSecretFile(another value) = %v, want %q", err, want)
	}
	if got, _ := readSecretValue(path); got != "sk-first" {
		t.Fatalf("key file = %q after the refusal, want sk-first", got)
	}
	if err := replaceSecretFile(path, "sk-second"); err != nil {
		t.Fatalf("replaceSecretFile: %v", err)
	}
	if got, _ := readSecretValue(path); got != "sk-second" {
		t.Fatalf("key file = %q after replaceSecretFile, want sk-second", got)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the key file (no temporary left)", len(entries))
	}
}

// TestApproveAndServeUseTheWebhooksOfTheirOwnConfig proves a CLI decision
// (approve, reject) and serve take their notification webhook files from
// the -config they loaded, not the active profile: an approve refused by an
// expired gate posts its notification to that config's webhooks.
func TestApproveAndServeUseTheWebhooksOfTheirOwnConfig(t *testing.T) {
	active := isolateSessionConfig(t)
	activeHooks := writeKeyFile(t, "https://hooks.slack.com/active", 0o600)
	writeSessionConfig(t, active, "slack_webhook_urls_file: "+activeHooks+"\n")
	ownHooks := writeKeyFile(t, "https://hooks.slack.com/own", 0o600)
	own := filepath.Join(t.TempDir(), "side.yml")
	writeSessionConfig(t, own, "review_gate_expiry: 2h\nslack_webhook_urls_file: "+ownHooks+"\n")

	for _, decide := range []func(dp *deps, dataDir string) error{
		func(dp *deps, dataDir string) error {
			return approveMain(dp, []string{"-config", own, "-data-dir", dataDir, "req-1"})
		},
		func(dp *deps, dataDir string) error {
			return rejectMain(dp, []string{"-config", own, "-data-dir", dataDir, "-reason", "too late", "req-1"})
		},
	} {
		dp := newTestDeps(t)
		var seen []webhookFiles
		fakeHostOf(dp).notifyWebhooksFn = func(files webhookFiles) notify.Webhooks {
			seen = append(seen, files)
			return notify.Webhooks{}
		}
		dataDir := t.TempDir()
		reviewWaitingSince(t, dataDir, request.StateSpecReview, 3*time.Hour)
		if err := decide(dp, dataDir); err == nil || !strings.Contains(err.Error(), "expired") {
			t.Fatalf("decision after expiry = %v, want the expiry refusal", err)
		}
		if len(seen) != 1 || seen[0] != (webhookFiles{Slack: ownHooks}) {
			t.Fatalf("expiry notification read webhook files %+v, want once the -config's %s", seen, ownHooks)
		}
	}

	dp := newTestDeps(t)
	sv := &serveRun{dp: dp, args: []string{"-config", own, "-data-dir", t.TempDir()}}
	if err := sv.loadConfig(); err != nil {
		t.Fatalf("serve loadConfig: %v", err)
	}
	if files := dp.webhooks.files.Load(); files == nil || *files != (webhookFiles{Slack: ownHooks}) {
		t.Fatalf("serve's webhook files = %+v, want the -config's %s", files, ownHooks)
	}
}
