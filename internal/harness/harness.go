// Package harness extracts the embedded Python scripts factoryd shells
// out to (build_app.py, goal_pilot.py, ticket_runner.py) to a cache
// directory on disk, so a single installed binary needs no
// buildgate checkout to find them at run time.
package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"buildgate/agent/pi"
)

const scriptsDir = "scripts"

type embeddedFile struct {
	name string
	data []byte
}

// Ensure extracts the embedded harness scripts to
// $XDG_CACHE_HOME/factoryd/harness/<content-hash>/, falling back to
// ~/.factory/harness/<content-hash>/ when XDG_CACHE_HOME is unset, and
// returns that directory. The directory is named by a hash of the
// embedded scripts' own content, not by factoryd's build version, so
// concurrent factoryd processes -- possibly from different builds --
// racing to extract always agree on the same target path.
//
// Extraction is skipped when the directory already contains every
// embedded file with a matching sha256 (the common case: cheap on every
// factoryd invocation once cached). Otherwise the scripts are written to
// a temp directory and published via publish -- see that function's own
// doc comment for how it stays safe against a second process racing to
// publish the same content-addressed directory concurrently.
func Ensure() (string, error) {
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	entries, err := fs.ReadDir(pi.Scripts, scriptsDir)
	if err != nil {
		return "", fmt.Errorf("read embedded harness scripts: %w", err)
	}
	hash, err := contentHash(entries)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "harness", hash)

	if upToDate(dir, entries) {
		return dir, nil
	}
	if err := publish(dir, entries); err != nil {
		return "", err
	}
	return dir, nil
}

func cacheRoot() (string, error) {
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		return filepath.Join(xdg, "factoryd"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for harness cache: %w", err)
	}
	return filepath.Join(home, ".factory"), nil
}

// contentHash returns a hex sha256 over every embedded script's
// (filename, content), sorted by filename so the result doesn't depend
// on directory iteration order. This is what names the cache directory
// Ensure extracts into.
func contentHash(entries []fs.DirEntry) (string, error) {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)

	h := sha256.New()
	for _, name := range names {
		data, err := pi.Scripts.ReadFile(scriptsDir + "/" + name)
		if err != nil {
			return "", fmt.Errorf("read embedded %s: %w", name, err)
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// upToDate reports whether dir already holds every entry with content
// matching the embedded copy.
func upToDate(dir string, entries []fs.DirEntry) bool {
	for _, e := range entries {
		want, err := embeddedSHA256(e.Name())
		if err != nil {
			return false
		}
		got, err := fileSHA256(filepath.Join(dir, e.Name()))
		if err != nil || got != want {
			return false
		}
	}
	return true
}

func embeddedSHA256(name string) (string, error) {
	data, err := pi.Scripts.ReadFile(scriptsDir + "/" + name)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sha256Hex(data), nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// publish extracts entries into dir via a temp-directory-then-rename
// sequence, and never removes an existing dir -- dir's name is a content
// hash, so an already-populated dir can only legitimately hold the exact
// content this call would also write.
//
// This makes it safe against a second factoryd process racing to
// publish the same directory: if this call's rename loses the race (dir
// already exists, so the rename fails), an existing dir that already
// matches the embedded content means the other process won cleanly --
// the temp directory is discarded and publish reports success. An
// existing dir that does NOT match is a corrupted or tampered cache
// sharing this content hash, which publish cannot safely repair by
// deleting (a legitimate winner's just-published directory would look
// identical to a race in progress) -- it returns an actionable error
// naming the path instead.
func publish(dir string, entries []fs.DirEntry) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create harness cache directory: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".tmp-harness-*")
	if err != nil {
		return fmt.Errorf("create temp harness directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	for _, e := range entries {
		data, err := pi.Scripts.ReadFile(scriptsDir + "/" + e.Name())
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", e.Name(), err)
		}
	}

	if err := os.Rename(tmp, dir); err != nil {
		if upToDate(dir, entries) {
			return nil
		}
		if _, statErr := os.Stat(dir); statErr == nil {
			return fmt.Errorf("harness cache directory %s exists but its content does not match its own content-hash name -- delete it and retry: %w", dir, err)
		}
		return fmt.Errorf("rename harness directory into place: %w", err)
	}
	return nil
}
