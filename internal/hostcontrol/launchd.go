package hostcontrol

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// WorkerServiceLabel is both the plist's own Label key and, joined under
// gui/<uid>/, the launchd service identifier `launchctl bootstrap`/
// `bootout`/`print` address it by.
const WorkerServiceLabel = "dev.factoryd.worker"

// ServeServiceLabel is the LaunchAgent label for the serve half of
// `install-service`: a loopback `factoryd serve` an operator no longer needs a dedicated
// terminal for either, alongside dev.factoryd.worker.
const ServeServiceLabel = "dev.factoryd.serve"

// WorkerPlistPath returns where install-service writes the LaunchAgent
// plist: ~/Library/LaunchAgents/dev.factoryd.worker.plist.
func WorkerPlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", WorkerServiceLabel+".plist"), nil
}

// ServePlistPath returns where install-service writes the serve
// LaunchAgent plist: ~/Library/LaunchAgents/dev.factoryd.serve.plist.
func ServePlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", ServeServiceLabel+".plist"), nil
}

// UnescapeXML reverses escapeXML, for reading a value back out of a plist
// this package generated (doctorCheckWorkerService's stale-binary
// check). Delegates to encoding/xml's own character-data decoding rather
// than reimplementing entity handling.
func UnescapeXML(s string) string {
	var decoded struct {
		Value string `xml:",chardata"`
	}
	if err := xml.Unmarshal([]byte("<x>"+s+"</x>"), &decoded); err != nil {
		return s
	}
	return decoded.Value
}

// programArgumentsArray and programArgumentsStringElement together extract
// every <string> value inside the ProgramArguments <array> of a plist
// buildWorkerPlist produced, in order -- programArgumentsFirstString
// above only needs the first (the binary path); a caller that also needs
// to compare the service's own -config/-data-dir arguments against a
// particular invocation's own (factoryd quickstart, checking whether an
// already-running service is actually draining the queue this invocation
// cares about -- a Codex review on this PR) needs the rest too.
var programArgumentsArray = regexp.MustCompile(`(?s)<key>ProgramArguments</key>\s*<array>(.*?)</array>`)

var programArgumentsStringElement = regexp.MustCompile(`(?s)<string>(.*?)</string>`)

// ProgramArgumentsStrings returns every ProgramArguments <string> value,
// in order, or nil if plistBytes has no recognizable ProgramArguments
// array at all.
func ProgramArgumentsStrings(plistBytes []byte) []string {
	arr := programArgumentsArray.FindSubmatch(plistBytes)
	if arr == nil {
		return nil
	}
	matches := programArgumentsStringElement.FindAllSubmatch(arr[1], -1)
	args := make([]string, 0, len(matches))
	for _, m := range matches {
		args = append(args, UnescapeXML(string(m[1])))
	}
	return args
}

// ProgramArgumentsFlagValue returns the value immediately following flag
// inside args (e.g. "-config" -> its path), or "", false if flag isn't
// present or has nothing following it.
func ProgramArgumentsFlagValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}
