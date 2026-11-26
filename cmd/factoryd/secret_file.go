package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"
)

// A secret file is how a credential the session config refers to reaches the
// host process that uses it: a static route's key (routes.<name>.
// credential_file) and the notification webhook URLs (slack_webhook_urls_file,
// discord_webhook_urls_file). The config names the file, never the value, and
// the file is read each time the value is needed, so a key written or rotated
// later is used by the next launch with no restart and no shell environment.

// maxSecretFileBytes bounds what readSecretFile reads: a key or a few webhook
// URLs, never a file someone pointed the setting at by mistake.
const maxSecretFileBytes = 64 << 10

// readSecretFile is readSecretFileOwnedBy for this process's own user.
func readSecretFile(path string) (string, error) {
	return readSecretFileOwnedBy(path, os.Getuid())
}

// readSecretFileOwnedBy returns the trimmed content of the secret file at
// path (a leading ~/ is the home directory). It refuses, with one line naming
// the file and the fix and never the content, a path that is missing, a
// symlink or not a regular file, a file not owned by uid, one with any group
// or other permission bit (it must be mode 0600 or stricter), and an empty
// one. The file is opened without following a symlink and checked through
// the open descriptor, so what is checked is what is read.
func readSecretFileOwnedBy(path string, uid int) (string, error) {
	resolved, err := expandAuthPath(path)
	if err != nil {
		return "", fmt.Errorf("secret file %s: resolve the home directory: %w", path, err)
	}
	f, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return "", fmt.Errorf("secret file %s does not exist: write the value to it with mode 0600 (umask 077; printf '%%s' \"$KEY\" > %s)", path, path)
		case errors.Is(err, syscall.ELOOP):
			return "", fmt.Errorf("secret file %s is a symlink: name the file itself, a regular file of mode 0600", path)
		}
		return "", fmt.Errorf("secret file %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("secret file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("secret file %s is not a regular file: name a regular file of mode 0600", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != uid {
		return "", fmt.Errorf("secret file %s is not owned by this user: copy it to a file you own with mode 0600", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("secret file %s has mode %#o, readable beyond its owner: chmod 600 %s", path, perm, path)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("secret file %s: %w", path, err)
	}
	if len(data) > maxSecretFileBytes {
		return "", fmt.Errorf("secret file %s is larger than %d bytes: it must hold the value and nothing else", path, maxSecretFileBytes)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("secret file %s is empty: write the value to it", path)
	}
	return value, nil
}

// readSecretValue is readSecretFile for a file that holds one value (a
// route's key): it also refuses content with a control character, such as
// a second line, after trimming.
func readSecretValue(path string) (string, error) {
	value, err := readSecretFile(path)
	if err != nil {
		return "", err
	}
	if hasControlCharacter(value) {
		return "", fmt.Errorf("secret file %s holds a control character (a second line?): it must hold the value alone on one line", path)
	}
	return value, nil
}

// hasControlCharacter reports whether s holds a control character (a
// newline, a tab, NUL, DEL).
func hasControlCharacter(s string) bool {
	return strings.IndexFunc(s, unicode.IsControl) >= 0
}

// writeFileAtomic writes data to path with mode perm: to a new file in the
// same directory, renamed over path, so a reader never sees a part-written
// file. It refuses a path that is a symlink (it never writes through one) or
// exists and is not a regular file. path's directory must exist.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink: refusing to write through it; remove it or name another file", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a regular file", path)
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create a file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// writeSecretFile writes value (trimmed) to path (a leading ~/ is the home
// directory) with mode 0600, creating its directory with mode 0700. It
// never replaces a file already there: one that holds the same value
// (trimmed) is kept, and any other (another value, a symlink, a file that
// cannot be read as a secret file) is refused. The check is made when the
// file is put in place (a hard link of a fully written temporary file, which
// fails when path exists), not before, so two writers of one path cannot
// both succeed with different values. It refuses an empty value and one with
// a control character.
func writeSecretFile(path, value string) error {
	return putSecretFile(path, value, false)
}

// replaceSecretFile is writeSecretFile that replaces a regular file at path
// holding another value, through writeFileAtomic (never through a symlink).
// Only quickstart's replace path calls it, after the operator said yes or
// gave the key with -credential (quickstartMayReplaceKeyFile).
func replaceSecretFile(path, value string) error {
	return putSecretFile(path, value, true)
}

// putSecretFile is writeSecretFile (replace false) and replaceSecretFile
// (replace true).
func putSecretFile(path, value string, replace bool) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("secret file %s: the value is empty", path)
	}
	if hasControlCharacter(value) {
		return fmt.Errorf("secret file %s: the value holds a control character (a second line?); give the value alone", path)
	}
	resolved, err := expandAuthPath(path)
	if err != nil {
		return fmt.Errorf("secret file %s: resolve the home directory: %w", path, err)
	}
	dir := filepath.Dir(resolved)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data := []byte(value + "\n")
	if replace {
		if err := writeFileAtomic(resolved, data, 0o600); err != nil {
			return fmt.Errorf("secret file %w", err)
		}
		return nil
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(resolved)+".*")
	if err != nil {
		return fmt.Errorf("create a file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", tmp.Name(), err)
	}
	err = os.Link(tmp.Name(), resolved)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if info, lerr := os.Lstat(resolved); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("secret file %s is a symlink: refusing to write through it; remove it or name another file", path)
	}
	if existing, rerr := readSecretValue(path); rerr == nil && existing == value {
		return nil
	}
	return fmt.Errorf("secret file %s already holds another value: refusing to replace it; remove it or name another file", path)
}

// keyFileHoldsOther reports whether writing value to the key file at path
// would replace a different key: false when the file is absent or already
// holds value (both trimmed). A file there that cannot be read as a key
// file counts as holding another. A symlink at path is refused: nothing
// writes through one.
func keyFileHoldsOther(path, value string) (bool, error) {
	resolved, err := expandAuthPath(path)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is a symlink: refusing to write through it; remove it or name another file", path)
	}
	existing, err := readSecretValue(path)
	return err != nil || existing != strings.TrimSpace(value), nil
}

// routeCredentialFilePath is where setup, quickstart and the install's
// conversion of a retired route key write route's key:
// credentials/<profile>/<route> beside the session config at configPath,
// <profile> being the config file's name without its .yml or .yaml, so two
// profiles in one directory never share a key file. A character of either
// name that is not a letter, digit, '.', '_' or '-' becomes '-', and leading
// dots are dropped, so the file stays in that directory. The path is
// absolute.
func routeCredentialFilePath(configPath, route string) string {
	dir := filepath.Dir(configPath)
	if abs, err := filepath.Abs(dir); err == nil {
		// The config names the file by an absolute path: a worker runs in
		// its data dir, where a relative one would name another file.
		dir = abs
	}
	return filepath.Join(dir, "credentials", safeFileName(configProfileName(configPath), "config"), safeFileName(route, "route"))
}

// safeFileName is name with every character that is not a letter, digit,
// '.', '_' or '-' made '-' and leading dots dropped; fallback when that
// leaves nothing.
func safeFileName(name, fallback string) string {
	safe := strings.TrimLeft(strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, name), ".")
	if safe == "" {
		return fallback
	}
	return safe
}
