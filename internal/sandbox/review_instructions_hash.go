package sandbox

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// The SHA-256 of a snapshot: its staged tree and its mask list, every field
// length-prefixed.

// hashSnapshot hashes the snapshot's trees and the mask list (not what was
// removed from the worktree, which a second call no longer finds): every field is length-prefixed, so no two different snapshots share a
// byte stream. Files carry their executable bit, links their text.
func hashSnapshot(treeDir string, masks []WorkspaceMask) (string, error) {
	type entry struct {
		kind byte
		path string
		body []byte
	}
	var entries []entry
	err := filepath.WalkDir(treeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(treeDir, p)
		if err != nil || rel == "." {
			return err
		}
		e := entry{kind: 'd', path: filepath.ToSlash(rel)}
		switch {
		case d.IsDir():
		case d.Type()&fs.ModeSymlink != 0:
			text, err := os.Readlink(p)
			e.kind, e.body = 'l', []byte(text)
			if err != nil {
				return err
			}
		case d.Type().IsRegular():
			body, err := os.ReadFile(p)
			e.kind, e.body = 'f', body
			if info, ierr := d.Info(); ierr == nil && info.Mode()&0o100 != 0 {
				e.kind = 'x'
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("review instructions: %s is not a regular file", rel)
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	h := sha256.New()
	for _, e := range entries {
		writeHashEntry(h, e.kind, e.path, e.body)
	}
	sorted := append([]WorkspaceMask(nil), masks...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target < sorted[j].Target })
	for _, m := range sorted {
		flags := []byte{0, 0}
		if m.Dir {
			flags[0] = 1
		}
		if m.AbsentInWorktree {
			flags[1] = 1
		}
		writeHashEntry(h, 'm', m.Target, flags)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeHashEntry(h io.Writer, kind byte, path string, body []byte) {
	var n [8]byte
	h.Write([]byte{kind})
	binary.BigEndian.PutUint64(n[:], uint64(len(path)))
	h.Write(n[:])
	h.Write([]byte(path))
	binary.BigEndian.PutUint64(n[:], uint64(len(body)))
	h.Write(n[:])
	h.Write(body)
}
