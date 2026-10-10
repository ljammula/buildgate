package sandbox

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // a git blob id, compared for equality with the result commit's own
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
)

// Whether a worktree file is a given git blob: the blob hash of its bytes, or
// of its bytes with the carriage returns a checkout added removed.

func blobHasher(size int64) hash.Hash {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", size)
	return h
}

// crlfStrip passes its input on with the "\r" before every "\n" removed, and
// fails (bad) on a "\n" without one or when its output would hold a "\r\n"
// itself (the input had "\r\r\n"). A "\r" anywhere else passes unchanged.
type crlfStrip struct {
	w       io.Writer
	pending bool // a "\r" is held back
	last    byte // the last byte passed on
	bad     bool
}

func (c *crlfStrip) Write(p []byte) (int, error) {
	out := make([]byte, 0, len(p)+1)
	emit := func(b byte) { out, c.last = append(out, b), b }
	for _, b := range p {
		switch {
		case b == '\n' && !c.pending:
			c.bad = true
		case b == '\n':
			c.pending = false
			c.bad = c.bad || c.last == '\r'
			emit('\n')
		case b == '\r':
			if c.pending {
				emit('\r')
			}
			c.pending = true
		default:
			if c.pending {
				emit('\r')
				c.pending = false
			}
			emit(b)
		}
	}
	_, err := c.w.Write(out)
	return len(p), err
}

func (c *crlfStrip) flush() error {
	if !c.pending {
		return nil
	}
	c.pending = false
	_, err := c.w.Write([]byte{'\r'})
	return err
}

// fileHashesTo reports whether the size bytes of the file at abs are the blob
// oid, or are it once the "\r" before each "\n" is removed (a checkout with
// core.autocrlf or an eol attribute). The second form is accepted only when
// every "\n" of the file has its "\r" and the blob itself holds no "\r\n": all
// a build can add to such a file is that one carriage return per line, and a
// lone "\r" elsewhere must match byte for byte. It streams, with no size cap.
// The second form needs the blob size in the hash header, which only a first
// pass can count, so a file the first pass does not match and that holds a
// "\n" is read once more.
func fileHashesTo(abs string, size int64, oid string) bool {
	f, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer f.Close()
	raw := blobHasher(size)
	lines := &byteCounter{b: '\n'}
	if n, err := io.Copy(io.MultiWriter(raw, lines), io.LimitReader(f, size+1)); err != nil || n != size {
		return false
	}
	if hex.EncodeToString(raw.Sum(nil)) == oid {
		return true
	}
	if lines.n == 0 {
		return false
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return false
	}
	norm := blobHasher(size - lines.n)
	cs := &crlfStrip{w: norm}
	if n, err := io.Copy(cs, io.LimitReader(f, size+1)); err != nil || n != size || cs.flush() != nil || cs.bad {
		return false
	}
	return hex.EncodeToString(norm.Sum(nil)) == oid
}

// byteCounter counts the bytes equal to b written to it.
type byteCounter struct {
	b byte
	n int64
}

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n += int64(bytes.Count(p, []byte{c.b}))
	return len(p), nil
}
