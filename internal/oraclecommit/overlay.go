package oraclecommit

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
)

// ValidateOverlayTarget checks that a manifest entry's RUN_COMMAND.txt is a
// `go test -overlay` command whose overlay Replace key is exactly
// targetPath, so the pinned copy that runs during the build and the copy
// committed at target_path provably test the same package. The overlay may
// be inline JSON or a file; for a file, readFile is called with the path as
// written in the command (nil readFile refuses file-based overlays).
//
// A command with no -overlay, an overlay with no Replace key equal to
// targetPath, or a Replace key that is absolute or otherwise not the
// clean relative target path is refused: nothing here guesses.
func ValidateOverlayTarget(command, targetPath string, readFile func(name string) ([]byte, error)) error {
	if err := ValidateTargetPath(targetPath, nil); err != nil {
		return fmt.Errorf("target_path: %w", err)
	}
	words, err := shellWords(stripLineContinuations(command))
	if err != nil {
		return err
	}
	var overlays []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "-overlay" || w == "--overlay":
			if i+1 >= len(words) {
				return errors.New("-overlay has no argument")
			}
			overlays = append(overlays, words[i+1])
			i++
		case strings.HasPrefix(w, "-overlay=") || strings.HasPrefix(w, "--overlay="):
			overlays = append(overlays, w[strings.Index(w, "=")+1:])
		}
	}
	if len(overlays) == 0 {
		return errors.New("RUN_COMMAND.txt has no -overlay; a committed oracle's command must overlay the pinned copy at target_path")
	}
	found := false
	for _, ov := range overlays {
		raw := []byte(ov)
		if !strings.HasPrefix(strings.TrimSpace(ov), "{") {
			if readFile == nil {
				return fmt.Errorf("-overlay %q names a file that cannot be read here; use inline JSON", ov)
			}
			raw, err = readFile(ov)
			if err != nil {
				return fmt.Errorf("read overlay file %q: %w", ov, err)
			}
		}
		var doc struct {
			Replace map[string]string `json:"Replace"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("parse -overlay JSON: %w", err)
		}
		for key := range doc.Replace {
			if strings.HasPrefix(key, "/") {
				return fmt.Errorf("overlay Replace key %q is absolute; it must be the relative target_path %q", key, targetPath)
			}
			if path.Clean(key) == targetPath {
				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("no overlay Replace key equals target_path %q", targetPath)
	}
	return nil
}

func stripLineContinuations(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\\\r\n", " "), "\\\n", " ")
}

// shellWords splits command into words honouring single quotes, double
// quotes and backslash escapes. It does not expand anything; a command that
// needs expansion to name its overlay simply fails the equality check.
func shellWords(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	flush := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in RUN_COMMAND.txt")
	}
	flush()
	return words, nil
}
