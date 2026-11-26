package main

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"

	"buildgate/internal/notify"
	"buildgate/internal/sanitize"
	"buildgate/internal/sessionconfig"
)

// webhookFiles are the notification webhook URL files of the session config
// this process loaded (slack_webhook_urls_file, discord_webhook_urls_file);
// "" sends nothing to that channel.
type webhookFiles struct {
	Slack, Discord string
}

// webhookFilesOf is s's webhook URL files.
func webhookFilesOf(s sessionconfig.Settings) webhookFiles {
	return webhookFiles{Slack: s.SlackWebhookURLsFile, Discord: s.DiscordWebhookURLsFile}
}

// webhookSettings holds the webhookFiles of the session config the process
// loaded, set once at its start (useWebhookFiles) and read by every
// notification.
type webhookSettings struct {
	files atomic.Pointer[webhookFiles]
}

// retiredWebhookVariables are the environment variables that once held the
// webhook URLs, each with the setting that replaced it. They are detected so
// doctor and the worker can say they are no longer read; nothing reads them.
var retiredWebhookVariables = []struct{ variable, setting string }{
	{"FACTORYD_SLACK_WEBHOOK_URLS", "slack_webhook_urls_file"},
	{"FACTORYD_DISCORD_WEBHOOK_URLS", "discord_webhook_urls_file"},
}

// useWebhookFiles records s's webhook files as the ones this process's
// notifications read: worker, serve and a run call it at their start, with
// the settings of the session config they loaded (-config included). The
// first call of a process wins, so a build the worker or serve runs
// in-process keeps theirs. That first call logs one line for each problem
// webhookChecks finds, so a notification that would be dropped is visible
// when the process starts.
func (dp *deps) useWebhookFiles(s sessionconfig.Settings, getenv func(string) string) {
	files := webhookFilesOf(s)
	if !dp.webhooks.files.CompareAndSwap(nil, &files) {
		return
	}
	for _, c := range webhookChecks(files, getenv) {
		if c.Err != nil {
			log.Printf("%s: %s", c.Name, sanitize.Line(c.Err.Error()))
		}
	}
}

// NotifyWebhooks are the webhook URLs of the session config this process
// loaded (useWebhookFiles), read from their files at each call; none before
// useWebhookFiles.
func (dp *deps) NotifyWebhooks() notify.Webhooks {
	files := dp.webhooks.files.Load()
	if files == nil {
		return notify.Webhooks{}
	}
	return dp.host.notifyWebhooks(*files)
}

// readWebhookFiles is the real host.notifyWebhooks: the URLs in files' two
// files, each read through readWebhookFile. A file that cannot be used is
// logged in one line naming the file and the fix, never its content, and
// sends nothing; notification is best effort.
func readWebhookFiles(files webhookFiles) notify.Webhooks {
	read := func(setting, path string) []string {
		if path == "" {
			return nil
		}
		urls, err := readWebhookFile(path)
		if err != nil {
			log.Printf("%s: %s", setting, sanitize.Line(err.Error()))
			return nil
		}
		return urls
	}
	return notify.Webhooks{
		Slack:   read("slack_webhook_urls_file", files.Slack),
		Discord: read("discord_webhook_urls_file", files.Discord),
	}
}

// readWebhookFile is the URLs in the webhook URL file at path (one per line
// or comma-separated), read through readSecretFile. A URL with a control
// character in it is refused, with the whole file.
func readWebhookFile(path string) ([]string, error) {
	content, err := readSecretFile(path)
	if err != nil {
		return nil, err
	}
	urls := notify.ParseWebhookURLs(content)
	for _, u := range urls {
		if hasControlCharacter(u) {
			return nil, fmt.Errorf("secret file %s holds a URL with a control character: write one URL per line and nothing else", path)
		}
	}
	return urls, nil
}

// webhookChecks are doctor's notification rows, also logged by a worker or
// serve at start: one per configured webhook URL file, failing with the fix
// when it cannot be read (missing, a symlink, another owner, a mode looser
// than 0600, empty), and one failing row per retired webhook variable set
// in getenv's environment.
func webhookChecks(files webhookFiles, getenv func(string) string) []doctorCheck {
	var checks []doctorCheck
	for _, f := range []struct{ setting, path string }{
		{"slack_webhook_urls_file", files.Slack},
		{"discord_webhook_urls_file", files.Discord},
	} {
		if f.path == "" {
			continue
		}
		check := doctorCheck{Name: "notifications: " + f.setting}
		if urls, err := readWebhookFile(f.path); err != nil {
			check.Err = err
			check.Fix = fmt.Sprintf("write the webhook URLs, one per line, to %s as a regular file you own with mode 0600 (chmod 600 %s)", f.path, f.path)
		} else {
			check.Detail = fmt.Sprintf("%d URL(s) in %s", len(urls), f.path)
		}
		checks = append(checks, check)
	}
	for _, r := range retiredWebhookVariables {
		if strings.TrimSpace(getenv(r.variable)) == "" {
			continue
		}
		checks = append(checks, doctorCheck{
			Name: "notifications: " + r.variable,
			Err:  fmt.Errorf("%s is set and no longer read: put the URLs in a file named by %s", r.variable, r.setting),
			Fix:  fmt.Sprintf("write the URLs, one per line, to a file of mode 0600, name it in the session config's %s, and unset %s", r.setting, r.variable),
		})
	}
	return checks
}

// notifyWebhooks is the real host.notifyWebhooks.
func (impl realHost) notifyWebhooks(files webhookFiles) notify.Webhooks {
	return readWebhookFiles(files)
}
